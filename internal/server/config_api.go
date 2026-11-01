package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// SettingsStore persists validated settings; changes take effect after restart.
type SettingsStore interface {
	Read() (map[string]any, error)
	Update(map[string]any) (map[string]any, error)
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
		"config": config, "restart_required": false, "env_overrides": configEnvironmentOverrides(),
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
	config, err := h.cfg.Settings.Update(patch)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "config": config, "restart_required": true, "env_overrides": configEnvironmentOverrides(),
	})
}
