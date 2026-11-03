package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// timeAfter 只是把 time.After 包一层，方便在测试里读。
func timeAfter(ms int) <-chan time.Time {
	return time.After(time.Duration(ms) * time.Millisecond)
}

// 调用方自己断开（context canceled）绝不能记到账号头上。
//
// 线上实测（2026-11-03 12:42~12:46）：客户端超时/回压交替取消请求，上游报
// `context canceled`，旧逻辑按聚合错误 Cooldown(10m) 把两个账号互相打进冷却，
// 之后整池 503 —— 与带图请求那次是同一类误伤。
func TestClientCancelDoesNotCoolAccounts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 上游慢慢回；客户端已经走了，这里的行为不影响结论。
		select {
		case <-r.Context().Done():
		case <-timeAfter(50):
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h, p := toolChoiceTestHandler(t, backend.URL)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":false}`)).WithContext(ctx)
	cancel() // 客户端在发出去的同时就断开

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=503 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "client_cancelled") {
		t.Fatalf("应当回 client_cancelled：%s", rec.Body.String())
	}
	total, healthy, disabled, cooling, _ := p.Stats()
	if total != 2 || healthy != 2 || disabled != 0 || cooling != 0 {
		t.Fatalf("客户端取消不该动账号状态：total=%d healthy=%d disabled=%d cooling=%d", total, healthy, disabled, cooling)
	}
	for _, st := range p.List() {
		if st["err_count"] != 0 {
			t.Fatalf("客户端取消不该累计错误：%#v", st)
		}
		if st["active_concurrent"] != 0 {
			t.Fatalf("客户端取消后并发槽位必须释放：%#v", st)
		}
	}
}
