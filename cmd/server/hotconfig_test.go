package main

import (
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/livecfg"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/scheduler"
)

// 只有装配期依赖的四个字段需要重启，其余都必须热生效。
func TestRestartRequiredFieldsAreAssemblyTimeOnly(t *testing.T) {
	want := []string{"listen", "auth_dir", "state_file", "upstream.timeout_seconds"}
	got := restartRequiredFields()
	if len(got) != len(want) {
		t.Fatalf("restart fields = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart fields = %v, want %v", got, want)
		}
	}
}

// 保存后热应用：快照、并发上限、调度参数一起换新，且不清空密码类字段。
func TestApplyHotConfigUpdatesLiveAndPool(t *testing.T) {
	c := Default()
	c.APIKey = "api-2"
	c.AdminKey = "admin-2"
	c.DefaultModel = "model-hot"
	c.SoftRateDur = 90 * time.Second
	c.ErrCooldownDur = 15 * time.Minute
	c.Cooldown.ErrThresh = 4
	c.QueueRetrySeconds = 3
	c.QueueMaxAttempts = 7
	c.KeepaliveWindow = "5m"
	c.MaxConcurrent = 2
	c.LoginClientID = "client-hot"
	c.OAuthCallbackHost = "https://example.com/cb"

	p, err := pool.New(nil, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.AddAccount(auth.New("u1", "n1", "d1", "tok", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""))
	sch := scheduler.New(scheduler.Config{Pool: p, Enabled: true, PollInterval: time.Hour})
	live := livecfg.New(livecfg.Snapshot{APIKey: "api-1", DefaultModel: "model-old"})

	restart := applyHotConfig(c, live, p, sch)
	if len(restart) != 4 {
		t.Fatalf("hot apply must report four restart fields, got %v", restart)
	}

	snap := live.Load()
	if snap.APIKey != "api-2" || snap.AdminKey != "admin-2" || snap.DefaultModel != "model-hot" {
		t.Fatalf("live snapshot not applied: %+v", snap)
	}
	if snap.QueueRetryDelay != 3*time.Second || snap.QueueMaxAttempts != 7 {
		t.Fatalf("queue settings not applied: %+v", snap)
	}
	if snap.SoftCooldown != 90*time.Second || snap.ErrCooldown != 15*time.Minute || snap.ErrThreshold != 4 {
		t.Fatalf("cooldown settings not applied: %+v", snap)
	}
	if snap.LoginClientID != "client-hot" || snap.OAuthCallbackHost != "https://example.com/cb" {
		t.Fatalf("oauth settings not applied: %+v", snap)
	}
	for _, row := range p.List() {
		if row["max_concurrent"] != 2 {
			t.Fatalf("pool max_concurrent not hot-updated: %+v", row)
		}
	}
}

// queue_retry_seconds 为 0（不配置）时沿用 10s 默认，与启动路径同口径。
func TestLiveSnapshotQueueRetryDefaultsToTenSeconds(t *testing.T) {
	c := Default()
	snap := liveSnapshot(c)
	if snap.QueueRetryDelay != 10*time.Second {
		t.Fatalf("queue retry delay = %s, want 10s", snap.QueueRetryDelay)
	}
	c.QueueRetrySeconds = 4
	if got := liveSnapshot(c).QueueRetryDelay; got != 4*time.Second {
		t.Fatalf("queue retry delay = %s, want 4s", got)
	}
}

