package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	EpCreditDelivery = "/v1/ops/delivery"
	EpCreditClaim    = "/v1/ops/claim"
	EpCreditConfirm  = "/v1/ops/confirm"
	creditChannel    = "IDE"

	// CampaignTypeDailyLogin 每日签到活动。上游用 type 标注活动用途，
	// campaignId 只是当期投放 ID：2026-11-01 实测「每日签到领100积分」已是
	// campaignId=34，历史记录里的 1/4 属于被替换掉的旧投放，按 ID 选会一个都领不到。
	CampaignTypeDailyLogin = "USER_LOGIN"
	// CampaignTypeNewUser 新手注册积分（一次性）。
	CampaignTypeNewUser = "NEW_USER_REGISTER"

	// codeClaimRejected 上游拒绝本次领取的业务码（实测两种文案：「并发冲突，请重试」、
	// 「尚未到达权益刷新时间，暂时无法领取」）。活动按 UTC+8 自然日发放 ——
	// delivery 下发的活动窗口本身就是 00:00 CST 起止（活动文档同样以 UTC+8 计日），
	// 因此这个码的含义是「这一次的积分已经在今天领过了」，不是需要重试的故障。
	codeClaimRejected = 40001
)

// CreditCampaign 是活动投放接口的脱敏条目；不保留 access 中的活动票据。
type CreditCampaign struct {
	ID                 int     `json:"campaignId"`
	Type               string  `json:"type"`
	Title              string  `json:"title"`
	Status             string  `json:"status"`
	Claimable          bool    `json:"claimable"`
	BenefitUnit        string  `json:"benefitUnit"`
	BenefitAmount      float64 `json:"benefitAmount"`
	PendingTotalAmount float64 `json:"pendingTotalAmount"`
	Extra              struct {
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
	} `json:"extra"`
}

// ClaimOutcome 一个活动本次处理的结果类别。账号池据此决定本地状态：
// claimed / already / unavailable 属于「已了结」，当天（新手活动则永久）
// 不再向上游发请求；pending / failed 需要下次继续尝试。
type ClaimOutcome string

const (
	// ClaimOutcomeClaimed 本次领取并确认入账。
	ClaimOutcomeClaimed ClaimOutcome = "claimed"
	// ClaimOutcomeAlready 早前已经领到（状态为 CONFIRMED/CONSUMED），或上游以
	// codeClaimRejected 拒绝重复领取。
	ClaimOutcomeAlready ClaimOutcome = "already"
	// ClaimOutcomePending 上游已受理但还没确认，必须下次补确认。
	ClaimOutcomePending ClaimOutcome = "pending"
	// ClaimOutcomeUnavailable 上游没有下发这个活动，或账号没有资格、活动已结束/未开始。
	ClaimOutcomeUnavailable ClaimOutcome = "unavailable"
	// ClaimOutcomeFailed 本次尝试失败，可重试。
	ClaimOutcomeFailed ClaimOutcome = "failed"
)

// Settled 报告本次是否已经了结（不需要再向上游尝试）。
func (o ClaimOutcome) Settled() bool {
	switch o {
	case ClaimOutcomeClaimed, ClaimOutcomeAlready, ClaimOutcomeUnavailable:
		return true
	default:
		return false
	}
}

// CheckinOptions 选择本次要尝试领取的活动；零值表示只查询不领取。
type CheckinOptions struct {
	Daily   bool // 每日签到（USER_LOGIN）
	NewUser bool // 新手注册（NEW_USER_REGISTER）
}

type CreditClaimResult struct {
	CampaignID int          `json:"campaign_id"`
	Type       string       `json:"type,omitempty"`
	Title      string       `json:"title"`
	OK         bool         `json:"ok"`
	Status     string       `json:"status"`
	Outcome    ClaimOutcome `json:"outcome,omitempty"`
	Credits    float64      `json:"credits,omitempty"`
	Message    string       `json:"message"`
}

