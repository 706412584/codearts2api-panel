package pool

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

type checkinClientFunc func(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error)

func (f checkinClientFunc) Checkin(ctx context.Context, cred upstream.SignCredential, opts upstream.CheckinOptions) (upstream.CheckinResult, error) {
	return f(ctx, cred, opts)
}

func TestCheckinSerializesAndKeepsInferenceHealthy(t *testing.T) {
	var calls, active, peak atomic.Int32
	client := checkinClientFunc(func(ctx context.Context, cred upstream.SignCredential, opts upstream.CheckinOptions) (upstream.CheckinResult, error) {
		calls.Add(1)
		n := active.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		defer active.Add(-1)
		if cred.SecurityToken != "token" || cred.AccessKeyID != "ak" {
			t.Error("wrong credential snapshot")
		}
		time.Sleep(10 * time.Millisecond)
		return upstream.CheckinResult{}, errors.New("activity unavailable")
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{ErrThreshold: 1, CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.Checkin(context.Background(), "u1") }()
	}
	wg.Wait()
	if calls.Load() != 3 || peak.Load() != 1 {
		t.Fatalf("calls=%d peak=%d", calls.Load(), peak.Load())
	}
	st := p.List()[0]
	if !p.Healthy("u1") || st["err_count"] != 0 || st["active_concurrent"] != 0 {
		t.Fatalf("activity error damaged inference: %+v", st)
	}
	if st["checkin"].(*CheckinStatus).OK {
		t.Fatal("failed checkin reported as success")
	}
	// 传输/业务失败不是「已了结」：本地不能因此把今天记为已签。
	if st["checkin_done"] != false {
		t.Fatalf("failed checkin must not mark today as done: %+v", st)
	}
	p.Disable("u1", "manual")
	if _, err := p.Checkin(context.Background(), "u1"); err == nil || calls.Load() != 3 {
		t.Fatal("disabled account was claimed")
	}
}

// 今日已签 + 新手已了结 ⇒ 完全不请求上游。
func TestCheckinSkipsUpstreamOnceSettled(t *testing.T) {
	var calls atomic.Int32
	client := checkinClientFunc(func(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
		calls.Add(1)
		return upstream.CheckinResult{
			OK: true, Credits: 100, Message: "领取成功（100 积分）",
			Daily: upstream.ClaimOutcomeClaimed, NewUser: upstream.ClaimOutcomeAlready,
		}, nil
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("first checkin must hit upstream once, calls=%d", calls.Load())
	}
	st := p.List()[0]
	if st["checkin_done"] != true || st["new_user_settled"] != true {
		t.Fatalf("settled flags missing: %+v", st)
	}
	for i := 0; i < 3; i++ {
		res, err := p.Checkin(context.Background(), "u1")
		if err != nil || !res.OK {
			t.Fatalf("cached checkin failed: result=%+v err=%v", res, err)
		}
		if res.Message != "领取成功（100 积分）（今日已签到，不再请求上游）" {
			t.Fatalf("unexpected cached message: %q", res.Message)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("settled account still hit upstream: calls=%d", calls.Load())
	}
}

// 签到未了结（如确认失败 pending）时必须继续尝试，不能被本地标记锁死。
func TestCheckinRetriesUntilSettled(t *testing.T) {
	var calls atomic.Int32
	outcomes := []upstream.CheckinResult{
		{OK: false, Message: "已领取，确认失败，稍后重试", Daily: upstream.ClaimOutcomePending, NewUser: upstream.ClaimOutcomeUnavailable},
		{OK: true, Message: "已补确认", Daily: upstream.ClaimOutcomeClaimed, NewUser: upstream.ClaimOutcomeUnavailable},
	}
	client := checkinClientFunc(func(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
		i := calls.Add(1) - 1
		if int(i) < len(outcomes) {
			return outcomes[i], nil
		}
		return upstream.CheckinResult{OK: true}, nil
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if p.List()[0]["checkin_done"] != false {
		t.Fatal("pending confirm must not mark today as done")
	}
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || p.List()[0]["checkin_done"] != true {
		t.Fatalf("pending checkin was not retried: calls=%d st=%+v", calls.Load(), p.List()[0])
	}
	// 新手活动已 unavailable，签到了结后整轮都不再请求上游。
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("settled account still hit upstream: calls=%d", calls.Load())
	}
}

// 「今天上游没下发签到活动」也要停止请求，但不能显示成「已签」——两者含义不同。
func TestCheckinUnavailableIsSettledButNotSigned(t *testing.T) {
	var calls atomic.Int32
	client := checkinClientFunc(func(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
		calls.Add(1)
		return upstream.CheckinResult{
			OK: true, Message: "活动列表未下发本次要领取的每日签到或新手活动",
			Daily: upstream.ClaimOutcomeUnavailable, NewUser: upstream.ClaimOutcomeUnavailable,
		}, nil
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	st := p.List()[0]
	if st["checkin_done"] != false {
		t.Fatalf("no activity must not look signed: %+v", st)
	}
	res, err := p.Checkin(context.Background(), "u1")
	if err != nil || !res.OK {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if res.Message != "今日没有可领的签到活动，不再请求上游" {
		t.Fatalf("unexpected cached message: %q", res.Message)
	}
	if calls.Load() != 1 {
		t.Fatalf("settled-but-unsigned account still hit upstream: calls=%d", calls.Load())
	}
}

// 重启后仍要知道「今天已签」，否则每次重启都会重复打一次上游。
func TestCheckinSettledSurvivesReload(t *testing.T) {
	stateFile := t.TempDir() + "/state.json"
	client := checkinClientFunc(func(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
		return upstream.CheckinResult{OK: true, Daily: upstream.ClaimOutcomeClaimed, NewUser: upstream.ClaimOutcomeAlready}, nil
	})
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := New([]*auth.Auth{a}, Config{CheckinClient: client}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"last_checkin_day"`) || !strings.Contains(string(raw), `"new_user_settled"`) || !strings.Contains(string(raw), `"checkin_claimed"`) {
		t.Fatalf("state file missing checkin ledger:\n%s", raw)
	}
	// 旧账本只有日期、没有 checkin_claimed：按「已签」兼容，别在升级当天把已签显示成未签。
	legacy := t.TempDir() + "/state.json"
	day := time.Now().Format("2006-01-02")
	if err := os.WriteFile(legacy, []byte(`[{"name":"u1","last_checkin_day":"`+day+`","new_user_settled":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyAuth := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	pLegacy, err := New([]*auth.Auth{legacyAuth}, Config{CheckinClient: checkinClientFunc(func(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
		t.Error("legacy ledger must suppress the upstream call")
		return upstream.CheckinResult{}, nil
	})}, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if pLegacy.List()[0]["checkin_done"] != true {
		t.Fatalf("legacy ledger must still read as signed: %+v", pLegacy.List()[0])
	}
	if _, err := pLegacy.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	reloaded := checkinClientFunc(func(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
		calls.Add(1)
		return upstream.CheckinResult{OK: true}, nil
	})
	a2 := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p2, err := New([]*auth.Auth{a2}, Config{CheckinClient: reloaded}, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if p2.List()[0]["checkin_done"] != true {
		t.Fatalf("checkin ledger not restored: %+v", p2.List()[0])
	}
	if _, err := p2.Checkin(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("restarted panel re-hit upstream: calls=%d", calls.Load())
	}
}
