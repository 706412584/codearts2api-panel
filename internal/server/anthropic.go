// anthropic.go Anthropic Messages 协议适配（Claude Code、Anthropic SDK 等客户端）。
//
// 本网关上游只提供 OpenAI chat completions，故此处把 Anthropic 请求翻成 chat 请求，
// 复用 /v1/chat/completions 的完整管道（账号轮转 / 并发排队 / 重试 / 图片处理 /
// 目录守卫 / 请求日志），再把 chat 响应或 SSE 流翻回 Anthropic 形状。转换本身由
// internal/apicompat 提供（移植自第三方 sub2api，LGPL-3.0，见其 PROVENANCE.md）。
//
// 采用「内部回环」而非复制 chat 主循环：chat 路径的状态机很大且与调度深度耦合，
// 回环让本功能对既有的 OpenAI 生产路径**零改动**（不碰 chatCompletions 一行）。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"unicode"

	"codearts2api-panel/internal/apicompat"
)

// anthropicErrType 把 HTTP 状态映射为 Anthropic 的错误类型。
// Claude Code 按 Anthropic 协议解析错误体；回 OpenAI 格式它只能显示成未知错误。
func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// writeAnthropicError 以 Anthropic 的错误格式回错。
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// anthropicMessages POST /v1/messages
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	limit := h.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"request body exceeds "+humanBytes(limit)+" limit (config max_body_mb)")
		return
	}
	var areq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if areq.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	// Anthropic → OpenAI chat 请求（apicompat 的信道保真转换）。
	creq, err := apicompat.AnthropicToChatCompletionsRequest(&areq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "convert request: "+err.Error())
		return
	}
	// 客户端发来的 model 是 Anthropic 名（claude-*），网关不以它选号——上游没有这个
	// 模型，直发会 002002009.404。统一用配置的默认模型（运维在面板里选的）。
	creq.Model = h.live().DefaultModel
	creq.Stream = areq.Stream
	if areq.Stream {
		creq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}
	chatBody, err := json.Marshal(creq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "marshal request: "+err.Error())
		return
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(chatBody)).
		WithContext(r.Context())
	req2.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("Authorization"); v != "" {
		req2.Header.Set("Authorization", v)
	}
	// 透传亲和头：多轮对话锁定同一账号，与 OpenAI 直连路径行为一致。
	for _, name := range []string{"X-Codearts-Chat-Id", "X-Session-Affinity", "X-Session-Id"} {
		if v := r.Header.Get(name); v != "" {
			req2.Header.Set(name, v)
		}
	}

	if !areq.Stream {
		rec := httptest.NewRecorder()
		h.chatCompletions(rec, req2)
		h.anthropicFromBuffered(w, rec.Code, rec.Body.Bytes())
		return
	}

	// 流式：用 io.Pipe 把 chat 的 SSE 实时转成 Anthropic 事件流。
	// pipeWriter 记录 chat 是否已写响应头及状态，据此区分「真流（200）」与
	// 「错误退出（≥400，正文是 OpenAI 错误 JSON）」。
	pr, pw := io.Pipe()
	sw := newPipeWriter(pw)
	chatDone := make(chan struct{})
	go func() {
		defer close(chatDone)
		h.chatCompletions(sw, req2)
		pw.Close()
	}()
	select {
	case <-sw.headerWritten():
	case <-chatDone:
	}
	if sw.Status() >= http.StatusBadRequest {
		raw, _ := io.ReadAll(pr) // 抽空管道，让 writer goroutine 能退出
		status := sw.Status()
		writeAnthropicError(w, status, anthropicErrType(status), openAIErrorMessage(raw))
		<-chatDone
		return
	}
	h.relayAnthropicStream(w, pr, areq.Model, chatDone)
}

// anthropicFromBuffered 把（已完整结束的）chat 响应转成 Anthropic 响应或错误。
func (h *Handler) anthropicFromBuffered(w http.ResponseWriter, status int, raw []byte) {
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		writeAnthropicError(w, status, anthropicErrType(status), openAIErrorMessage(raw))
		return
	}
	var cresp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(raw, &cresp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "parse upstream chat response: "+err.Error())
		return
	}
	model := cresp.Model
	if model == "" {
		model = h.live().DefaultModel
	}
	writeJSON(w, http.StatusOK, apicompat.ChatCompletionsResponseToAnthropic(&cresp, model))
}

