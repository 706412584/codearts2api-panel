// 模型目录。
//
// 上游可用模型来自两路（对齐官方客户端 ModelService）：
//  1. agent-center：useragents 找 CodeAgent（alias 智能体 + show_in_ide）→ detail 的 gpts.models
//  2. 内置：GET /v1/model/builtin（Agent-Type: PromptCenter）
//
// 福利目录 /api/v1/gateway/config 已下架，不再请求。
//
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"codearts2api-panel/internal/auth"
)

// CatalogTTL 账号模型目录的有效期（超过则建议重新发现）。
const CatalogTTL = time.Hour

// ModelInfo 模型信息（与 workbuddy/trae 一致）。
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxTokens     int64  `json:"maxTokens,omitempty"`
	RatioDisplay  string `json:"ratio_display,omitempty"`
	Desc          string `json:"desc,omitempty"`
	// Notice 上游的模型级提示（如闲时折扣说明），面板作为 tooltip 第二行展示。
	Notice string `json:"notice,omitempty"`
}

// 内置模型种子（大小写归一用）。
var seedKnownModels = []string{
	"GLM-5.2", "GLM-5.2-ArkTS-SPARK", "GLM-5.1", "GLM-4.7",
	"OpenPangu-2.0-Pro", "OpenPangu-2.0-Flash",
	"Qwen3-VL-235B", "Qwen3.5-397B-A17B-VL", "Qwen3.6-27B-VL",
}

// accountCatalog 一个账号的模型目录快照。
type accountCatalog struct {
	infos   []ModelInfo
	fetched time.Time
}

var catalog = struct {
	sync.RWMutex
	accounts map[string]*accountCatalog // accountID -> 目录
	known    map[string]string          // lower(id) -> 精确 ID（跨账号，大小写归一）
}{accounts: map[string]*accountCatalog{}, known: map[string]string{}}

func init() {
	catalog.known = make(map[string]string, len(seedKnownModels))
	for _, id := range seedKnownModels {
		if _, ok := catalog.known[strings.ToLower(id)]; !ok {
			catalog.known[strings.ToLower(id)] = id
		}
	}
}

// SetAccountModels 整体替换某账号的模型目录（不是增补：套餐轮换后旧模型不得残留）。
func SetAccountModels(accountID string, infos []ModelInfo) {
	if accountID == "" {
		return
	}
	ac := &accountCatalog{
		infos:   append([]ModelInfo(nil), infos...),
		fetched: time.Now(),
	}
	catalog.Lock()
	defer catalog.Unlock()
	if catalog.known == nil {
		catalog.known = map[string]string{}
	}
	for _, mi := range ac.infos {
		if mi.ID == "" {
			continue
		}
		key := strings.ToLower(mi.ID)
		if _, ok := catalog.known[key]; !ok || mi.ID != key {
			catalog.known[key] = mi.ID
		}
	}
	catalog.accounts[accountID] = ac
}

// AccountModels 返回某账号的模型目录快照（未发现时 ok=false）。
func AccountModels(accountID string) ([]ModelInfo, bool) {
	catalog.RLock()
	defer catalog.RUnlock()
	ac, ok := catalog.accounts[accountID]
	if !ok {
		return nil, false
	}
	return append([]ModelInfo(nil), ac.infos...), true
}

// AccountCatalogStale 报告某账号目录是否需重新发现（未发现或超过 CatalogTTL）。
func AccountCatalogStale(accountID string) bool {
	catalog.RLock()
	defer catalog.RUnlock()
	ac, ok := catalog.accounts[accountID]
	return !ok || time.Since(ac.fetched) >= CatalogTTL
}

// lookupKnownModel 按小写 ID 查精确模型 ID（种子 + 已发现模型）。
func lookupKnownModel(id string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(id))
	if key == "" {
		return "", false
	}
	catalog.RLock()
	defer catalog.RUnlock()
	exact, ok := catalog.known[key]
	return exact, ok
}

