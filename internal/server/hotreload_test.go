package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api-panel/internal/livecfg"
)

// liveSettingsStore 模拟 cmd/server 里"保存 + 热应用"的存储，并回传重启清单。
type liveSettingsStore struct {
	testSettingsStore
	restart []string
}

func (s liveSettingsStore) UpdateLive(patch map[string]any) (map[string]any, []string, error) {
	return patch, s.restart, s.err
}

// 保存接口必须回传"还需重启"的字段数组，前端据此区分"立即生效"与"需重启"。
func TestSettingsAPIReturnsRestartList(t *testing.T) {
	h := NewHandler(Config{
		APIKey: "gateway-test", AdminKey: "admin-test",
		Settings: liveSettingsStore{restart: []string{"listen", "state_file"}},
	})
	req := httptest.NewRequest("PUT", "/admin/api/config", strings.NewReader(`{"default_model":"x"}`))
	req.Header.Set("Authorization", "Bearer admin-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("save rejected: %d %s", w.Code, body)
	}
	if !strings.Contains(body, `"restart_required":["listen","state_file"]`) {
		t.Fatalf("restart list missing or wrong shape: %s", body)
	}
}

// 没有热应用能力时（测试/裸用），保存仍要继续工作，清单为空数组。
func TestSettingsAPIWithoutApplierReportsEmptyRestartList(t *testing.T) {
	h := NewHandler(Config{APIKey: "test", Settings: testSettingsStore{}})
	req := httptest.NewRequest("PUT", "/admin/api/config", strings.NewReader(`{"default_model":"x"}`))
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restart_required":[]`) {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body)
	}
}

// 鉴权密钥、默认模型等字段从 livecfg 快照读：Store 之后立即生效，无需重启。
func TestLiveSnapshotDrivesAuthAndDefaults(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{
		APIKey:       "key-1",
		AdminKey:     "admin-1",
		DefaultModel: "model-1",
		SoftCooldown: time.Minute,
		ErrThreshold: 3,
		ErrCooldown:  10 * time.Minute,
	})
	h := NewHandler(Config{Settings: testSettingsStore{}, Live: live})

	status := func(key string) int {
		req := httptest.NewRequest("GET", "/admin/api/config", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if got := status("admin-1"); got != http.StatusOK {
		t.Fatalf("live admin key rejected: %d", got)
	}
	if got := h.live().DefaultModel; got != "model-1" {
		t.Fatalf("default model = %q", got)
	}

	live.Store(livecfg.Snapshot{
		APIKey:       "key-2",
		AdminKey:     "admin-2",
		DefaultModel: "model-2",
		SoftCooldown: 2 * time.Minute,
		ErrThreshold: 5,
		ErrCooldown:  20 * time.Minute,
	})
	if got := status("admin-1"); got != http.StatusUnauthorized {
		t.Fatalf("rotated admin key still accepted: %d", got)
	}
	if got := status("admin-2"); got != http.StatusOK {
		t.Fatalf("new admin key rejected: %d", got)
	}
	got := h.live()
	if got.DefaultModel != "model-2" || got.ErrThreshold != 5 || got.SoftCooldown != 2*time.Minute {
		t.Fatalf("snapshot not refreshed: %+v", got)
	}
}

// 未注入 holder 时回落到启动字段，保证裸用/测试路径行为不变。
func TestLiveFallsBackToStaticConfig(t *testing.T) {
	h := NewHandler(Config{
		APIKey: "static", AdminKey: "static-admin", Settings: testSettingsStore{},
		DefaultModel: "static-model", QueueMaxAttempts: 30, QueueRetryDelay: 10 * time.Second,
	})
	snap := h.live()
	if snap.APIKey != "static" || snap.AdminKey != "static-admin" {
		t.Fatalf("static keys not carried over: %+v", snap)
	}
	if snap.DefaultModel != "static-model" || snap.QueueMaxAttempts != 30 || snap.QueueRetryDelay != 10*time.Second {
		t.Fatalf("static defaults not carried over: %+v", snap)
	}
}

// 面板 overview 的 schedule 必须反映热更新后的值（不能被启动时快照冻住）。
func TestWatchInfoFollowsLiveSnapshot(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{
		WatchEnabled: true, CreditsAutoClaim: true, PollMinutes: 45,
		RefreshSkewMinutes: 20, KeepaliveIntervalMinutes: 7,
		MaxConcurrent: 2, KeepaliveWindow: "5m",
	})
	h := NewHandler(Config{Settings: testSettingsStore{}, Live: live})
	got := h.watchInfo()
	if got["poll_minutes"] != 45 || got["max_concurrent"] != 2 || got["keepalive_window"] != "5m" {
		t.Fatalf("watch info not from live snapshot: %+v", got)
	}
	live.Store(livecfg.Snapshot{PollMinutes: 15, MaxConcurrent: 3})
	if got := h.watchInfo(); got["poll_minutes"] != 15 || got["max_concurrent"] != 3 {
		t.Fatalf("watch info did not refresh: %+v", got)
	}
}

// 未注入快照时沿用启动值，老调用方行为不变。
func TestWatchInfoFallsBackToStatic(t *testing.T) {
	static := map[string]any{"poll_minutes": 30}
	h := NewHandler(Config{Settings: testSettingsStore{}, WatchInfo: static})
	if got := h.watchInfo(); got["poll_minutes"] != 30 {
		t.Fatalf("static watch info lost: %+v", got)
	}
}
