package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 保存时非法值回写默认值：0/负数/解析不了的时长不再报 400，而是按默认值落盘，
// 面板拿到的也是回写后的值（表单立刻显示 10/30，而不是存完还是 0）。
func TestSettingsRewriteInvalidRetryAndCooldownToDefaults(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "")
	t.Setenv("CA2A_ADMIN_KEY", "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	store := &fileSettings{path: path}
	got, err := store.Update(map[string]any{
		"queue_retry_seconds": 0,
		"queue_max_attempts":  -3,
		"cooldown": map[string]any{
			"err_threshold": 0,
			"soft_rate":     "not-a-duration",
			"err_cooldown":  "-5m",
		},
	})
	if err != nil {
		t.Fatalf("非法值应回写默认值而不是拒绝保存: %v", err)
	}
	if got["queue_retry_seconds"] != float64(10) || got["queue_max_attempts"] != float64(30) {
		t.Fatalf("返回给面板的 queue 值未回写: %v / %v", got["queue_retry_seconds"], got["queue_max_attempts"])
	}
	cooldown, _ := got["cooldown"].(map[string]any)
	if cooldown["err_threshold"] != float64(3) || cooldown["soft_rate"] != "60s" || cooldown["err_cooldown"] != "10m" {
		t.Fatalf("返回给面板的 cooldown 值未回写: %v", cooldown)
	}
	// 磁盘上也必须是回写后的值：否则重启后运行期与文件又对不上。
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueueRetrySeconds != 10 || cfg.QueueMaxAttempts != 30 {
		t.Fatalf("落盘的 queue 未归一化: %d / %d", cfg.QueueRetrySeconds, cfg.QueueMaxAttempts)
	}
	if cfg.Cooldown.ErrThresh != 3 || cfg.Cooldown.SoftRate != "60s" || cfg.Cooldown.ErrCooldown != "10m" {
		t.Fatalf("落盘的 cooldown 未归一化: %+v", cfg.Cooldown)
	}
	if cfg.SoftRateDur != time.Minute || cfg.ErrCooldownDur != 10*time.Minute {
		t.Fatalf("解析后的时长不符: %s / %s", cfg.SoftRateDur, cfg.ErrCooldownDur)
	}
}

func TestSettingsNormalizeMultimodalValuesAcrossReadSaveAndLiveConfig(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "")
	t.Setenv("CA2A_ADMIN_KEY", "")
	t.Setenv("CA2A_MAX_BODY_MB", "")
	for _, operation := range []string{"read", "save"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := ensureConfig(path); err != nil {
				t.Fatal(err)
			}
			before, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			patch := map[string]any{
				"api_key": "", "admin_key": "", "max_body_mb": 0,
				"image_shrink": map[string]any{
					"enabled": false, "trigger_body_mb": 0, "target_image_mb": -1,
					"target_total_mb": 0, "max_dimension": -1, "quality": 101,
					"extension": map[string]any{"preserved": "image"},
				},
				"vision_bridge": map[string]any{
					"enabled": false, "model": " ", "max_images": 33, "max_chars": 100001,
					"max_tokens": 0, "timeout_seconds": -1, "total_timeout_seconds": 0,
					"prompt": "custom prompt", "extension": map[string]any{"preserved": "vision"},
				},
				"extension": map[string]any{"preserved": true},
			}
			want := map[string]any{
				"api_key": "", "admin_key": "", "max_body_mb": 32,
				"image_shrink": map[string]any{
					"enabled": false, "trigger_body_mb": 4, "target_image_mb": 2,
					"target_total_mb": 4, "max_dimension": 2048, "quality": 85,
					"extension": map[string]any{"preserved": "image"},
				},
				"vision_bridge": map[string]any{
					"enabled": false, "model": "Qwen3-VL-235B", "max_images": 8, "max_chars": 6000,
					"max_tokens": 1500, "timeout_seconds": 120, "total_timeout_seconds": 90,
					"prompt": "custom prompt", "extension": map[string]any{"preserved": "vision"},
				},
				"extension": map[string]any{"preserved": true},
			}
			assertValues := func(values map[string]any) {
				t.Helper()
				for key, expected := range want {
					gotJSON, err := json.Marshal(values[key])
					if err != nil {
						t.Fatal(err)
					}
					wantJSON, err := json.Marshal(expected)
					if err != nil {
						t.Fatal(err)
					}
					if string(gotJSON) != string(wantJSON) {
						t.Errorf("%s = %s, want %s", key, gotJSON, wantJSON)
					}
				}
			}
			var applied *Config
			store := &fileSettings{path: path, apply: func(c *Config) []string {
				applied = c
				return nil
			}}
			if operation == "read" {
				values, err := configMap(before)
				if err != nil {
					t.Fatal(err)
				}
				delete(patch, "api_key")
				delete(patch, "admin_key")
				mergeConfig(values, patch)
				raw, err := json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				values, _, err := store.UpdateLive(patch)
				if err != nil {
					t.Fatal(err)
				}
				assertValues(values)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &values); err != nil {
					t.Fatal(err)
				}
				if values["api_key"] != before.APIKey || values["admin_key"] != before.AdminKey {
					t.Fatal("saving normalized values changed existing secrets")
				}
				assertValues(redactConfig(values))
				if applied == nil {
					t.Fatal("saved configuration was not applied")
				}
				snap := liveSnapshot(applied)
				if snap.MaxBodyBytes != 32<<20 || snap.ImageShrinkEnabled || snap.VisionBridgeEnabled || snap.VisionBridge.Enabled {
					t.Fatalf("body limit or disabled switches differ in live config: %+v", snap)
				}
				if snap.ImageShrink.TriggerBytes != 4<<20 || snap.ImageShrink.TargetImageBytes != 2<<20 ||
					snap.ImageShrink.TargetTotalBytes != 4<<20 || snap.ImageShrink.MaxDimension != 2048 || snap.ImageShrink.Quality != 85 {
					t.Fatalf("image policy differs in live config: %+v", snap.ImageShrink)
				}
				if snap.VisionBridge.Model != "Qwen3-VL-235B" || snap.VisionBridge.MaxImages != 8 ||
					snap.VisionBridge.MaxChars != 6000 || snap.VisionBridge.MaxTokens != 1500 ||
					snap.VisionBridge.Timeout != 120*time.Second || snap.VisionBridge.TotalTimeout != 90*time.Second ||
					snap.VisionBridge.Prompt != "custom prompt" {
					t.Fatalf("vision policy differs in live config: %+v", snap.VisionBridge)
				}
			}
			values, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			assertValues(values)
		})
	}
}