// MergeModels 合并多路模型列表：按小写 ID 去重，参数更全的优先。
// 结果按小写 ID 排序，保证 /v1/models 输出稳定（缓存刷新前后顺序一致）。
func MergeModels(sets ...[]ModelInfo) []ModelInfo {
	merged := map[string]ModelInfo{}
	for _, set := range sets {
		for _, mi := range set {
			if mi.ID == "" {
				continue
			}
			key := strings.ToLower(mi.ID)
			cur, ok := merged[key]
			if !ok {
				merged[key] = mi
				continue
			}
			merged[key] = richerModel(cur, mi)
		}
	}
	out := make([]ModelInfo, 0, len(merged))
	for _, mi := range merged {
		out = append(out, mi)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].ID), strings.ToLower(out[j].ID)
		if li != lj {
			return li < lj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// richerModel 合并同一模型的两条记录，保留更完整的参数。
func richerModel(cur, next ModelInfo) ModelInfo {
	keep, drop := cur, next
	if betterModel(next, cur) {
		keep, drop = next, cur
	}
	if keep.Name == "" {
		keep.Name = drop.Name
	}
	if keep.ContextWindow == 0 {
		keep.ContextWindow = drop.ContextWindow
	}
	if keep.MaxTokens == 0 {
		keep.MaxTokens = drop.MaxTokens
	}
	if keep.RatioDisplay == "" {
		keep.RatioDisplay = drop.RatioDisplay
	}
	if keep.Desc == "" {
		keep.Desc = drop.Desc
	}
	if keep.Notice == "" {
		keep.Notice = drop.Notice
	}
	return keep
}

// betterModel 报告 a 是否比 b 更值得保留：优先带参数，其次保留精确大小写。
func betterModel(a, b ModelInfo) bool {
	if (a.ContextWindow > 0) != (b.ContextWindow > 0) {
		return a.ContextWindow > 0
	}
	return a.ID != strings.ToLower(a.ID) && b.ID == strings.ToLower(b.ID)
}

// FetchModels 拉取指定账号当前可用模型（Agent 与内置两路合并），并写入该账号的模型目录。
// 任一路失败只记日志不阻断另一路。
func (c *Client) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	token, ak, sk := acct.Credentials()
	cred := SignCredential{AccessKeyID: ak, SecretAccessKey: sk, SecurityToken: token}
	accountID := acct.UserID
	var sets [][]ModelInfo
	if infos, err := c.fetchAgentModels(cred); err != nil {
		Log("agent models: %v", err)
	} else {
		sets = append(sets, infos)
	}
	if infos, err := c.fetchBuiltinModels(accountID, cred); err != nil {
		Log("builtin models: %v", err)
	} else {
		sets = append(sets, infos)
	}
	out := MergeModels(sets...)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	SetAccountModels(accountID, out)
	return out, nil
}

