package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

type failingCheckinClient struct{ calls int }

func (f *failingCheckinClient) Checkin(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
	f.calls++
	return upstream.CheckinResult{}, errors.New("temporary activity failure")
}

type countingCheckinClient struct{ calls int }

func (c *countingCheckinClient) Checkin(context.Context, upstream.SignCredential, upstream.CheckinOptions) (upstream.CheckinResult, error) {
	c.calls++
	return upstream.CheckinResult{OK: true, Daily: upstream.ClaimOutcomeClaimed, NewUser: upstream.ClaimOutcomeUnavailable}, nil
}

func newTestPool(t *testing.T, client pool.CheckinClient) *pool.Pool {
	t.Helper()
	a := auth.New("u1", "user", "domain", "token", "ak", "sk", time.Now().Add(2*time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{CheckinClient: client}, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Token 轮询不再顺带领取积分：签到按 checkin_hours 的时点走。
func TestTickLeavesCheckinToSchedule(t *testing.T) {
	c := &countingCheckinClient{}
	s := New(Config{Pool: newTestPool(t, c), CreditsAutoClaim: true})
	s.Tick(context.Background())
	s.Tick(context.Background())
	if c.calls != 0 {
		t.Fatalf("token poll claimed credits %d times", c.calls)
	}
}

// 每个时点都会触发一次领取；同一天内多次触发由账号池账本收敛成一次真实领取。
func TestCheckinLoopFiresAtEachSlot(t *testing.T) {
	c := &countingCheckinClient{}
	s := New(Config{Pool: newTestPool(t, c), Enabled: true, CreditsAutoClaim: true, CheckinHours: []int{9, 21}})
	waits := 0
	s.wait = func(context.Context, time.Time) bool {
		waits++
		return waits <= 3
	}
	s.runCheckinLoop(context.Background())
	if waits != 4 {
		t.Fatalf("waits = %d, want 4 (three fired slots + one cancel)", waits)
	}
	// 时点照触发，但今天的签到已经了结：后续时点在池内短路，不再打上游。
	if c.calls != 1 {
		t.Fatalf("claims = %d, want 1 (same-day slots must not re-claim)", c.calls)
	}
}

// 时点触发失败不该让循环退出：下一次时点照常尝试。
func TestCheckinLoopSurvivesFailure(t *testing.T) {
	c := &failingCheckinClient{}
	s := New(Config{Pool: newTestPool(t, c), Enabled: true, CreditsAutoClaim: true, CheckinHours: []int{9}})
	waits := 0
	s.wait = func(context.Context, time.Time) bool {
		waits++
		return waits <= 2
	}
	s.runCheckinLoop(context.Background())
	if c.calls != 2 {
		t.Fatalf("calls = %d, want 2 (one per fired slot)", c.calls)
	}
}

// 启动补领只在「今天已经过了某个时点」时发生：08:00 启动不抢跑，10:00 启动立刻补。
func TestCatchUpOnlyAfterPassedSlot(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name  string
		now   time.Time
		hours []int
		want  int
	}{
		{name: "before first slot", now: day.Add(8 * time.Hour), hours: []int{9, 21}, want: 0},
		{name: "after first slot", now: day.Add(10 * time.Hour), hours: []int{9, 21}, want: 1},
		{name: "after last slot", now: day.Add(22 * time.Hour), hours: []int{9, 21}, want: 1},
		{name: "before single slot", now: day.Add(5 * time.Hour), hours: []int{9}, want: 0},
		{name: "midnight start", now: day.Add(10 * time.Minute), hours: []int{9, 21}, want: 0},
		{name: "single slot passed", now: day.Add(23 * time.Hour), hours: []int{9}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &countingCheckinClient{}
			s := New(Config{Pool: newTestPool(t, c), CreditsAutoClaim: true, CheckinHours: tc.hours})
			s.catchUpCheckin(context.Background(), tc.now)
			if c.calls != tc.want {
				t.Fatalf("catch-up claims = %d, want %d", c.calls, tc.want)
			}
		})
	}
}

func TestNextFireSkipsCurrentHour(t *testing.T) {
	loc := time.Local
	nine := time.Date(2026, 11, 1, 9, 0, 0, 0, loc)
	if got := nextFire(nine, []int{9, 21}); !got.Equal(nine.Add(12 * time.Hour)) {
		t.Fatalf("nextFire at exactly 09:00 = %v, want 21:00", got)
	}
	if got := nextFire(nine.Add(time.Minute), []int{9, 21}); !got.Equal(time.Date(2026, 11, 1, 21, 0, 0, 0, loc)) {
		t.Fatalf("nextFire at 09:01 = %v, want 21:00", got)
	}
	if got := nextFire(time.Date(2026, 11, 1, 23, 0, 0, 0, loc), []int{9, 21}); !got.Equal(time.Date(2026, 11, 2, 9, 0, 0, 0, loc)) {
		t.Fatalf("nextFire at 23:00 = %v, want tomorrow 09:00", got)
	}
}

func TestLastPassedFire(t *testing.T) {
	loc := time.Local
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	if got := lastPassedFire(day.Add(8*time.Hour), []int{9, 21}); !got.IsZero() {
		t.Fatalf("lastPassedFire before first slot = %v, want zero", got)
	}
	if got := lastPassedFire(day.Add(9*time.Hour), []int{9, 21}); !got.Equal(day.Add(9 * time.Hour)) {
		t.Fatalf("lastPassedFire at 09:00 = %v, want 09:00", got)
	}
	if got := lastPassedFire(day.Add(23*time.Hour), []int{9, 21}); !got.Equal(day.Add(21 * time.Hour)) {
		t.Fatalf("lastPassedFire at 23:00 = %v, want 21:00", got)
	}
}

func TestNormalizeHours(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []int
		want []int
	}{
		{name: "default", in: nil, want: []int{9, 21}},
		{name: "dedupe and sort", in: []int{21, 9, 21}, want: []int{9, 21}},
		{name: "filter invalid", in: []int{-1, 24, 7, 7}, want: []int{7}},
		{name: "all invalid falls back", in: []int{99, -3}, want: []int{9, 21}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeHours(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("normalizeHours(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("normalizeHours(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
	// 返回值必须是副本：改它可以，但不能污染默认时点列表。
	got := normalizeHours(nil)
	got[0] = -5
	if again := normalizeHours(nil); again[0] != 9 {
		t.Fatalf("default hours were mutated: %v", again)
	}
}

// 取消上下文时 sleepUntil 必须立刻返回 false，优雅停机不能等到时点。
func TestSleepUntilCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepUntil(ctx, time.Now().Add(time.Hour)) {
		t.Fatal("cancelled context must not report a fired slot")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
	if !sleepUntil(context.Background(), time.Now().Add(-time.Second)) {
		t.Fatal("past target must fire immediately")
	}
}

// 关闭领取后不再产生任何上游请求。
func TestRunWithoutCreditsAutoClaimSkipsCheckin(t *testing.T) {
	c := &countingCheckinClient{}
	s := New(Config{Pool: newTestPool(t, c), Enabled: true, CreditsAutoClaim: false})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	if c.calls != 0 {
		t.Fatalf("disabled credits still claimed %d times", c.calls)
	}
}

// 定时入口与手动入口共用 RunCheckinNow：禁用账号不参与。
func TestRunCheckinNowSkipsDisabledAccounts(t *testing.T) {
	c := &countingCheckinClient{}
	p := newTestPool(t, c)
	s := New(Config{Pool: p, CreditsAutoClaim: true})
	s.RunCheckinNow(context.Background())
	if c.calls != 1 {
		t.Fatalf("calls = %d, want 1", c.calls)
	}
	p.Disable("u1", "manual")
	s.RunCheckinNow(context.Background())
	if c.calls != 1 {
		t.Fatalf("disabled account was claimed: calls = %d", c.calls)
	}
}

// Reconfigure 热更新：开关、间隔、签到时点立即换成新值，账号池句柄保持不变。
func TestReconfigureHotUpdatesSchedule(t *testing.T) {
	p := newTestPool(t, &countingCheckinClient{})
	s := New(Config{Pool: p, Enabled: false, CheckinHours: []int{9}})
	s.Reconfigure(Config{
		Pool:              nil, // 不接受替换
		Enabled:           true,
		CreditsAutoClaim:  true,
		CheckinHours:      []int{10, 22},
		PollInterval:      5 * time.Minute,
		RefreshSkew:       90 * time.Minute,
		KeepaliveInterval: 20 * time.Minute,
	})
	cfg := s.config()
	if !cfg.Enabled || !cfg.CreditsAutoClaim {
		t.Fatalf("flags not applied: %+v", cfg)
	}
	if len(cfg.CheckinHours) != 2 || cfg.CheckinHours[0] != 10 || cfg.CheckinHours[1] != 22 {
		t.Fatalf("checkin hours = %v", cfg.CheckinHours)
	}
	if cfg.PollInterval != 5*time.Minute || cfg.RefreshSkew != 90*time.Minute || cfg.KeepaliveInterval != 20*time.Minute {
		t.Fatalf("durations not applied: %+v", cfg)
	}
	if cfg.Pool != p {
		t.Fatal("Reconfigure 不得替换账号池句柄")
	}
}

// 热关掉 credits_auto_claim 后签到时点不再触发领取（循环空转等待，不退出）。
func TestReconfigureStopsCheckinWhenDisabled(t *testing.T) {
	c := &countingCheckinClient{}
	s := New(Config{Pool: newTestPool(t, c), Enabled: true, CreditsAutoClaim: true, CheckinHours: []int{9}})
	ctx, cancel := context.WithCancel(context.Background())
	s.Reconfigure(Config{Pool: s.config().Pool, Enabled: true, CreditsAutoClaim: false, CheckinHours: []int{9}})
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	s.runCheckinLoop(ctx)
	if c.calls != 0 {
		t.Fatalf("disabled credits still claimed %d times", c.calls)
	}
}