func TestEnsureConfigCreatesDistinctSecretsAndDoesNotOverwrite(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "")
	t.Setenv("CA2A_ADMIN_KEY", "")
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.APIKey) != 64 || len(cfg.AdminKey) != 64 || cfg.APIKey == cfg.AdminKey {
		t.Fatal("first run must create independent random API and admin keys")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "SoftRateDur") {
		t.Fatal("derived runtime durations must not be written to configuration")
	}
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("initialization overwrote an existing configuration")
	}
}

func TestSettingsPreservesSecretsAndUnknownFieldsAndRejectsInvalidPatch(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "")
	t.Setenv("CA2A_ADMIN_KEY", "")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	before, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &fileSettings{path: path}
	public, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if public["api_key"] != "" || public["admin_key"] != "" {
		t.Fatal("settings endpoint exposed credentials")
	}
	_, err = store.Update(map[string]any{
		"api_key": "", "admin_key": "", "default_model": "model-alias",
		"extension": map[string]any{"preserved": true, "changed": float64(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(map[string]any{"extension": map[string]any{"changed": float64(2)}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != before.APIKey || loaded.AdminKey != before.AdminKey || loaded.DefaultModel != "model-alias" {
		t.Fatal("redacted keys did not preserve the existing secrets")
	}
	values, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	extra := values["extension"].(map[string]any)
	if extra["preserved"] != true || extra["changed"] != float64(2) {
		t.Fatal("nested unknown fields were lost")
	}
	good, _ := os.ReadFile(path)
	for _, patch := range []map[string]any{
		// 时长/次数类的非法值不再拒绝保存：见 TestSettingsRewriteInvalidRetryAndCooldownToDefaults。
		{"listen": "localhost:99999"},
		{"keepalive_window": "-1s"},
		{"api_key": nil},
		{"upstream": map[string]any{"timeout_seconds": "invalid"}},
	} {
		if _, err := store.Update(patch); err == nil {
			t.Fatalf("invalid settings accepted: %v", patch)
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(good) {
			t.Fatal("invalid patch changed the configuration on disk")
		}
	}
}

func TestSettingsStoreValidatesConfigObjectAndEnvironmentKeysOnly(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "env-api-key")
	t.Setenv("CA2A_ADMIN_KEY", "env-admin-key")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	store := &fileSettings{path: path}
	public, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(public)
	if strings.Contains(string(raw), "env-api-key") || strings.Contains(string(raw), "env-admin-key") {
		t.Fatal("environment credentials must not appear in public settings")
	}
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(); err == nil {
		t.Fatal("null configuration accepted")
	}
}

func TestStoragePathsAreRelativeToConfigDirectory(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	resolveDataPaths(cfg, filepath.Join(root, "config.json"))
	if cfg.AuthDir != filepath.Join(root, "auths") || cfg.StateFile != filepath.Join(root, "data", "state.json") {
		t.Fatalf("config storage escaped its directory: %s %s", cfg.AuthDir, cfg.StateFile)
	}
}
