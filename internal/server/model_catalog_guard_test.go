package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// catalogGuardHandler 组一个只有单个账号的 handler，并返回替身上游的调用计数。
func catalogGuardHandler(t *testing.T, uid string, catalog []upstream.ModelInfo) (*Handler, *int32) {
	t.Helper()
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(up.Close)

	a := auth.New(uid, "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	if catalog != nil {
		upstream.SetAccountModels(uid, catalog)
	}
	return NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1}), &calls
}

func chatBody(model string) string {
	return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
}

// 目录里没有这个 ID 就本地 404，不要带着它去打上游（上游只会回
// InferHub.002002009.404，还白占一次并发会话与轮转名额）。
func TestChatRejectsModelOutsideCatalog(t *testing.T) {
	h, calls := catalogGuardHandler(t, "guard-acct-a", []upstream.ModelInfo{{ID: "GLM-5.2"}, {ID: "DeepSeek-V4.1-Flash-1017"}})

	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("test1"), "k")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "model_not_available") || !strings.Contains(body, "test1") || !strings.Contains(body, "/v1/models") {
		t.Fatalf("unhelpful 404 body: %s", body)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("unknown model still hit upstream %d time(s)", got)
	}
}

// 目录里有的模型照常转发；大小写与旧别名按 CanonicalModel 归一（客户端习惯写小写）。
func TestChatForwardsCataloguedModel(t *testing.T) {
	h, calls := catalogGuardHandler(t, "guard-acct-b", []upstream.ModelInfo{{ID: "GLM-5.2"}})

	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("no upstream answer passed through: %s", w.Body.String())
	}
	if got := atomic.LoadInt32(calls); got == 0 {
		t.Fatal("catalogued model was not forwarded upstream")
	}
}

// 目录还没发现过（本进程没调用过 /v1/models、也没做过惰性发现）时没有依据，
// 不能凭缺失的目录拒请求，仍交给上游判定。
func TestChatAllowsModelWhenCatalogUnknown(t *testing.T) {
	h, calls := catalogGuardHandler(t, "guard-acct-c", nil)

	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("whatever-1"), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(calls); got == 0 {
		t.Fatal("request with unknown catalog must still go upstream")
	}
}

// 空目录（账号确实没有任何模型）也算「目录已知」：这时同样拒，避免把必然 404 的
// 请求打到上游。
func TestChatRejectsModelWhenCatalogIsEmpty(t *testing.T) {
	h, calls := catalogGuardHandler(t, "guard-acct-d", []upstream.ModelInfo{})

	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("GLM-5.2"), "k")
	if w.Code != http.StatusNotFound || atomic.LoadInt32(calls) != 0 {
		t.Fatalf("status = %d calls = %d, want 404 without upstream", w.Code, atomic.LoadInt32(calls))
	}
}

// 守卫按大小写不敏感判定「有账号能服务」，上游却按注册名精确匹配：真正发出去的
// model 必须是**当前账号目录里**的写法，而不是跨账号索引（或种子）里那个。
func TestChatSendsAccountCatalogCase(t *testing.T) {
	var mu sync.Mutex
	var sent string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		sent, _ = body["model"].(string)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer up.Close()

	// 另一个账号把全局大小写索引定成混合写法；本账号的注册名是全小写。
	upstream.SetAccountModels("wire-case-other", []upstream.ModelInfo{{ID: "Case-Probe-Wire"}})
	uid := "wire-case-acct"
	upstream.SetAccountModels(uid, []upstream.ModelInfo{{ID: "case-probe-wire"}})

	a := auth.New(uid, "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1})

	w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("Case-Probe-Wire"), "k")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := sent
	mu.Unlock()
	if got != "case-probe-wire" {
		t.Fatalf("上游收到的 model = %q，应为该账号注册的 case-probe-wire", got)
	}
}

func TestUpstreamUnavailableReason(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{`codearts error code=InferHub.002002009.404 msg=The model is not registered, please request other model`, "注册"},
		{`codearts error code=TM.00001041 msg=并发会话数已达上限`, ""},
		{`dial tcp: i/o timeout`, ""},
	}
	for _, c := range cases {
		got := upstreamUnavailableReason(errors.New(c.msg))
		if c.want == "" && got != "" {
			t.Errorf("%q 不应判为模型不可用，got=%q", c.msg, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%q 应判为模型不可用，got=%q", c.msg, got)
		}
	}
}

// 模型级错误（not registered）不得被当成账号故障：
// 错误可能被上游包装成 5xx，但健康账号不应因此被冷却。
func TestModelErrorDoesNotCoolAccount(t *testing.T) {
	a := &auth.Auth{UserID: "u-cool", UserName: "u-cool", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second), ErrCooldown: time.Minute, ErrThreshold: 1})

	err404 := &upstream.ApiError{
		Code: 502, Status: 502, Path: "/api/v2/chat/completions",
		Message: "InferHub.002002009.404 The model is not registered, please request other model",
	}
	if !h.handleUpstreamError(p.Get("u-cool"), "GLM-5.2-ArkTS-SPARK", err404) {
		t.Error("模型级错误应返回 true，让调用方立即结束轮转")
	}
	if !p.Healthy("u-cool") {
		t.Error("模型级错误不得冷却账号")
	}
}
