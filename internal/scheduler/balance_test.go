package scheduler

import (
	"context"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/balance"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

type schedulerBalanceClient struct{ calls int }

func (c *schedulerBalanceClient) FetchAccountInfo(context.Context, upstream.SignCredential) (*upstream.CreditBalance, error) {
	c.calls++
	return &upstream.CreditBalance{Total: 12.75, Packages: []upstream.CreditPackage{}}, nil
}

func TestRunBalanceRefreshNowSamplesAndSkipsDisabled(t *testing.T) {
	a := auth.New("balance-u", "user", "", "", "ak", "sk", "", "", "")
	client := &schedulerBalanceClient{}
	recorder := balance.New("")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{BalanceClient: client, BalanceObserver: recorder}, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Pool: p, Enabled: false}) // 余额趋势不依赖 token 看护开关。
	s.RunBalanceRefreshNow(context.Background())
	s.RunBalanceRefreshNow(context.Background())
	if client.calls != 2 {
		t.Fatalf("balance refreshes = %d, want 2", client.calls)
	}
	series, _ := recorder.Snapshot(time.Time{})
	if len(series) != 1 || len(series[0].Points) != 1 || series[0].Current != 12.75 {
		t.Fatalf("unexpected samples: %+v", series)
	}
	p.Disable("balance-u", "manual")
	s.RunBalanceRefreshNow(context.Background())
	if client.calls != 2 {
		t.Fatalf("disabled account was refreshed: %d calls", client.calls)
	}
}