// fetchAgentModels 官方 fetchAgentModels 等价实现：agent-center detail 的 gpts.models。
func (c *Client) fetchAgentModels(cred SignCredential) ([]ModelInfo, error) {
	agentID, err := c.defaultAgentID(cred)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, fmt.Errorf("no default agent found")
	}
	raw, err := c.getSigned(context.Background(), c.snapURL(EpAgentDetail)+"?agent_id="+url.QueryEscape(agentID), cred, true)
	if err != nil {
		return nil, err
	}
	var detail struct {
		Gpts struct {
			Models []struct {
				ModelAlias string `json:"model_alias"`
				ModelName  string `json:"model_name"`
				ModelID    string `json:"model_id"`
				// Notice 与 model_parameters 同级：空字符串表示无提示。
				Notice string `json:"notice"`
				Params struct {
					ContextWindow int64  `json:"context_window"`
					MaxTokens     int64  `json:"max_tokens"`
					ModelID       string `json:"model_id"`
					ModelDesc     string `json:"model_desc"`
				} `json:"model_parameters"`
			} `json:"models"`
		} `json:"gpts"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]ModelInfo, 0, len(detail.Gpts.Models))
	for _, m := range detail.Gpts.Models {
		id := firstNonEmpty(m.ModelID, m.ModelAlias, m.Params.ModelID)
		if id == "" {
			// 只剩展示名可当 ID：上游 ID 不写空格（GLM 5.2 → GLM-5.2），带空格直接拿来调用是 404。
			id = strings.ReplaceAll(strings.TrimSpace(m.ModelName), " ", "-")
		}
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{
			ID:            id,
			Name:          firstNonEmpty(m.ModelName, id),
			ContextWindow: m.Params.ContextWindow,
			MaxTokens:     m.Params.MaxTokens,
			Desc:          m.Params.ModelDesc,
			Notice:        strings.TrimSpace(m.Notice),
		})
	}
	return out, nil
}

// defaultAgentID 拉取用户 agent 列表，返回 CodeAgent 的 agent_id。
// 过滤条件与官方客户端一致：agent_name=="CodeAgent" && alias.alias_zh_cn=="智能体" && show_in_ide，
// 退化为 is_primary_agent，再退化为首个。
func (c *Client) defaultAgentID(cred SignCredential) (string, error) {
	raw, err := c.getSigned(context.Background(), c.snapURL(EpAgentList)+"?offset=0&limit=100&is_primary_agent=true", cred, true)
	if err != nil {
		return "", err
	}
	var out struct {
		Agents []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
			Primary   bool   `json:"is_primary_agent"`
			ShowInIDE bool   `json:"show_in_ide"`
			Alias     struct {
				ZhCN string `json:"alias_zh_cn"`
			} `json:"alias"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("parse agents: %w", err)
	}
	for _, a := range out.Agents {
		if a.AgentName == "CodeAgent" && a.Alias.ZhCN == "智能体" && a.ShowInIDE {
			return a.AgentID, nil
		}
	}
	for _, a := range out.Agents {
		if a.Primary {
			return a.AgentID, nil
		}
	}
	if len(out.Agents) > 0 {
		return out.Agents[0].AgentID, nil
	}
	return "", nil
}

// fetchBuiltinModels GET /v1/model/builtin（Agent-Type: PromptCenter）。
func (c *Client) fetchBuiltinModels(accountID string, cred SignCredential) ([]ModelInfo, error) {
	key := accountID + "|builtin"
	if c.srcBlocked(key) {
		return nil, fmt.Errorf("builtin source backoff")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.snapURL(EpModelBuiltin), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
	}
	signRequest(req, []byte{}, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		c.noteSrcFail(key)
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		c.noteSrcFail(key)
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(raw), 300), Path: EpModelBuiltin}
	}
	var out struct {
		BuiltinModels []struct {
			ModelID       string `json:"model_id"`
			ModelName     string `json:"model_name"`
			ModelDesc     string `json:"model_desc"`
			ContextWindow int64  `json:"context_window"`
			MaxTokens     int64  `json:"max_tokens"`
			Credit        []struct {
				RatioDisplay string `json:"ratio_display"`
			} `json:"credit"`
		} `json:"builtinModels"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		c.noteSrcFail(key)
		return nil, fmt.Errorf("parse builtin models: %w", err)
	}
	c.clearSrcFail(key)
	infos := make([]ModelInfo, 0, len(out.BuiltinModels))
	for _, m := range out.BuiltinModels {
		id := firstNonEmpty(m.ModelID, m.ModelName)
		if id == "" {
			continue
		}
		mi := ModelInfo{ID: id, Name: firstNonEmpty(m.ModelName, id), ContextWindow: m.ContextWindow, MaxTokens: m.MaxTokens, Desc: m.ModelDesc}
		if len(m.Credit) > 0 {
			mi.RatioDisplay = m.Credit[0].RatioDisplay
		}
		infos = append(infos, mi)
	}
	return infos, nil
}
