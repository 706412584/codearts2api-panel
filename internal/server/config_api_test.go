package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testSettingsStore struct {
	err error
}

func (s testSettingsStore) Read() (map[string]any, error) {
	return map[string]any{"api_key": "", "admin_key": "", "default_model": "test"}, s.err
}

func (s testSettingsStore) Update(patch map[string]any) (map[string]any, error) {
	return patch, s.err
}

func TestSettingsAPIMustUseAdminKeyAndRejectMalformedInput(t *testing.T) {
	h := NewHandler(Config{APIKey: "gateway-test", AdminKey: "admin-test", Settings: testSettingsStore{}})
	for _, tc := range []struct {
		method, key, body string
		status            int
	}{
		{"GET", "", "", http.StatusUnauthorized},
		{"GET", "gateway-test", "", http.StatusUnauthorized},
		{"GET", "admin-test", "", http.StatusOK},
		{"PUT", "admin-test", "null", http.StatusBadRequest},
		{"PUT", "admin-test", "[]", http.StatusBadRequest},
		{"PUT", "admin-test", "{} {}", http.StatusBadRequest},
		{"PUT", "admin-test", "{", http.StatusBadRequest},
		{"PUT", "admin-test", `{"default_model":"updated"}`, http.StatusOK},
	} {
		req := httptest.NewRequest(tc.method, "/admin/api/config", strings.NewReader(tc.body))
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s key=%s status=%d want=%d body=%s", tc.method, tc.key, w.Code, tc.status, w.Body)
		}
	}
}

func TestSettingsAPIReportsPersistenceFailure(t *testing.T) {
	h := NewHandler(Config{APIKey: "test", Settings: testSettingsStore{err: errors.New("write failed")}})
	req := httptest.NewRequest("PUT", "/admin/api/config", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("failed save reported success: %s", w.Body)
	}
}