// CheckinResult 的 Credits 是本次领取量，不是账号余额。
// Daily / NewUser 是两个活动各自的结果类别，未尝试时为空。
type CheckinResult struct {
	OK        bool                `json:"ok"`
	Credits   float64             `json:"credits,omitempty"`
	Message   string              `json:"message"`
	Campaigns []CreditClaimResult `json:"campaigns"`
	Daily     ClaimOutcome        `json:"daily,omitempty"`
	NewUser   ClaimOutcome        `json:"new_user,omitempty"`
}

func (c *Client) creditRequest(ctx context.Context, method, path string, body any, cred SignCredential) (json.RawMessage, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.snapURL(path), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	signRequest(req, raw, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Path: req.URL.Path, Message: truncateStr(string(response), 300)}
	}
	var envelope struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, fmt.Errorf("parse credit activity response: %w", err)
	}
	if envelope.Code == nil {
		return nil, fmt.Errorf("credit activity response missing business code")
	}
	if *envelope.Code != 0 {
		return nil, &ApiError{Code: *envelope.Code, Status: http.StatusBadRequest, Path: req.URL.Path, Message: envelope.Message}
	}
	return envelope.Data, nil
}

// FetchCreditCampaigns 只读查询活动，端点位于 snap 根路径而非 snap-manager。
func (c *Client) FetchCreditCampaigns(ctx context.Context, cred SignCredential) ([]CreditCampaign, error) {
	raw, err := c.creditRequest(ctx, http.MethodGet, EpCreditDelivery+"?channel="+creditChannel, nil, cred)
	if err != nil {
		return nil, err
	}
	var data struct {
		Items *[]CreditCampaign `json:"items"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse credit campaigns: %w", err)
	}
	if data.Items == nil {
		return nil, fmt.Errorf("credit activity response missing data.items")
	}
	return *data.Items, nil
}

// Checkin 领取每日签到（USER_LOGIN）和新手注册（NEW_USER_REGISTER）积分。
// 活动用途按 type 判定，不按 campaignId：ID 会随投放批次替换，写死的 ID 过期后
// 会静默「一个活动都没选中」。邀请、学生认证需要账号本人操作，不代领。
// 上游决定资格和每日刷新，已领取不再 claim；CLAIMED 只补 confirm。
// opts 用来跳过本地已经了结的活动 —— 调用方（账号池）负责「每天一次 / 只领一次」，
// 这里只管「不要重复领取」。调用方还应串行化同一账号的领取。
func (c *Client) Checkin(ctx context.Context, cred SignCredential, opts CheckinOptions) (CheckinResult, error) {
	campaigns, err := c.FetchCreditCampaigns(ctx, cred)
	if err != nil {
		return CheckinResult{Message: err.Error()}, err
	}
	out := CheckinResult{OK: true, Campaigns: make([]CreditClaimResult, 0)}
	for _, campaign := range campaigns {
		if !wantedCampaign(campaign.Type, opts) {
			continue
		}
		res := c.claimCreditCampaign(ctx, cred, campaign, time.Now())
		out.Campaigns = append(out.Campaigns, res)
		out.OK = out.OK && res.OK
		out.Credits += res.Credits
		switch campaign.Type {
		case CampaignTypeDailyLogin:
			out.Daily = res.Outcome
		case CampaignTypeNewUser:
			out.NewUser = res.Outcome
		}
	}
	// 上游根本没下发这个活动：对本地来说就是「没有可领的」，不必每天再问一次。
	if opts.Daily && out.Daily == "" {
		out.Daily = ClaimOutcomeUnavailable
	}
	if opts.NewUser && out.NewUser == "" {
		out.NewUser = ClaimOutcomeUnavailable
	}
	parts := make([]string, 0, len(out.Campaigns))
	for _, res := range out.Campaigns {
		parts = append(parts, res.Title+": "+res.Message)
	}
	out.Message = strings.Join(parts, "; ")
	if len(parts) == 0 {
		out.Message = "活动列表未下发本次要领取的每日签到或新手活动"
	}
	return out, nil
}

// wantedCampaign 报告该活动类型本次是否要尝试。
func wantedCampaign(campaignType string, opts CheckinOptions) bool {
	switch campaignType {
	case CampaignTypeDailyLogin:
		return opts.Daily
	case CampaignTypeNewUser:
		return opts.NewUser
	default:
		return false
	}
}

func (c *Client) claimCreditCampaign(ctx context.Context, cred SignCredential, campaign CreditCampaign, now time.Time) CreditClaimResult {
	res := CreditClaimResult{CampaignID: campaign.ID, Type: campaign.Type, Title: campaign.Title, Status: strings.TrimSpace(campaign.Status), OK: true}
	if res.Title == "" {
		res.Title = fmt.Sprintf("积分活动 %d", campaign.ID)
	}
	if end, err := time.Parse(time.RFC3339Nano, campaign.Extra.EndTime); err == nil && !now.Before(end) {
		res.Message, res.Outcome = "活动已结束", ClaimOutcomeUnavailable
		return res
	}
	if start, err := time.Parse(time.RFC3339Nano, campaign.Extra.StartTime); err == nil && now.Before(start) {
		res.Message, res.Outcome = "活动尚未开始", ClaimOutcomeUnavailable
		return res
	}
	switch res.Status {
	case "CONFIRMED", "CONSUMED":
		res.Message, res.Outcome = "已领取", ClaimOutcomeAlready
		return res
	case "CLAIMED":
		// 已到账但尚未确认；不得重复领取。
		res.Outcome = ClaimOutcomePending
	case "ELIGIBLE":
		if !campaign.Claimable {
			res.OK, res.Message, res.Outcome = false, "暂不可领取，稍后重试", ClaimOutcomeFailed
			return res
		}
		_, err := c.creditRequest(ctx, http.MethodPost, EpCreditClaim, map[string]any{
			"campaignId": campaign.ID, "channel": creditChannel,
			"idempotentKey": fmt.Sprintf("claim_%d_%d", campaign.ID, now.UnixMilli()),
		}, cred)
		if err != nil {
			// 40001 = 今天已经领过了（见 codeClaimRejected）。判成失败会让「重复点击/
			// 另一个客户端先领了」看起来像故障，而且本地不会了结，下一轮又打一次上游。
			var apiErr *ApiError
			if errors.As(err, &apiErr) && apiErr.Code == codeClaimRejected {
				res.OK, res.Message, res.Outcome = true, "今日已领（上游拒绝重复领取："+apiErr.Message+"）", ClaimOutcomeAlready
				return res
			}
			res.OK, res.Message, res.Outcome = false, err.Error(), ClaimOutcomeFailed
			return res
		}
		res.Status = "CLAIMED"
		res.Outcome = ClaimOutcomePending
		res.Credits = campaign.PendingTotalAmount
		if res.Credits <= 0 {
			res.Credits = campaign.BenefitAmount
		}
	case "":
		res.Message, res.Outcome = "未获得活动资格", ClaimOutcomeUnavailable
		return res
	default:
		res.OK, res.Message, res.Outcome = false, "未知活动状态: "+res.Status, ClaimOutcomeFailed
		return res
	}
	if _, err := c.creditRequest(ctx, http.MethodPost, EpCreditConfirm, map[string]any{"campaignId": campaign.ID}, cred); err != nil {
		// 已受理未确认：算 pending 而不是 failed —— 下次要补确认，不能当今日完成。
		res.OK, res.Message, res.Outcome = false, "已领取，确认失败，稍后重试: "+err.Error(), ClaimOutcomePending
		return res
	}
	res.Status = "CONFIRMED"
	res.Message, res.Outcome = "已补确认", ClaimOutcomeClaimed
	if res.Credits > 0 {
		res.Message = fmt.Sprintf("领取成功（%g 积分）", res.Credits)
	}
	return res
}
