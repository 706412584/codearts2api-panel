package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// SettingsStore 持久化已校验的配置。
type SettingsStore interface {
	Read() (map[string]any, error)
	Update(map[string]any) (map[string]any, error)
}

// SettingsApplier 可选：保存后热应用配置，并返回仍需重启的字段名。
// 未实现时（测试/裸用）按 SettingsStore.Update 处理，重启清单为空。
type SettingsApplier interface {
	UpdateLive(map[string]any) (map[string]any, []string, error)
}

func configEnvironmentOverrides() []string {
	names := []string{}
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(name, "CA2A_") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (h *Handler) adminConfigGet(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "configuration storage unavailable"})
		return
	}
	config, err := h.cfg.Settings.Read()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config": config, "restart_required": []string{}, "env_overrides": configEnvironmentOverrides(),
	})
}

func (h *Handler) adminConfigPut(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "configuration storage unavailable"})
		return
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "configuration is too large or unreadable"})
		return
	}
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "configuration must be a JSON object"})
		return
	}
	var config map[string]any
	var restart []string
	if applier, ok := h.cfg.Settings.(SettingsApplier); ok {
		config, restart, err = applier.UpdateLive(patch)
	} else {
		config, err = h.cfg.Settings.Update(patch)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if restart == nil {
		restart = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "config": config, "restart_required": restart, "env_overrides": configEnvironmentOverrides(),
	})
}
