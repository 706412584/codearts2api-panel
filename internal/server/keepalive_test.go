package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
)

// keepaliveTestHandler 组一个多账号 handler，账号都是「凭据已失效」的禁用态。
func keepaliveTestHandler(t *testing.T) *Handler {
	t.Helper()
	mk := func(uid string) *auth.Auth {
		return auth.New(uid, "user", "domain", "token", "ak", "sk",
			time.Now().Add(time.Hour).Format(time.RFC3339), "refresh", "verifier")
	}
	p, err := pool.New([]*auth.Auth{mk("u1"), mk("u2")}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Accounts() {
		p.Disable(a.Name, "refresh_token expired; re-login required")
	}
	return NewHandler(Config{Pool: p, APIKey: "k", AdminKey: "k", DefaultModel: "glm-5.2", MaxRotate: 1})
}

func keepaliveCall(h *Handler, body string) map[string]any {
	r := httptest.NewRequest(http.MethodPost, "/admin/api/keepalive", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer k")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// 批量保活必须**包含已禁用账号**：它们正是要被救回来的对象。此前 pickTargets 跳过
// disabled，导致全池失效时点「全员保活」得到「无目标」。
func TestBulkKeepaliveTargetsDisabledAccounts(t *testing.T) {
	h := keepaliveTestHandler(t)
	out := keepaliveCall(h, `{}`)
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("批量保活应覆盖 2 个（含已禁用的）账号，实际 %d：%v", len(results), out)
	}
}

// 手动保活成功后要清掉禁用标记，否则会出现「提示成功但账号仍是禁用」——token 被
// 同步修好后账号卡死在禁用态。
func TestKeepaliveClearsDisabledFlag(t *testing.T) {
	h := keepaliveTestHandler(t)
	out := keepaliveCall(h, `{"uid":"u1"}`)
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("单账号保活应返回 1 条结果：%v", out)
	}
	row, _ := results[0].(map[string]any)
	// token 未临期，validate 会直接通过（不联网），因此结果为成功。
	if ok, _ := row["ok"].(bool); !ok {
		t.Fatalf("token 未临期时手动保活应成功：%v", row)
	}
	for _, st := range h.cfg.Pool.List() {
		if st["uid"] == "u1" {
			if disabled, _ := st["disabled"].(bool); disabled {
				t.Fatal("手动保活成功后仍标记为禁用（界面会一直显示失效）")
			}
		}
	}
}

// 凭据失效与「上游封禁」必须能被面板区分，否则用户以为号被封了。
func TestDisabledKindSeparatesCredentialFromBan(t *testing.T) {
	h := keepaliveTestHandler(t)
	byUID := map[string]map[string]any{}
	for _, st := range h.cfg.Pool.List() {
		byUID[st["uid"].(string)] = st
	}
	if got := byUID["u1"]["disabled_kind"]; got != "credential" {
		t.Fatalf("refresh_token 失效应归类为 credential，实际 %v", got)
	}

	p2, err := pool.New([]*auth.Auth{auth.New("b1", "u", "d", "t", "ak", "sk",
		time.Now().Add(time.Hour).Format(time.RFC3339), "refresh", "verifier")}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p2.Disable("b1", "account banned by upstream")
	st := p2.List()[0]
	if got := st["disabled_kind"]; got != "banned" {
		t.Fatalf("封禁应归类为 banned，实际 %v", got)
	}
}
