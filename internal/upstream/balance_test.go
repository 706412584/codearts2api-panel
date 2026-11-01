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

func TestBalancesKeepLedgerUnitsAndSigningSeparate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method %s", r.Method)
		}
		if r.Header.Get("X-Security-Token") != "st" {
			t.Error("missing STS token")
		}
		if strings.Contains(strings.ToLower(r.Header.Get("Authorization")), "agent-type;") {
			t.Error("statistics routing header entered signature")
		}
		switch r.URL.Path {
		case EpPackageInfo:
			if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Language") != "zh-cn" {
				t.Error("missing statistics routing headers")
			}
			io.WriteString(w, `{"package":{"package_name_cn":"trial","spec_code":"trial"},"metrics":[{"name":"usageTotalPackageCredit","package_credit_remain":10.25},{"name":"usageBasicPackageCredit","package_credit_amount":20,"package_credit_used":9.75,"package_credit_remain":10.25}]}`)
		case EpBenefitBalance:
			if r.Header.Get("Agent-Type") != "" {
				t.Error("unexpected benefit routing header")
			}
			io.WriteString(w, `{"error_code":"0000","result":{"total_quota":1000,"total_balance":800,"used_amount":200}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	c := New(time.Second)
	c.snapBase, c.benefitBase = server.URL, server.URL
	cred := SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st"}
	credit, err := c.FetchAccountInfo(context.Background(), cred)
	if err != nil || credit.Total != 10.25 || len(credit.Packages) != 1 {
		t.Fatalf("credit=%+v err=%v", credit, err)
	}
	benefit, err := c.FetchBenefitBalance(context.Background(), cred)
	if err != nil || benefit.TotalBalance != 800 {
		t.Fatalf("benefit=%+v err=%v", benefit, err)
	}
}

func TestBalanceMissingFieldsAreNotZero(t *testing.T) {
	for _, tc := range []struct {
		body      string
		benefit   bool
		wantError bool
	}{
		{`{"package":{},"metrics":[]}`, false, false},
		{`{"package":{},"metrics":[{"name":"usageTotalPackageCredit"}]}`, false, true},
		{`{"error_code":"4000","result":{}}`, true, true},
		{`{"error_code":"0000","result":{}}`, true, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }))
			defer s.Close()
			c := New(time.Second)
			c.snapBase, c.benefitBase = s.URL, s.URL
			var err error
			if tc.benefit {
				_, err = c.FetchBenefitBalance(context.Background(), SignCredential{})
			} else {
				value, e := c.FetchAccountInfo(context.Background(), SignCredential{})
				err = e
				if !tc.wantError && value != nil {
					t.Fatal("unknown credit became zero")
				}
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("error %v", err)
			}
		})
	}
}
