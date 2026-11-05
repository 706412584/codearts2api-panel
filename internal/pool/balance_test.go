package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

type balanceStub struct {
	mu        sync.Mutex
	creditErr error
	unknown   bool
}

func (s *balanceStub) FetchAccountInfo(context.Context, upstream.SignCredential) (*upstream.CreditBalance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creditErr != nil {
		return nil, s.creditErr
	}
	if s.unknown {
		return nil, nil
	}
	return &upstream.CreditBalance{Total: 12.75, Packages: []upstream.CreditPackage{}}, nil
}

type balanceObserverStub struct{ totals []float64 }

func (o *balanceObserverStub) Add(_ string, credits float64) bool {
	o.totals = append(o.totals, credits)
	return true
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

type activityBalanceStub struct {
	calls   chan struct{}
	release chan struct{}
}

func (s *activityBalanceStub) FetchAccountInfo(context.Context, upstream.SignCredential) (*upstream.CreditBalance, error) {
	s.calls <- struct{}{}
	<-s.release
	return &upstream.CreditBalance{Total: 99}, nil
}

func TestNoteSuccessRefreshesStaleBalanceWithoutDuplicateRequests(t *testing.T) {
	a := auth.New("a", "a", "", "", "ak", "sk", "", "", "")
	client := &activityBalanceStub{calls: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	p, err := New([]*auth.Auth{a}, Config{BalanceClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.NoteSuccess("a")
	select {
	case <-client.calls:
	case <-time.After(time.Second):
		t.Fatal("successful request did not refresh the missing balance cache")
	}
	p.NoteSuccess("a")
	select {
	case <-client.calls:
		t.Fatal("in-flight refresh was duplicated")
	default:
	}
	client.release <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for {
		acct := p.Get("a")
		acct.mu.Lock()
		done := acct.creditBalance != nil && !acct.balanceRefreshing
		acct.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("asynchronous balance refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	p.NoteSuccess("a")
	select {
	case <-client.calls:
		t.Fatal("fresh balance should not be refreshed")
	default:
	}
	acct := p.Get("a")
	acct.mu.Lock()
	acct.balanceUpdatedAt = time.Now().Add(-6 * time.Minute)
	acct.mu.Unlock()
	p.NoteSuccess("a")
	select {
	case <-client.calls:
	case <-time.After(time.Second):
		t.Fatal("stale balance was not refreshed after successful request")
	}
	client.release <- struct{}{}
}

func TestBalanceObserverSkipsFailuresAndUnknownTotals(t *testing.T) {
	a := auth.New("a", "a", "", "", "ak", "sk", "", "", "")
	stub := &balanceStub{}
	observer := &balanceObserverStub{}
	p, err := New([]*auth.Auth{a}, Config{BalanceClient: stub, BalanceObserver: observer}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.RefreshBalance(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if len(observer.totals) != 1 || observer.totals[0] != 12.75 {
		t.Fatalf("fresh decimal total not observed: %v", observer.totals)
	}
	stub.mu.Lock()
	stub.creditErr = errors.New("unavailable")
	stub.mu.Unlock()
	_, _ = p.RefreshBalance(context.Background(), "a")
	stub.mu.Lock()
	stub.creditErr, stub.unknown = nil, true
	stub.mu.Unlock()
	if out, err := p.RefreshBalance(context.Background(), "a"); err != nil || out.CreditBalance != nil {
		t.Fatalf("unknown balance result = %+v, %v", out, err)
	}
	if len(observer.totals) != 1 {
		t.Fatalf("cached failure or unknown total was sampled: %v", observer.totals)
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
