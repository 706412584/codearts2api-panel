package server

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"codearts2api-panel/internal/reqlog"
)

type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// Ring keeps a bounded log snapshot; HTTP bodies and credentials are never
// added by request recording. Redaction also protects accidental library logs.
type Ring struct {
	mu       sync.Mutex
	entries  []LogEntry
	capacity int
}

var credentialLogPattern = regexp.MustCompile(`(?i)(["']?(?:access_key_id|secret_access_key|security_token|securitytoken|cloud_dragon_token|refresh_token|access_token|code_verifier|dpop_private_key|dpop|authorization|x-auth-token|x-security-token|api_key|admin_key|secret)["']?\s*[:=]\s*)[^\r\n]*`)

func NewRing(capacity int) *Ring {
	if capacity <= 0 { capacity = 500 }
	return &Ring{capacity: capacity}
}

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line == "" {
			continue
		}
		line = credentialLogPattern.ReplaceAllString(line, `${1}"[REDACTED]"`)
		if len(line) > 4096 {
			line = line[:4096] + "…"
		}
		channel := "sys"
		if strings.HasPrefix(line, "chat ") {
			channel = "chat"
		}
		if strings.Contains(line, "checkin ") || strings.Contains(line, "credit checkin ") {
			channel = "task"
		}
		r.entries = append(r.entries, LogEntry{TS: time.Now(), Ch: channel, Text: line})
		if len(r.entries) > r.capacity {
			r.entries = r.entries[len(r.entries)-r.capacity:]
		}
	}
	return len(p), nil
}

func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

func (h *Handler) LogWriter() io.Writer { return h.logs }

func (h *Handler) adminLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": h.logs.Snapshot()})
}

func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.cfg.RequestLog.Snapshot())
}

type responseObserver struct {
	http.ResponseWriter
	status    int
	wrote     bool
	started   time.Time
	firstByte time.Duration
}

func (w *responseObserver) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote, w.status = true, status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseObserver) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.firstByte == 0 {
		w.firstByte = time.Since(w.started)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseObserver) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseObserver) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func captureUsage(event *reqlog.Event, usage map[string]any) {
	number := func(key string) float64 {
		switch value := usage[key].(type) {
		case float64:
			return value
		case int:
			return float64(value)
		case int64:
			return float64(value)
		default:
			return 0
		}
	}
	event.PromptTokens = int64(number("prompt_tokens"))
	event.CompletionTokens = int64(number("completion_tokens"))
	event.TotalTokens = int64(number("total_tokens"))
	if _, ok := usage["credit"]; ok {
		event.Credit, event.HasCredit = number("credit"), true
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if hit, ok := details["cached_tokens"].(float64); ok {
			event.CacheHitTokens = int64(hit)
		}
	}
	if hit, ok := usage["prompt_cache_hit_tokens"].(float64); ok {
		event.CacheHitTokens = int64(hit)
	}
}

func safeLogLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, value)
	if len(value) > 160 {
		return value[:160]
	}
	return value
}

func (h *Handler) finishRequest(event reqlog.Event, observed *responseObserver, started time.Time) {
	event.Status = observed.status
	event.DurationMs = time.Since(started).Milliseconds()
	event.TTFBMs = observed.firstByte.Milliseconds()
	h.cfg.RequestLog.Record(event)
	fmt.Fprintf(h.logs, "chat request=%s model=%s status=%d ok=%t duration_ms=%d tokens=%d\n", event.RequestID, safeLogLabel(event.Model), event.Status, event.OK, event.DurationMs, event.TotalTokens)
}
