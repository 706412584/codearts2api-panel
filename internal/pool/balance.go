package pool

import (
	"context"
	"fmt"
	"time"

	"codearts2api-panel/internal/upstream"
)

type BalanceClient interface {
	FetchAccountInfo(context.Context, upstream.SignCredential) (*upstream.CreditBalance, error)
}

// BalanceObserver 接收每次真实查询成功的新鲜余额；手动与后台刷新共用此入口。
type BalanceObserver interface {
	Add(uid string, credits float64) bool
}

type BalanceResult struct {
	UID            string                   `json:"uid"`
	OK             bool                     `json:"ok"`
	CreditBalance  *upstream.CreditBalance  `json:"credit_balance"`
	UpdatedAt      string                   `json:"balance_updated_at"`
	CreditError    string                   `json:"credit_balance_error"`
}

func optionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// RefreshBalance updates the CREDIT ledger. UI polling reads List only.
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
		a.creditBalanceError = err.Error()
		a.balanceUpdatedAt = time.Now()
		out := BalanceResult{UID: uid, CreditBalance: a.creditBalance, UpdatedAt: optionalTime(a.balanceUpdatedAt), CreditError: err.Error()}
		a.mu.Unlock()
		return out, err
	}
	token, ak, sk := a.Auth.Credentials()
	cred := upstream.SignCredential{SecurityToken: token, AccessKeyID: ak, SecretAccessKey: sk}
	client := p.cfg.BalanceClient
	if client == nil {
		client = a.Client
	}
	credit, creditErr := client.FetchAccountInfo(ctx, cred)
	a.mu.Lock()
	if a.removed {
		a.mu.Unlock()
		return BalanceResult{UID: uid}, fmt.Errorf("account removed")
	}
	if creditErr == nil {
		a.creditBalance = credit
	}
	a.creditBalanceError = ""
	if creditErr != nil {
		a.creditBalanceError = creditErr.Error()
	}
	a.balanceUpdatedAt = time.Now()
	out := BalanceResult{UID: uid, OK: creditErr == nil, CreditBalance: a.creditBalance, UpdatedAt: optionalTime(a.balanceUpdatedAt), CreditError: a.creditBalanceError}
	a.mu.Unlock()
	p.saveState()
	// nil 余额表示上游没有 CREDIT 指标，是未知而不是 0；失败时 out 里的缓存也不能采样。
	if creditErr == nil && credit != nil && p.cfg.BalanceObserver != nil {
		p.cfg.BalanceObserver.Add(uid, credit.Total)
	}
	return out, creditErr
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
