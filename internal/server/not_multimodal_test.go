package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 上游「请求模型非多模态」（InferHub.001001020.406）必须按「客户端请求内容」问题处理：
// 不换账号、不冷却账号，直接 400 返回。
//
// 背景：上游把 406 包成 HTTP 502，落进 handleUpstreamError 的 Status>=500 分支 →
// 每个账号各冷却 10 分钟 → 整池不可用。实测一次带图请求就把两个账号一起打停
// （2026-11-01~11-03 共 124 次整池冷却），必须回归住。
func TestChatCompletionsNotMultimodalDoesNotCoolAccounts(t *testing.T) {
	bodies := map[string]string{
		// 上游实测返回的形态：纯文本 body
		"plain": "InferHub.001001020.406 The request model is not multimodal",
		// 兼容 JSON 包装形态
		"json": `{"error":{"code":"InferHub.001001020.406","message":"The request model is not multimodal"}}`,
		// 多模态内容格式错误
		"bad_format": "InferHub.001001046 The multimodal content format is invalid",
		// 图片过大：上游媒体转换返回 413，被包成 502
		"too_large": "InferHub.ModelArts.81029.400 Media conversion failed: downstream returned 413 Payload Too Large",
	}
	for name, body := range bodies {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", name, stream), func(t *testing.T) {
				var attempts atomic.Int32
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, body)
				}))
				defer backend.Close()

				h, p := toolChoiceTestHandler(t, backend.URL)
				payload := fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"text","text":"这张图是什么"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}],"stream":%t}`, stream)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status=%d want=400 body=%s", rec.Code, rec.Body.String())
				}
				if attempts.Load() != 1 {
					t.Fatalf("非多模态错误不应换号重试：attempts=%d", attempts.Load())
				}
				assertToolChoicePoolHealthy(t, p)
			})
		}
	}
}

// 真正的上游故障（5xx）仍然必须冷却账号——上面的放宽不能把这条行为一起吃掉。
func TestChatCompletionsOther502StillCools(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "InferHub.003000001 upstream internal error")
	}))
	defer backend.Close()
	h, p := toolChoiceTestHandler(t, backend.URL)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	_, healthy, disabled, cooling, _ := p.Stats()
	if rec.Code < 500 || healthy != 0 || disabled != 0 || cooling != 2 {
		t.Fatalf("普通 502 不应失去冷却行为：status=%d healthy=%d disabled=%d cooling=%d", rec.Code, healthy, disabled, cooling)
	}
}
