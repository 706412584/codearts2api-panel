// Package scheduler 提供 token 自动续期和活动积分定时领取。
//
// 两条循环彼此独立：
//   - token 看护：按 PollInterval 轮询，校验登录态、临近过期主动续期、保活；
//   - 积分签到：只在 CheckinHours 指定的本地整点执行（默认 09:00 / 21:00），
//     与轮询间隔无关 —— 轮询是「多久看一次 token」，签到是「每天几点领」。
package scheduler

import (
	"context"
	"log"
	"time"

	"codearts2api-panel/internal/pool"
)

// Config 调度器配置。
type Config struct {
	Pool              *pool.Pool
	Enabled           bool
	CreditsAutoClaim  bool          // 在 CheckinHours 时点领取每日签到/新手积分
	CheckinHours      []int         // 每日签到时点（本地小时 0-23），空值回落 [9, 21]
	PollInterval      time.Duration // token 轮询间隔，默认 30m
	RefreshSkew       time.Duration // 到期前多久刷新，默认 30m
	KeepaliveInterval time.Duration // 保活心跳间隔，默认 15m
}

// Scheduler 定时任务。
type Scheduler struct {
	cfg Config
	// wait 等待到指定墙钟时刻，返回 false 表示 ctx 取消。测试注入即时返回。
	wait func(context.Context, time.Time) bool
}

// New 构造调度器。
func New(cfg Config) *Scheduler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = 15 * time.Minute
	}
	cfg.CheckinHours = normalizeHours(cfg.CheckinHours)
	return &Scheduler{cfg: cfg, wait: sleepUntil}
}

// Run 启动定时循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		log.Printf("token watchdog disabled")
		return
	}
	if !s.cfg.CreditsAutoClaim {
		log.Printf("credit checkin disabled (credits_auto_claim=false)")
		s.runTokenLoop(ctx)
		return
	}
	go s.runTokenLoop(ctx)
	// 启动补领：今天已经过了最近一个时点却还没签时补一次，避免一整天没领到。
	s.catchUpCheckin(ctx, time.Now())
	s.runCheckinLoop(ctx)
}

// runTokenLoop 校验启用账号（自动续期 + 保活），阻塞直到 ctx 取消。
func (s *Scheduler) runTokenLoop(ctx context.Context) {
	log.Printf("token watchdog enabled: poll=%s refresh_skew=%s keepalive=%s",
		s.cfg.PollInterval, s.cfg.RefreshSkew, s.cfg.KeepaliveInterval)

	// 立即执行一次
	s.Tick(ctx)

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick 校验启用账号（自动续期 + 保活）。
//
// 签到**不再**挂在这里：轮询间隔（默认 30 分钟）与「每天几点」是两件事，
// 挂着跑等于一天领几十次上游活动列表。签到由 runCheckinLoop 按时点触发。
func (s *Scheduler) Tick(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !s.cfg.Pool.Enabled(acct.Name) {
			continue
		}
		ok, _ := s.cfg.Pool.Validate(acct)
		if !ok {
			log.Printf("token account=%s invalid/disabled", acct.Name)
			continue
		}
		remaining := acct.Auth.Remaining().Round(time.Minute)
		log.Printf("token account=%s ok remaining=%s", acct.Name, remaining)

		// ticket 登录不返回 refresh_token；这种账号跳过无意义的周期刷新。
		if acct.Auth.Refresh() == "" {
			continue
		}
		if err := s.cfg.Pool.CheckAndRefreshTokenWithin(acct.Name, s.cfg.RefreshSkew); err != nil {
			log.Printf("proactive refresh failed account=%s err=%v", acct.Name, err)
		} else if remaining <= s.cfg.RefreshSkew {
			log.Printf("token refreshed account=%s new_remaining=%s", acct.Name,
				acct.Auth.Remaining().Round(time.Minute))
		}
	}
}
