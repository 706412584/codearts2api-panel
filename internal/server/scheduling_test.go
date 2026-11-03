package server

import (
	"net/http"
	"net/http/httptest"

	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// slotTestHandler 组一个 N 账号 handler，上游照常回答，返回池与调用计数。
func slotTestHandler(t *testing.T, uids []string, maxConcurrent int) (*pool.Pool, *int32) {
	t.Helper()
	var calls int32
	up := httptestServer(t, &calls)
	t.Cleanup(up.Close)

	auths := make([]*auth.Auth, 0, len(uids))
	for _, uid := range uids {
		a := auth.New(uid, "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
		auths = append(auths, a)
		upstream.SetAccountModels(uid, []upstream.ModelInfo{{ID: "GLM-5.2"}})
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: maxConcurrent}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	}
	return p, &calls
}

func httptestServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
}

// 单账号池：唯一账号的槽位被占住时，新请求要能等到槽位释放后成功，
// 而不是被自己的排除集排掉直接 503。
//
// 这是「排除集误用 tried 把当前账号自己排除」那个高危缺陷的回归测试：
// 修复前该用例必然 503（无候选立即失败）。
func TestSingleAccountWaitsForItsOnlySlotInsteadOfFailing(t *testing.T) {
	prev := slotWaitTimeout
	slotWaitTimeout = 2 * time.Second
	t.Cleanup(func() { slotWaitTimeout = prev })

	p, calls := slotTestHandler(t, []string{"solo"}, 1)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1})

	// 占住唯一账号的槽位。
	if !p.AcquireLock("solo") {
		t.Fatal("could not occupy the only account")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		p.ReleaseLock("solo")
	}()

	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("GLM-5.2"), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (should wait for the only slot): %s", w.Code, w.Body.String())
	}
	if atomic.LoadInt32(calls) == 0 {
		t.Fatal("request never reached upstream after slot was released")
	}
}

// 槽位始终不释放且超过等待窗时，收尾应是 429 all_accounts_busy（容量问题、
// 可重试），而不是 503「账号不可用」——账号其实是健康的。
func TestAllSaturatedReturns429AllAccountsBusy(t *testing.T) {
	// 注入短等待窗：测的是「等满窗口后的收尾语义」，不必真等默认的 15s。
	prev := slotWaitTimeout
	slotWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { slotWaitTimeout = prev })

	p, _ := slotTestHandler(t, []string{"busy"}, 1)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1})

	if !p.AcquireLock("busy") {
		t.Fatal("could not occupy the only account")
	}
	defer p.ReleaseLock("busy")

	start := time.Now()
	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("GLM-5.2"), "k")
	elapsed := time.Since(start)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !contains(body, "all_accounts_busy") {
		t.Fatalf("429 body should name all_accounts_busy: %s", body)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("waited %s; should give up within the slot wait window", elapsed)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatal("429 should carry Retry-After so clients know to retry")
	}
}

// 多账号池：一个账号满载时新请求要落到空闲账号，而不是排队等满载的那个。
func TestSaturatedAccountRoutesToIdlePeer(t *testing.T) {
	p, calls := slotTestHandler(t, []string{"a1", "a2"}, 1)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 3})

	if !p.AcquireLock("a1") {
		t.Fatal("occupy a1")
	}
	defer p.ReleaseLock("a1")

	start := time.Now()
	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("GLM-5.2"), "k")
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 via idle peer: %s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(calls); got == 0 {
		t.Fatal("request never reached upstream")
	}
	// 应该几乎立即落到 a2；明显短于等待窗说明没有在 a1 上排队。
	if elapsed > 2*time.Second {
		t.Fatalf("took %s; saturated account should hand off to an idle peer immediately", elapsed)
	}
}
