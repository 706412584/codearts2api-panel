package pool

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

func TestConcurrentRefreshUsesBoundClientAndPersistsRotatedToken(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "bound-client" {
			t.Error("used global client id")
		}
		if r.Form.Get("code_verifier") != "verifier" || r.Header.Get("DPoP") == "" {
			t.Error("missing proof material")
		}
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		fmt.Fprintf(w, `{"refresh_token":"new-rt","credentials":{"access_key_id":"new-ak","secret_access_key":"new-sk","security_token":"new-st","expiration":%q}}`, time.Now().Add(2*time.Hour).Format(time.RFC3339))
	}))
	defer sts.Close()
	jwk, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u", "n", "d", "st", "ak", "sk", time.Now().Add(time.Minute).Format(time.RFC3339), "old-rt", "verifier", map[string]string(jwk))
	a.SetClientID("bound-client")
	dir := t.TempDir()
	if err := auth.SaveNew(dir, a); err != nil {
		t.Fatal(err)
	}
	cfg := upstream.DefaultLoginConfig()
	cfg.STSHost = sts.URL
	cfg.ClientID = "global-client"
	p, _ := New([]*auth.Auth{a}, Config{LoginConfig: cfg}, "")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); errs <- p.RefreshToken("u") }()
	<-entered
	wg.Add(1)
	go func() { defer wg.Done(); errs <- p.RefreshToken("u") }()
	// Keep the STS boundary blocked while the second caller reaches the account lock.
	time.Sleep(25 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("consumed %d rotating refresh tokens", calls.Load())
	}
	loaded, err := auth.LoadDir(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("reload: %v", err)
	}
	if loaded[0].Refresh() != "new-rt" || loaded[0].ClientIDOr("") != "bound-client" {
		t.Fatal("new token or bound client not persisted")
	}
	stored, _ := json.Marshal(loaded[0].DPoPPrivateJWK())
	original, _ := json.Marshal(map[string]string(jwk))
	if string(stored) != string(original) {
		t.Fatal("DPoP key changed across renewal")
	}
}
