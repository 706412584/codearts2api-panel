package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/balance"
	"codearts2api-panel/internal/pool"
)

func TestBalanceHistoryEndpointUsesAdminAuthAndPreservesDecimals(t *testing.T) {
	a := auth.New("history-u", "历史账号", "", "", "ak", "sk", "", "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	recorder := balance.New("")
	if !recorder.Add("history-u", 12.75) {
		t.Fatal("first sample was rejected")
	}
	h := NewHandler(Config{Pool: p, Balance: recorder, APIKey: "inference", AdminKey: "management"})
	if w := panelRequest(h, http.MethodGet, "/admin/api/balance_history?days=99", "", "inference"); w.Code != http.StatusUnauthorized {
		t.Fatalf("inference key read balance history: %d", w.Code)
	}
	w := panelRequest(h, http.MethodGet, "/admin/api/balance_history?days=99", "", "management")
	if w.Code != http.StatusOK {
		t.Fatalf("balance history status = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Days   int `json:"days"`
		Series []struct {
			UID     string  `json:"uid"`
			Name    string  `json:"name"`
			Current float64 `json:"current"`
			Points  []struct {
				Credits float64 `json:"c"`
			} `json:"points"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Days != 30 || len(out.Series) != 1 || out.Series[0].Current != 12.75 || out.Series[0].Name != "历史账号" {
		t.Fatalf("unexpected balance history: %+v", out)
	}
	if len(out.Series[0].Points) != 1 || out.Series[0].Points[0].Credits != 12.75 {
		t.Fatalf("decimal point was truncated: %+v", out.Series[0].Points)
	}
}

func TestBalanceHistoryUnavailable(t *testing.T) {
	h := NewHandler(Config{AdminKey: "management"})
	w := panelRequest(h, http.MethodGet, "/admin/api/balance_history", "", "management")
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("missing recorder status = %d", w.Code)
	}
}