// openAIErrorMessage 从 OpenAI 错误体里抠出 message，取不到就回原样文本。
func openAIErrorMessage(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "upstream error"
	}
	return truncateMsg(s, errorTextLimit)
}

// relayAnthropicStream 把回环得到的 chat SSE 逐帧转成 Anthropic 事件流写回客户端。
func (h *Handler) relayAnthropicStream(w http.ResponseWriter, pr *io.PipeReader, clientModel string, done <-chan struct{}) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	state := apicompat.NewChatCompletionsToAnthropicStreamState(clientModel)
	emit := func(evts []apicompat.AnthropicStreamEvent) bool {
		for _, e := range evts {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(e)
			if err != nil {
				continue
			}
			if _, werr := io.WriteString(w, sse); werr != nil {
				return false
			}
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}

	br := bufio.NewReaderSize(pr, 64*1024)
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(trimmed[len("data:"):])
			if payload == "[DONE]" {
				break
			}
			var chunk apicompat.ChatCompletionsChunk
			if jerr := json.Unmarshal([]byte(payload), &chunk); jerr == nil {
				if !emit(apicompat.ChatCompletionsChunkToAnthropicEvents(&chunk, state)) {
					pr.Close() // 客户端断连：关读端，让 writer 尽快返回
					<-done
					return
				}
			}
		}
		if err != nil {
			break // io.EOF 或读错误：收尾
		}
	}
	// 即便中途断流也发收尾事件，否则客户端会一直等 message_stop（表现为"卡住"）。
	emit(apicompat.FinalizeChatCompletionsAnthropicStream(state))
	<-done
}

// countTokens POST /v1/messages/count_tokens
//
// 上游没有 token 计数接口，故本地估算。Claude Code 用它做上下文预算，估值有偏差
// 可接受，但绝不能报错——该端点失败会让客户端提前压缩上下文。
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	limit := h.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var areq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	n := estimateTokens(string(areq.System))
	for _, m := range areq.Messages {
		n += estimateTokens(string(m.Content))
	}
	for _, t := range areq.Tools {
		n += estimateTokens(t.Name) + estimateTokens(t.Description) + len(t.InputSchema)/4
	}
	if n < 1 {
		n = 1 // 上游对空输入也算 1 个 token，返回 0 会让客户端困惑
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
}

// estimateTokens 估算文本 token 数。CJK 约 0.67 token/字符，ASCII 约 0.25。
// 不追求精确——精确计数需要与上游一致的分词器，本项目拿不到。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var n float64
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			n += 0.67
		} else {
			n += 0.25
		}
	}
	if n < 1 {
		return 1
	}
	return int(n + 0.5)
}

// ---------------------------------------------------------------------------
// pipeWriter：把 chat 路径写出的内容导入 io.Pipe，并记录状态码
// ---------------------------------------------------------------------------

// pipeWriter 是喂给 h.chatCompletions 的 ResponseWriter。它把 SSE 字节流写进
// io.Pipe 供上层实时转换；同时用 once 记录「首个响应头/首写」时刻，使调用方能
// 在该时刻判断这是成功流（200）还是错误（≥400）。
//
// 之所以不用 httptest.ResponseRecorder：后者只在 handler 全部返回后才能取到正文，
// 无法把 SSE 实时转出去（会退化成「先攒完整个流再转」）。
type pipeWriter struct {
	pw     *io.PipeWriter
	hdr    http.Header
	once   sync.Once
	ch     chan struct{}
	status int
}

func newPipeWriter(pw *io.PipeWriter) *pipeWriter {
	return &pipeWriter{pw: pw, hdr: make(http.Header), ch: make(chan struct{}), status: http.StatusOK}
}

func (p *pipeWriter) Header() http.Header { return p.hdr }

func (p *pipeWriter) WriteHeader(status int) {
	p.once.Do(func() {
		p.status = status
		close(p.ch)
	})
}

func (p *pipeWriter) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.ch) }) // 隐式 200：先 Write 未显式 WriteHeader
	return p.pw.Write(b)
}

func (p *pipeWriter) Flush() {}

// headerWritten 在首个响应头/首写发生时关闭。
func (p *pipeWriter) headerWritten() <-chan struct{} { return p.ch }

// Status 返回记录的 HTTP 状态（默认 200）。须在 headerWritten() 关闭后读，才保证可见。
func (p *pipeWriter) Status() int { return p.status }
