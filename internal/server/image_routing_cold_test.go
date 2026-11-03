package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// 冷启动 + 带图打纯文本模型，是「目录未发现时 analyze 不路由」的回归测试。
//
// 目录是内存态、重启即空。catalogDeclaresNoImages 只看「已发现」的目录，目录为空时
// 它把「不知道能力」当成「能收图」放行 —— 图片原样发上游，吃 InferHub.001001020
// not multimodal。修复后：带图请求在判定能力前先 ensureCatalog 补一次目录。
//
// 上游这里同时扮演三个角色，靠路径区分：
//   - /v1/agent-center/... 与 /v1/model/builtin：模型目录发现
//   - /api/v2/chat/completions：主模型 / 视觉模型
func TestChatColdCatalogRoutesImageToAnalyze(t *testing.T) {
	var catalogFetched, mainCalls, visionCalls atomic.Int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "useragents"):
			catalogFetched.Add(1)
			_, _ = io.WriteString(w, `{"agents":[{"agent_id":"agent-1","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
			return
		case strings.Contains(r.URL.Path, "agents/detail"):
			// glm-5.2 明确声明不支持图片；Qwen3-VL-235B 支持。
			no, yes := false, true
			_, _ = io.WriteString(w, `{"gpts":{"models":[
				{"model_name":"glm-5.2","model_parameters":{"model_id":"glm-5.2","supports_images":`+boolJSON(no)+`}},
				{"model_name":"qwen","model_parameters":{"model_id":"Qwen3-VL-235B","supports_images":`+boolJSON(yes)+`}}
			]}}`)
			return
		case strings.Contains(r.URL.Path, "model/builtin"):
			_, _ = io.WriteString(w, `{"builtinModels":[]}`)
			return
		}

		// chat：按模型名区分主模型 / 视觉模型
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(model, "VL") {
			visionCalls.Add(1)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"图片里有 3 个红色圆\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		n := mainCalls.Add(1)
		if n == 1 {
			// 关键断言：主模型首轮**不得**收到 image_url —— 说明图被换成了占位符。
			if strings.Contains(string(body), "image_url") {
				t.Errorf("冷目录下主模型不应收到图片本体（analyze 未路由）")
			}
			id := extractImageID(string(body))
			if id == "" {
				t.Errorf("主模型应收到 [ImageId: ...] 占位符，body=%.300s", body)
			}
			args, _ := json.Marshal(map[string]any{"imageId": []string{id}, "task": "几个红圆"})
			frame := map[string]any{"choices": []any{map[string]any{
				"index": 0, "finish_reason": "tool_calls",
				"delta": map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": analyzeImageToolName, "arguments": string(args)},
				}}},
			}}}
			raw, _ := json.Marshal(frame)
			_, _ = io.WriteString(w, "data: "+string(raw)+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"3 个红色圆\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	// 冷启动：目录刻意不预先 SetAccountModels —— 全靠 ensureCatalog 现拉。
	a := auth.New("cold-acct", "user", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		// base URL 同时服务 chat 与目录发现
		acct.Client = upstream.NewWithBaseURL(5*time.Second, up.URL)
	}
	if _, ok := upstream.AccountModels("cold-acct"); ok {
		t.Fatal("前置：测试开始时目录必须为空（模拟冷启动）")
	}
	h := NewHandler(Config{
		Pool: p, DefaultModel: "glm-5.2", MaxRotate: 1,
		ErrThreshold: 1, ImageHandling: "analyze",
		// 目录发现走 cfg.Upstream（与 chat 的 acct.Client 是两个句柄），必须一并注入，
		// 否则 ensureCatalog 会去打真实主机、测试观察不到。
		Upstream: upstream.NewWithBaseURL(5*time.Second, up.URL),
	})

	payload := `{"model":"glm-5.2","stream":false,"max_tokens":200,
		"messages":[{"role":"user","content":[{"type":"text","text":"几个红圆"},{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if catalogFetched.Load() == 0 {
		t.Error("带图请求在目录未发现时应补拉一次目录")
	}
	if mainCalls.Load() < 2 {
		t.Fatalf("应发生工具回灌（主模型至少 2 次），实际 %d", mainCalls.Load())
	}
	if visionCalls.Load() != 1 {
		t.Errorf("视觉模型应被调用 1 次，实际 %d", visionCalls.Load())
	}
	if !strings.Contains(rec.Body.String(), "3 个红色圆") {
		t.Errorf("最终答复应含视觉分析结果，got=%s", rec.Body.String())
	}
}

// 目录已就绪时，带图请求不得再触发目录补拉（热路径零额外上游调用）。
func TestChatWarmCatalogSkipsRediscovery(t *testing.T) {
	var catalogFetched atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "useragents") || strings.Contains(r.URL.Path, "agents/detail") ||
			strings.Contains(r.URL.Path, "model/builtin") {
			catalogFetched.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	a := auth.New("warm-acct", "user", "domain", "token", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	upstream.SetAccountModels("warm-acct", []upstream.ModelInfo{{ID: "glm-5.2", SupportsImages: boolPtr(false)}})
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithBaseURL(5*time.Second, up.URL)
	}
	h := NewHandler(Config{Pool: p, DefaultModel: "glm-5.2", MaxRotate: 1, ErrThreshold: 1, ImageHandling: "analyze",
		Upstream: upstream.NewWithBaseURL(5*time.Second, up.URL)})

	payload := `{"model":"glm-5.2","stream":false,"max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"` + tinyPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	if catalogFetched.Load() != 0 {
		t.Errorf("目录已就绪时不应补拉目录，实际补拉 %d 次", catalogFetched.Load())
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
