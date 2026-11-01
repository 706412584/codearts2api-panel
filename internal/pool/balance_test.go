package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

type balanceStub struct {
	mu         sync.Mutex
	creditErr  error
}

func (s *balanceStub) FetchAccountInfo(context.Context, upstream.SignCredential) (*upstream.CreditBalance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creditErr != nil {
		return nil, s.creditErr
	}
	return &upstream.CreditBalance{Total: 12.75, Packages: []upstream.CreditPackage{}}, nil
}

func TestBalanceCachePartialFailureAndPersistentTotals(t *testing.T) {
	dir := t.TempDir()
	a := auth.New("a", "a", "", "", "ak", "sk", "", "", "")
	if err := auth.SaveNew(dir, a); err != nil {
		t.Fatal(err)
	}
	stub := &balanceStub{}
	state := filepath.Join(dir, "data", "state.json")
	p, err := New([]*auth.Auth{a}, Config{BalanceClient: stub}, state)
	if err != nil {
		t.Fatal(err)
	}
	if p.List()[0]["credit_balance"] != (*upstream.CreditBalance)(nil) {
		t.Fatal("unqueried credit was populated")
	}
	out, err := p.RefreshBalance(context.Background(), "a")
	if err != nil || !out.OK || out.CreditBalance == nil || out.CreditBalance.Total != 12.75 {
		t.Fatalf("result %+v, %v", out, err)
	}
	_, _, _, _, total := p.Stats()
	if total != 12.75 {
		t.Fatalf("total=%v", total)
	}
	stub.mu.Lock()
	stub.creditErr = errors.New("credit unavailable")
	stub.mu.Unlock()
	out, err = p.RefreshBalance(context.Background(), "a")
	if err == nil || out.CreditBalance.Total != 12.75 || out.CreditError == "" {
		t.Fatalf("cached result %+v, %v", out, err)
	}
	p2, err := New([]*auth.Auth{a}, Config{}, state)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, total = p2.Stats()
	if total != 12.75 {
		t.Fatal("known total was lost on restart")
	}
}

func TestRemovePersistsStateAndCannotResurrectCredential(t *testing.T) {
	dir := t.TempDir()
	a := auth.New("a", "a", "", "", "ak", "sk", "", "", "")
	if err := auth.SaveNew(dir, a); err != nil {
		t.Fatal(err)
	}
	p, err := New([]*auth.Auth{a}, Config{}, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if len(p.Accounts()) != 0 {
		t.Fatal("account remained in pool")
	}
	if _, err := os.Stat(filepath.Join(dir, a.FileName())); !os.IsNotExist(err) {
		t.Fatal("credential remained on disk")
	}
	if err := a.Save(); err == nil {
		t.Fatal("in-flight handle recreated credential")
	}
	if err := p.Remove("a"); err == nil {
		t.Fatal("missing account removal was reported successful")
	}
}
