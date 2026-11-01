package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefreshProofFailureIsNotTokenTerminal(t *testing.T) {
	for _, tc := range []struct {
		code     string
		terminal bool
	}{{"InvalidDPoPHeader", false}, {"STS5.1806", true}, {"ExpiredRefreshToken", true}} {
		t.Run(tc.code, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(400)
				io.WriteString(w, `{"error_code":"`+tc.code+`","error_msg":"rejected"}`)
			}))
			defer s.Close()
			cfg := DefaultLoginConfig()
			cfg.STSHost = s.URL
			jwk, _ := NewDPoPPrivateJWK()
			_, err := New(time.Second).RefreshToken(context.Background(), cfg, "rt", "verifier", jwk)
			var expired *RefreshTokenExpiredError
			if err == nil || errors.As(err, &expired) != tc.terminal {
				t.Fatalf("error %v, terminal=%v", err, tc.terminal)
			}
		})
	}
}

func TestPermanentChatOmitsEmptySecurityTokenHeader(t *testing.T) {
	if _, exists := ChatHeadersV2("", "trace", "en")["x-auth-token"]; exists {
		t.Fatal("permanent credentials emitted empty x-auth-token")
	}
}
