package server

import (
	"strings"
	"sync"
	"time"
)

// 模型可用性学习。
//
// 背景（2026-09-15 实测）：上游 /v1/models 的发现结果 ≠ 账号真能调用的模型。
// agent-center 与 /v1/model/builtin 会列出未在当前账号注册的模型（实测
// GLM-5.2-ArkTS-SPARK / OpenPangu-2.0-Pro / OpenPangu-2.0-Flash 全部返回
// InferHub.002002009.404）。
//
// 这里**不主动探测**：真实 chat 流量本身就会写结论（成功 markUsable / 失败
// markUnusable），空闲时不再额外打上游。因此某模型在被真实调用一次之前可能是
// 「未知」——此时 /v1/models 照列、聊天照放行，交给上游作答；一旦真实请求报
// not registered 即标记不可用并从列表下架。代价是「从未被调用过的僵尸模型」会
// 短暂出现在列表里，收益是网关在空闲时对上游零额外请求。

const (
	// availabilityTTL 结论有效期。过了就重新视为未知、交给真实流量再判定，
	// 避免套餐变化后一直按旧结论办事。
	availabilityTTL = 30 * time.Minute
)

type availState int

const (
	availUnknown availState = iota
	availUsable
	availUnusable
)

type availEntry struct {
	state  availState
	reason string
	at     time.Time
}

var availability = struct {
	sync.Mutex
	byKey map[string]availEntry // accountID|modelLower -> 状态
}{byKey: map[string]availEntry{}}

func availKey(accountID, model string) string {
	return accountID + "|" + strings.ToLower(model)
}

// modelAvailability 返回缓存的可用性；过期视为未知。
func modelAvailability(accountID, model string) (availState, string) {
	availability.Lock()
	defer availability.Unlock()
	e, ok := availability.byKey[availKey(accountID, model)]
	if !ok || time.Since(e.at) > availabilityTTL {
		return availUnknown, ""
	}
	return e.state, e.reason
}

// markUnusable 记录某账号下某模型不可用（来自真实请求失败）。
func markUnusable(accountID, model, reason string) {
	if accountID == "" || model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUnusable, reason: reason, at: time.Now()}
}

// markUsable 记录某账号下某模型可用。
func markUsable(accountID, model string) {
	if accountID == "" || model == "" {
		return
	}
	availability.Lock()
	defer availability.Unlock()
	availability.byKey[availKey(accountID, model)] = availEntry{state: availUsable, at: time.Now()}
}

// errClass 是上游错误的处置类别。判定只看错误文本（ApiError 与流内
// UpstreamError 的字符串都带业务码），这样流式/非流式两条路径共用一套规则。
type errClass int

const (
	// classAccountFault 账号侧故障（5xx、网络、鉴权异常）：冷却账号并轮转。
	classAccountFault errClass = iota
	// classModelUnusable 该模型对此账号不可用（未注册）：记模型不可用、不冷却、不轮转。
	classModelUnusable
	// classBudget 请求本身超出模型能力（prompt 过长）：不冷却、不轮转、
	// 换模型也没用，直接给客户端 400。实测 ModelArts.81001：同一个超长请求
	// 打 1M 上下文的 deepseek 成功、打 131k 的 qwen3-vl 报 81001——
	// 是「这个模型装不下」，不是账号坏了。
	classBudget
	// classBadRequest 客户端请求形状问题（参数无效、多模态不支持）：不冷却、不轮转。
	classBadRequest
)

// errClassText 按错误文本分类。三处判定必须放在同一个地方，
// 否则同一错误在流式与非流式路径会得到不同处置（曾因此出现
// 「流式不冷却、非流式冷却」的割裂）。
func errClassText(s string) errClass {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "002002009") || strings.Contains(low, "not registered"):
		return classModelUnusable
	case strings.Contains(low, "81001") || strings.Contains(low, "prompt") && strings.Contains(low, "exceed"):
		return classBudget
	case strings.Contains(low, "not multimodal") ||
		strings.Contains(low, "001001020") ||
		strings.Contains(low, "001001046") || // 多模态内容格式不正确
		strings.Contains(low, "001001005") ||
		strings.Contains(low, "request param is invalid") ||
		// 81029「Media conversion failed: ... 413」= 图片超出上游媒体转换上限，
		// 同属请求内容问题：上游常把它包成 502，落进「账号故障」会误冷却整池。
		strings.Contains(low, "81029") ||
		strings.Contains(low, "media conversion failed"):
		return classBadRequest
	}
	return classAccountFault
}

// classifyUpstreamError 归类上游错误。nil 视为账号故障便于调用方少写分支，
// 调用方应先用 err==nil 短路。
func classifyUpstreamError(err error) errClass {
	if err == nil {
		return classAccountFault
	}
	return errClassText(err.Error())
}

// upstreamUnavailableReason 在错误属于「该模型对这个账号不可用」时返回说明，否则空串。
func upstreamUnavailableReason(err error) string {
	if err == nil {
		return ""
	}
	return upstreamUnavailableReasonText(err.Error())
}

// upstreamUnavailableReasonText 同 upstreamUnavailableReason，作用于响应正文。
func upstreamUnavailableReasonText(s string) string {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "002002009") || strings.Contains(low, "not registered"):
		return "模型未在当前账号注册"
	}
	return ""
}
