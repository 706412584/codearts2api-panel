package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentModelAliasIsRequestIDNotDisplayName(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Agent-Type") != "AgentCenter" || !strings.Contains(strings.ToLower(r.Header.Get("Authorization")), "agent-type;") {
			t.Error("AgentCenter routing header missing from signature")
		}
		switch r.URL.Path {
		case EpAgentList:
			io.WriteString(w, `{"agents":[{"agent_id":"a","is_primary_agent":true}]}`)
		case EpAgentDetail:
			io.WriteString(w, `{"gpts":{"models":[{"model_alias":"kimi-k3","model_name":"Kimi K3","model_parameters":{"context_window":1048576}}]}}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer s.Close()
	c := New(time.Second)
	c.snapBase = s.URL
	models, err := c.fetchAgentModels(SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk"})
	if err != nil || len(models) != 1 || models[0].ID != "kimi-k3" || models[0].Name != "Kimi K3" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}
