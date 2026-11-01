package upstream

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSERequiresATrustedTerminalEvent(t *testing.T) {
	for _, wire := range []string{"", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "data: {\"choices\":[],\"usage\":{\"total_tokens\":3}}\n\n"} {
		if _, err := AggregateRaw(strings.NewReader(wire)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("aggregate accepted incomplete stream: %q err=%v", wire, err)
		}
		called := false
		w := httptest.NewRecorder()
		err := StreamCapture(w, strings.NewReader(wire), "model", func(*RawCompletion) { called = true })
		if !errors.Is(err, io.ErrUnexpectedEOF) || called || !strings.Contains(w.Body.String(), "event: error") {
			t.Fatalf("stream accepted incomplete response: err=%v done=%t body=%s", err, called, w.Body.String())
		}
	}
	for _, wire := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"text\":\"[DONE]\",\"error_code\":\"0\"}\n",
		"event: done\ndata: {\"error_code\":\"0\"}\n",
		"data: [DONE]\n\n",
	} {
		if _, err := AggregateRaw(strings.NewReader(wire)); err != nil {
			t.Fatalf("trusted terminal rejected: %v", err)
		}
	}
}
