package pool

import (
	"context"
	"fmt"
	"time"

	"codearts2api-panel/internal/upstream"
)

// CheckinClient 允许注入独立的活动客户端；默认使用各账号的上游客户端。
type CheckinClient interface {
	Checkin(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error)
}

type CheckinStatus struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
}

// checkinDay 返回本地日期键（"2006-01-02"）。签到按自然日重置，跨零点自动失效。
func checkinDay(now time.Time) string { return now.Format("2006-01-02") }

// Checkin 串行领取指定账号的每日签到和新手注册积分。
//
// 「每天只领一次、新手只领一次、不重复打上游」由本地状态兜住：
//   - lastCheckinDay == 今天：今日签到已了结，不再请求上游；
//   - newUserSettled：新手活动已了结（领到 / 上游已确认 / 没有该活动），永久不再请求；
//   - 两者都了结时直接回放上次结果，零上游请求。
//
// 只有上游真的给了答复（err == nil）才落「了结」标记：传输失败、鉴权失败、
// 已受理待确认（pending）都会保留未了结状态，下一轮继续补确认/重试。
// 活动故障只记录签到结果，不计入推理错误、冷却或禁用；token 仍由原有
// Validate/RefreshToken 处理。
func (p *Pool) Checkin(ctx context.Context, name string) (upstream.CheckinResult, error) {
	a := p.Get(name)
	if a == nil {
		return upstream.CheckinResult{}, fmt.Errorf("account not found")
	}
	a.checkinMu.Lock()
	defer a.checkinMu.Unlock()
	if err := ctx.Err(); err != nil {
		return upstream.CheckinResult{}, err
	}
	today := checkinDay(time.Now())
	a.mu.Lock()
	disabled := a.disabled
	opts := upstream.CheckinOptions{Daily: a.lastCheckinDay != today, NewUser: !a.newUserSettled}
	cached, claimedToday := a.checkin, a.checkinClaimed
	a.mu.Unlock()
	if disabled {
		return upstream.CheckinResult{}, fmt.Errorf("account disabled")
	}
	if !opts.Daily && !opts.NewUser {
		msg := "今日没有可领的签到活动，不再请求上游"
		if claimedToday {
			msg = "今日已签到，不再请求上游"
			if cached != nil && cached.OK && cached.Message != "" {
				msg = cached.Message + "（今日已签到，不再请求上游）"
			}
		}
		return upstream.CheckinResult{OK: true, Message: msg}, nil
	}
	ok, err := p.Validate(a)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("token invalid; re-login required")
		}
		return upstream.CheckinResult{}, err
	}
	token, ak, sk := a.Auth.Credentials()
	client := p.cfg.CheckinClient
	if client == nil {
		client = a.Client
	}
	result, err := client.Checkin(ctx, upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk}, opts)
	if err != nil {
		result.OK, result.Message = false, err.Error()
	}
	settled := false
	if err == nil {
		a.mu.Lock()
		if result.Daily.Settled() && a.lastCheckinDay != today {
			a.lastCheckinDay = today
			// 只有真的领到/已领才算「已签」；「上游没下发签到活动」只是今天不用问了。
			a.checkinClaimed = result.Daily == upstream.ClaimOutcomeClaimed || result.Daily == upstream.ClaimOutcomeAlready
			settled = true
		}
		if result.NewUser.Settled() && !a.newUserSettled {
			a.newUserSettled = true
			settled = true
		}
		a.mu.Unlock()
	}
	if settled {
		p.saveState()
	}
	a.mu.Lock()
	a.checkin = &CheckinStatus{OK: result.OK, Message: result.Message, CheckedAt: time.Now()}
	a.mu.Unlock()
	return result, err
}
