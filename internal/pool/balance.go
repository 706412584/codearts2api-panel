package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"codearts2api-panel/internal/upstream"
)

type BalanceClient interface {
	FetchAccountInfo(context.Context, upstream.SignCredential) (*upstream.CreditBalance, error)
	FetchBenefitBalance(context.Context, upstream.SignCredential) (*upstream.BenefitBalance, error)
}

type BalanceResult struct {
	UID            string                   `json:"uid"`
	OK             bool                     `json:"ok"`
	CreditBalance  *upstream.CreditBalance  `json:"credit_balance"`
	BenefitBalance *upstream.BenefitBalance `json:"benefit_balance"`
	UpdatedAt      string                   `json:"balance_updated_at"`
	CreditError    string                   `json:"credit_balance_error"`
	BenefitError   string                   `json:"benefit_balance_error"`
}

func optionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// RefreshBalance updates the two ledgers independently and retains successful
// cached data when one upstream is unavailable. UI polling reads List only.
func (p *Pool) RefreshBalance(ctx context.Context, uid string) (BalanceResult, error) {
	a := p.Get(uid)
	if a == nil {
		return BalanceResult{UID: uid}, fmt.Errorf("account not found")
	}
	a.balanceMu.Lock()
	defer a.balanceMu.Unlock()
	if err := ctx.Err(); err != nil {
		return BalanceResult{UID: uid}, err
	}
	if ok, err := p.Validate(a); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("token invalid; re-login required")
		}
		a.mu.Lock()
		a.creditBalanceError, a.benefitBalanceError = err.Error(), err.Error()
		a.balanceUpdatedAt = time.Now()
		out := BalanceResult{UID: uid, CreditBalance: a.creditBalance, BenefitBalance: a.benefitBalance, UpdatedAt: optionalTime(a.balanceUpdatedAt), CreditError: err.Error(), BenefitError: err.Error()}
		a.mu.Unlock()
		return out, err
	}
	token, ak, sk := a.Auth.Credentials()
	cred := upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk}
	client := p.cfg.BalanceClient
	if client == nil {
		client = a.Client
	}
	var credit *upstream.CreditBalance
	var benefit *upstream.BenefitBalance
	var creditErr, benefitErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); credit, creditErr = client.FetchAccountInfo(ctx, cred) }()
	go func() { defer wg.Done(); benefit, benefitErr = client.FetchBenefitBalance(ctx, cred) }()
	wg.Wait()
	a.mu.Lock()
	if a.removed {
		a.mu.Unlock()
		return BalanceResult{UID: uid}, fmt.Errorf("account removed")
	}
	if creditErr == nil {
		a.creditBalance = credit
	}
	if benefitErr == nil {
		a.benefitBalance = benefit
	}
	a.creditBalanceError, a.benefitBalanceError = "", ""
	if creditErr != nil {
		a.creditBalanceError = creditErr.Error()
	}
	if benefitErr != nil {
		a.benefitBalanceError = benefitErr.Error()
	}
	a.balanceUpdatedAt = time.Now()
	out := BalanceResult{UID: uid, OK: creditErr == nil && benefitErr == nil, CreditBalance: a.creditBalance, BenefitBalance: a.benefitBalance, UpdatedAt: optionalTime(a.balanceUpdatedAt), CreditError: a.creditBalanceError, BenefitError: a.benefitBalanceError}
	a.mu.Unlock()
	p.saveState()
	return out, errors.Join(creditErr, benefitErr)
}

// SetBenefitAutoClaim applies the configured write policy to current and future accounts.
func (p *Pool) SetBenefitAutoClaim(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.BenefitAutoClaim = enabled
	for _, a := range p.accounts {
		a.Client.SetBenefitAutoClaim(enabled)
	}
}

// Remove deletes the credential before removing the account from the pool.
// Existing requests retain a valid pointer, but cannot save it again.
func (p *Pool) Remove(uid string) error {
	p.mu.Lock()
	index := -1
	for i, a := range p.accounts {
		if a.UID == uid || a.Name == uid {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		return fmt.Errorf("account not found")
	}
	a := p.accounts[index]
	a.mu.Lock()
	if err := a.Auth.RemoveFile(); err != nil {
		a.mu.Unlock()
		p.mu.Unlock()
		return fmt.Errorf("remove credential: %w", err)
	}
	a.removed, a.disabled = true, true
	a.mu.Unlock()
	p.accounts = append(p.accounts[:index], p.accounts[index+1:]...)
	p.mu.Unlock()
	if err := p.saveStateErr(); err != nil {
		return fmt.Errorf("account removed, state persistence failed: %w", err)
	}
	return nil
}
