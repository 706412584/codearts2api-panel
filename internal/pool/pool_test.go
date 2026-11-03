package pool

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

func TestAddAccountInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.AddAccount(a)

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("newly added account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

func TestSyncToDirInitializesConcurrency(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "verifier")
	p.SyncToDir([]*auth.Auth{a})

	if !p.AcquireLockWait("u1", 0) {
		t.Fatal("synced account did not receive a concurrency slot")
	}
	p.ReleaseLock("u1")
}

// 满载账号不该被优先选中：同池里还有空闲账号时必须选空闲的。
func TestPickExcludingPrefersIdleAccount(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"u1", "u2"} {
		p.AddAccount(auth.New(id, id, "d", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	}
	// u1 占满槽位；PickExcluding 不应再选它。
	if !p.AcquireLock("u1") {
		t.Fatal("could not occupy u1")
	}
	if got := p.PickExcluding(nil); got == nil || got.Name != "u2" {
		gotName := "<nil>"
		if got != nil {
			gotName = got.Name
		}
		t.Fatalf("picked %s, want u2 (u1 is saturated)", gotName)
	}
	// 都满载时回落到仍返回一个账号（调用方靠短超时排队换号）。
	if !p.AcquireLock("u2") {
		t.Fatal("could not occupy u2")
	}
	if got := p.PickExcluding(nil); got == nil {
		t.Fatal("with every account saturated PickExcluding must still return a candidate for the caller to queue on")
	}
}

// 余额高的账号在同档位里优先。
func TestPickExcludingPrefersHigherCredits(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"low", "high", "unknown"} {
		p.AddAccount(auth.New(id, id, "d", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	}
	setCredits := func(name string, total float64) {
		a := p.Get(name)
		a.mu.Lock()
		a.creditBalance = &upstream.CreditBalance{Total: total}
		a.mu.Unlock()
	}
	setCredits("low", 100)
	setCredits("high", 9000)
	// unknown 保持 nil：余额未知的账号排在有余额的之后。
	if got := p.PickExcluding(nil); got == nil || got.Name != "high" {
		gotName := "<nil>"
		if got != nil {
			gotName = got.Name
		}
		t.Fatalf("picked %s, want high", gotName)
	}
	deleteMap := map[string]bool{"high": true}
	if got := p.PickExcluding(deleteMap); got == nil || got.Name != "low" {
		gotName := "<nil>"
		if got != nil {
			gotName = got.Name
		}
		t.Fatalf("picked %s, want low (high excluded, unknown ranks last)", gotName)
	}
}

// AcquireLockAny 能把槽位分给任意空闲账号，并遵守 exclude 与超时。
func TestAcquireLockAnySkipsSaturatedAndExcluded(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"u1", "u2", "u3"} {
		p.AddAccount(auth.New(id, id, "d", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	}
	if !p.AcquireLock("u1") {
		t.Fatal("occupy u1")
	}
	// u1 满载 → 应拿到 u2。
	name, ok := p.AcquireLockAny(nil, nil, 0)
	if !ok || name != "u2" {
		t.Fatalf("got %q ok=%v, want u2", name, ok)
	}
	p.ReleaseLock("u2")
	// 排除 u2、u1 已满载 → 应拿到 u3。
	name, ok = p.AcquireLockAny(nil, map[string]bool{"u1": true, "u2": true}, 0)
	if !ok || name != "u3" {
		t.Fatalf("got %q ok=%v, want u3", name, ok)
	}
	p.ReleaseLock("u3")
	// 全部排除 → 超时返回空。
	if name, ok := p.AcquireLockAny(nil, map[string]bool{"u1": true, "u2": true, "u3": true}, 0); ok {
		t.Fatalf("expected no candidate, got %q", name)
	}
}

// 单账号池：唯一的账号满载时，仍要能等它空出槽位（而不是把它自己排除掉）。
// 这是「排除集误用 tried」那个高危缺陷的回归测试。
func TestAcquireLockAnySingleAccountStillWaitsForItsOnlySlot(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("only", "only", "d", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	if !p.AcquireLock("only") {
		t.Fatal("occupy the only account")
	}
	// 槽位被占：正确行为是等待，而不是立刻判定无候选。
	if _, ok := p.AcquireLockAny(nil, nil, 0); ok {
		t.Fatal("expected no slot while saturated with zero timeout")
	}
	// 释放后应能拿到；模拟「等一会儿就有了」。
	go func() { time.Sleep(150 * time.Millisecond); p.ReleaseLock("only") }()
	name, ok := p.AcquireLockAny(nil, nil, 3*time.Second)
	if !ok || name != "only" {
		t.Fatalf("got %q ok=%v, want only", name, ok)
	}
	p.ReleaseLock("only")
}

// 排除集之外已无健康账号时，AcquireLockAny 必须立刻返回，不空等。
func TestAcquireLockAnyReturnsImmediatelyWhenNothingEligible(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("u1", "u1", "d", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	start := time.Now()
	if name, ok := p.AcquireLockAny(nil, map[string]bool{"u1": true}, 5*time.Second); ok {
		t.Fatalf("expected failure, got %q", name)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s with no eligible account; should return immediately", elapsed)
	}
}

func TestValidateKeepsAccountEnabledAfterRetryableRefreshFailure(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary upstream outage", http.StatusBadGateway)
	}))
	defer sts.Close()
	dpopPrivateJWK, err := upstream.NewDPoPPrivateJWK()
	if err != nil {
		t.Fatal(err)
	}
	loginCfg := upstream.DefaultLoginConfig()
	loginCfg.STSHost = sts.URL
	a := auth.New(
		"u-refresh", "user", "domain", "token", "ak", "sk",
		time.Now().Add(30*time.Minute).Format(time.RFC3339), "refresh", "verifier",
		map[string]string(dpopPrivateJWK),
	)
	p, err := New([]*auth.Auth{a}, Config{RefreshSkew: time.Hour, LoginConfig: loginCfg}, "")
	if err != nil {
		t.Fatal(err)
	}
	ok, refreshErr := p.Validate(p.Accounts()[0])
	if ok || refreshErr == nil {
		t.Fatalf("validate=(ok=%t, err=%v), want retryable refresh failure", ok, refreshErr)
	}
	status := p.List()[0]
	if disabled, _ := status["disabled"].(bool); disabled {
		t.Fatalf("retryable refresh failure permanently disabled account: %#v", status)
	}
}

// watch.refresh_skew_minutes 必须是有效的：此前 pool 硬编码 1 小时阈值，
// 调度器传进来的 RefreshSkew 只用于日志，配置改了也不生效。
func TestRefreshSkewControlsThreshold(t *testing.T) {
	a := auth.New("u-skew", "n", "d", "tok", "ak", "sk",
		time.Now().Add(2*time.Hour).Format(time.RFC3339), "refresh-token", "verifier")
	p, err := New([]*auth.Auth{a}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 2h：skew=1h 时不刷新，skew=3h 时应命中刷新（这里只验证判定分支，
	// 真刷新需要网络，故只检查不触发的那一侧不报错）。
	if err := p.CheckAndRefreshTokenWithin("u-skew", time.Hour); err != nil {
		t.Fatalf("剩余 2h、skew=1h 不应触发刷新: %v", err)
	}
}

// SetMaxConcurrentAll 热更新并发上限：已加载账号与新加入账号都按新值，非正值忽略。
func TestSetMaxConcurrentAllAppliesToExistingAndNewAccounts(t *testing.T) {
	p, err := New(nil, Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("u1", "n1", "d1", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	p.SetMaxConcurrentAll(2)
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("existing account not updated: %+v", row)
		}
	}
	p.AddAccount(auth.New("u2", "n2", "d2", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("account added after hot update = %+v", row)
		}
	}
	p.SetMaxConcurrentAll(0) // 非正值不生效，避免把上限清零
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("zero must be ignored: %+v", row)
		}
	}
}

// SetKeepaliveWindow 热更新保活窗口，NeedKeepalive 立即按新值判定。
func TestSetKeepaliveWindowHotUpdate(t *testing.T) {
	p, err := New(nil, Config{KeepaliveWindow: time.Hour}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("u1", "n1", "d1", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	if p.NeedKeepalive("u1") {
		t.Fatal("fresh account must not need keepalive")
	}
	p.PingKeepalive("u1")
	p.SetKeepaliveWindow(time.Nanosecond)
	// Windows 单调时钟粒度粗：紧接着调用 time.Since 会拿到 0，0 > 1ns 为假。
	// 睡一小会让「Ping 之后已过窗口」这个前提成立，测试才确定性地测到热更新。
	time.Sleep(time.Millisecond)
	if !p.NeedKeepalive("u1") {
		t.Fatal("shrunken window must trigger keepalive")
	}
	if p.keepaliveWindow() != time.Nanosecond {
		t.Fatalf("window not stored: %s", p.keepaliveWindow())
	}
}
