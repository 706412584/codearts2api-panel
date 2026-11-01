package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreditBalanceUsesUnsignedRoutingHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpPackageInfo {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Security-Token") != "st" {
			t.Error("missing STS token")
		}
		if strings.Contains(strings.ToLower(r.Header.Get("Authorization")), "agent-type;") {
			t.Error("statistics routing header entered signature")
		}
		if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Language") != "zh-cn" {
			t.Error("missing statistics routing headers")
		}
		_, _ = io.WriteString(w, `{"package":{"package_name_cn":"trial","spec_code":"trial"},"metrics":[{"name":"usageTotalPackageCredit","package_credit_remain":10.25},{"name":"usageBasicPackageCredit","package_credit_amount":20,"package_credit_used":9.75,"package_credit_remain":10.25}]}`)
	}))
	defer server.Close()
	c := New(time.Second)
	c.snapBase = server.URL
	cred := SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st"}
	credit, err := c.FetchAccountInfo(context.Background(), cred)
	if err != nil || credit.Total != 10.25 || len(credit.Packages) != 1 {
		t.Fatalf("credit=%+v err=%v", credit, err)
	}
}

func TestCreditBalanceMissingFieldsAreNotZero(t *testing.T) {
	for _, tc := range []struct {
		body      string
		wantError bool
	}{
		{`{"package":{},"metrics":[]}`, false},
		{`{"package":{},"metrics":[{"name":"usageTotalPackageCredit"}]}`, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer s.Close()
			c := New(time.Second)
			c.snapBase = s.URL
			value, err := c.FetchAccountInfo(context.Background(), SignCredential{})
			if !tc.wantError && value != nil {
				t.Fatal("unknown credit became zero")
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("error %v", err)
			}
		})
	}
}
