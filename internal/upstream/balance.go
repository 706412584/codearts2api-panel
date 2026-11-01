package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const EpPackageInfo = "/snap-manager/v1/statistics/plugin"

type CreditPackage struct {
	Name           string   `json:"name"`
	Total          float64  `json:"total"`
	Used           float64  `json:"used"`
	Remaining      float64  `json:"remaining"`
	ExpiringAmount *float64 `json:"expiring_amount,omitempty"`
}

// CreditBalance is the CREDIT ledger.
type CreditBalance struct {
	Total          float64         `json:"total"`
	Packages       []CreditPackage `json:"packages"`
	PackageName    string          `json:"package_name"`
	SpecCode       string          `json:"spec_code"`
	Status         string          `json:"status"`
	EndDate        string          `json:"end_date"`
	ExpiringAmount *float64        `json:"expiring_amount,omitempty"`
}

// balanceGet signs the exact empty payload. PromptCenter routing headers on the
// statistics endpoint follow the separately observed unsigned header variant.
func (c *Client) balanceGet(ctx context.Context, endpoint string, cred SignCredential, promptCenter bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	signRequest(req, nil, cred)
	if promptCenter {
		req.Header.Set("Agent-Type", "PromptCenter")
		req.Header.Set("X-Language", "zh-cn")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Path: req.URL.Path, Message: truncateStr(string(raw), 300)}
	}
	return raw, nil
}

// FetchAccountInfo returns nil when the account does not expose credit metrics.
func (c *Client) FetchAccountInfo(ctx context.Context, cred SignCredential) (*CreditBalance, error) {
	raw, err := c.balanceGet(ctx, c.snapURL(EpPackageInfo), cred, true)
	if err != nil {
		return nil, err
	}
	var response struct {
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_msg"`
		Package      *struct {
			Name        string `json:"package_name_cn"`
			EnglishName string `json:"package_name_en"`
			SpecCode    string `json:"spec_code"`
			Status      string `json:"status"`
		} `json:"package"`
		EndDate string `json:"end_date"`
		Metrics []struct {
			Name     string   `json:"name"`
			Amount   float64  `json:"package_credit_amount"`
			Used     float64  `json:"package_credit_used"`
			Remain   *float64 `json:"package_credit_remain"`
			Expiring *float64 `json:"package_credit_expiring_amount"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("parse package balance: %w", err)
	}
	if response.ErrorCode != "" && response.ErrorCode != "0000" {
		return nil, fmt.Errorf("package balance %s: %s", response.ErrorCode, response.ErrorMessage)
	}
	if response.Package == nil {
		return nil, fmt.Errorf("package balance response missing package")
	}
	out := &CreditBalance{Packages: []CreditPackage{}, PackageName: response.Package.Name, SpecCode: response.Package.SpecCode, Status: response.Package.Status, EndDate: response.EndDate}
	if out.PackageName == "" {
		out.PackageName = response.Package.EnglishName
	}
	var total *float64
	known := false
	for _, m := range response.Metrics {
		if m.Name != "usageTotalPackageCredit" && m.Name != "usageBasicPackageCredit" && m.Name != "usageOnDemandPackageCredit" && m.Name != "usageBonusPackageCredit" {
			continue
		}
		if m.Remain == nil {
			return nil, fmt.Errorf("credit metric missing remaining amount")
		}
		known = true
		if m.Name == "usageTotalPackageCredit" {
			total = m.Remain
			out.ExpiringAmount = m.Expiring
			continue
		}
		out.Packages = append(out.Packages, CreditPackage{Name: m.Name, Total: m.Amount, Used: m.Used, Remaining: *m.Remain, ExpiringAmount: m.Expiring})
		out.Total += *m.Remain
	}
	if !known {
		return nil, nil
	}
	if total != nil {
		out.Total = *total
	}
	return out, nil
}
