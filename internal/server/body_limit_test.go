package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

func bodyLimitHandler(t *testing.T, endpoint string, maxBody int64) *Handler {
	t.Helper()
	auths := []*auth.Auth{
		auth.New(t.Name()+"-a", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(time.Second, endpoint)
	}
	return NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 2, ErrThreshold: 1, MaxBodyBytes: maxBody})
}

func chatPayload(imageBytes int) string {
	if imageBytes == 0 {
		return `{"messages":[{"role":"user","content":"hi"}],"stream":false}`
	}
	// base64 里塞重复的 'A' 只是为了撑体积；这里测的是「放不放行」，不是图片内容。
	pad := strings.Repeat("A", imageBytes)
	return fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}],"stream":false}`, pad)
}

// 旧行为：请求体硬编码 8MB 上限，多图/长会话的截图请求直接被拒成 413。
// 现在上限由 config 的 max_body_mb 决定（默认 32MB），9MB 必须放行且**完整转发**。
func TestChatBodyLimitAllowsLargePayloadByDefault(t *testing.T) {
	var received int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received = n
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()

	h := bodyLimitHandler(t, backend.URL, 0) // 0 = 用默认值
	payload := chatPayload(9 << 20)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("9MB 请求被拒绝：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if received < int64(9<<20) {
		t.Fatalf("上游只收到 %d 字节，9MB payload 没有完整转发", received)
	}
}

// 超限时仍然是 413，且不打上游（提示里带上真正的上限，方便对上配置）。
func TestChatBodyLimitRejectsOversize(t *testing.T) {
	var attempts int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	h := bodyLimitHandler(t, backend.URL, 1<<10) // 1KB
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatPayload(4<<10))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want=413 body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 0 {
		t.Fatalf("超限请求不应打到上游：attempts=%d", attempts)
	}
	if !strings.Contains(rec.Body.String(), "1KB") || !strings.Contains(rec.Body.String(), "max_body_mb") {
		t.Fatalf("提示应带上实际上限与配置项名：%s", rec.Body.String())
	}
}
