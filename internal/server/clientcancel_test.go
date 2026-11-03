package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

type cancelStageWriter struct {
	*httptest.ResponseRecorder
	onHeader func()
	onWrite  func([]byte) (int, error)
}

func (w *cancelStageWriter) Header() http.Header {
	if w.onHeader != nil {
		callback := w.onHeader
		w.onHeader = nil
		callback()
	}
	return w.ResponseRecorder.Header()
}

func (w *cancelStageWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		return w.onWrite(p)
	}
	return w.ResponseRecorder.Write(p)
}

func TestClientCancelAtUpstreamStagesDoesNotCoolAccounts(t *testing.T) {
	for _, stage := range []string{"preflight", "stream", "aggregate"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var attempts atomic.Int32
			releaseBackend := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				attempts.Add(1)
				if stage == "preflight" {
					// Cancel only after the upstream has received the connected request.
					cancel()
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-releaseBackend:
				}
			}))
			defer backend.Close()
			defer close(releaseBackend)

			h, p := toolChoiceTestHandler(t, backend.URL)
			rec := httptest.NewRecorder()
			writer := &cancelStageWriter{ResponseRecorder: rec}
			cancelledAtStage := false
			firstFailedChunk := ""
			switch stage {
			case "stream":
				writer.onWrite = func(chunk []byte) (int, error) {
					if firstFailedChunk == "" {
						firstFailedChunk = string(chunk)
					}
					cancelledAtStage = true
					cancel()
					return 0, errors.New("broken pipe")
				}
			case "aggregate":
				// The first response Header call sets X-Codearts-Chat-Id after SSE preflight.
				writer.onHeader = func() {
					cancelledAtStage = true
					cancel()
				}
			}
			payload := `{"messages":[{"role":"user","content":"hi"}],"stream":false}`
			if stage == "stream" {
				payload = strings.Replace(payload, "false", "true", 1)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)).WithContext(ctx)
			h.ServeHTTP(writer, req)

			if ctx.Err() != context.Canceled {
				t.Fatalf("stage must cancel deterministically before the safety timeout: %v", ctx.Err())
			}
			if stage != "preflight" && (!cancelledAtStage || rec.Header().Get("X-Codearts-Chat-Id") == "") {
				t.Fatal("request did not pass SSE preflight before cancellation")
			}
			if stage == "stream" {
				if !strings.Contains(firstFailedChunk, "partial") {
					t.Fatalf("cancel must occur while writing the first content chunk: %q", firstFailedChunk)
				}
			} else if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "client_cancelled") {
				t.Fatalf("cancellation must return 503 client_cancelled: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if attempts.Load() != 1 {
				t.Fatalf("client cancellation must not rotate accounts: upstream attempts=%d", attempts.Load())
			}
			events := h.cfg.RequestLog.Snapshot().Recent
			if len(events) != 1 || events[0].Attempts != 1 {
				t.Fatalf("client cancellation must stop after the connected attempt: %+v", events)
			}
			assertToolChoicePoolHealthy(t, p)
		})
	}
}
