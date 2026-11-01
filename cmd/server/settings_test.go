package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
		{"cooldown": map[string]any{"soft_rate": "invalid-duration"}},
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
