package main

import (
	"time"

	"codearts2api-panel/internal/livecfg"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/reqlog"
	"codearts2api-panel/internal/scheduler"
)

// restartRequiredFields 面板保存后仍需重启进程才能生效的字段。
//
// 这四项都绑定在装配期对象上：监听地址在 http.Server.Addr，凭据目录/状态文件在
// 启动时被 auth.LoadDir 和 pool.New 解析成句柄，上游超时在 http.Client 里。
// 其余字段由 livecfg 快照 / pool setter / scheduler.Reconfigure 热生效。
func restartRequiredFields() []string {
	return []string{"listen", "auth_dir", "state_file", "upstream.timeout_seconds"}
}

// liveSnapshot 把文件配置转换成请求期快照（与 handler 读取口径一一对应）。
func liveSnapshot(c *Config) livecfg.Snapshot {
	delay := 10 * time.Second
	if c.QueueRetrySeconds > 0 {
		delay = time.Duration(c.QueueRetrySeconds) * time.Second
	}
	return livecfg.Snapshot{
		APIKey:            c.APIKey,
		AdminKey:          c.AdminKey,
		DefaultModel:      c.DefaultModel,
		SoftCooldown:      c.SoftRateDur,
		ErrThreshold:      c.Cooldown.ErrThresh,
		ErrCooldown:       c.ErrCooldownDur,
		QueueRetryDelay:   delay,
		QueueMaxAttempts:  c.QueueMaxAttempts,
		OAuthCallbackHost: c.OAuthCallbackHost,
		LoginClientID:     c.LoginClientID,

		WatchEnabled:             c.Watch.Enabled,
		CreditsAutoClaim:         c.CreditsAutoClaim,
		PollMinutes:              c.Watch.PollMinutes,
		RefreshSkewMinutes:       c.Watch.RefreshSkewM,
		KeepaliveIntervalMinutes: c.Watch.KeepaliveInterval,
		MaxConcurrent:            c.MaxConcurrent,
		KeepaliveWindow:          c.KeepaliveWindow,
	}
}

// keepaliveWindow 保活窗口，解析失败回落 10 分钟（与 pool 默认一致）。
func keepaliveWindow(c *Config) time.Duration {
	if c.KeepaliveWindow == "" {
		return 10 * time.Minute
	}
	dur, err := time.ParseDuration(c.KeepaliveWindow)
	if err != nil || dur <= 0 {
		return 10 * time.Minute
	}
	return dur
}

// applyHotConfig 立即应用可热更新的字段，返回仍需重启的字段列表。
func applyHotConfig(c *Config, live *livecfg.Holder, p *pool.Pool, sch *scheduler.Scheduler, logs *reqlog.Recorder) []string {
	live.Store(liveSnapshot(c))
	p.SetMaxConcurrentAll(c.MaxConcurrent)
	p.SetKeepaliveWindow(keepaliveWindow(c))
	p.SetLoginClientID(c.LoginClientID)
	// 归档清理在后台每秒一轮，这里只换天数，新值一秒内生效。
	logs.SetRetentionDays(c.RequestLogRetentionDays)
	sch.Reconfigure(scheduler.Config{
		Pool:              p,
		Enabled:           c.Watch.Enabled,
		CreditsAutoClaim:  c.CreditsAutoClaim,
		CheckinHours:      c.CheckinHours,
		PollInterval:      time.Duration(c.Watch.PollMinutes) * time.Minute,
		RefreshSkew:       time.Duration(c.Watch.RefreshSkewM) * time.Minute,
		KeepaliveInterval: time.Duration(c.Watch.KeepaliveInterval) * time.Minute,
	})
	return restartRequiredFields()
}
