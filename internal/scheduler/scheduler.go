// Package scheduler 提供 token 自动续期、余额采样和活动积分定时领取。
//
// 三条循环彼此独立：
//   - token 看护：按 PollInterval 轮询，校验登录态、临近过期主动续期、保活；
//   - 余额采样：启动即刷新，之后每 30 分钟查询一次启用账号；
//   - 积分签到：只在 CheckinHours 指定的本地整点执行（默认 09:00 / 21:00），
//     与轮询间隔无关 —— 轮询是「多久看一次 token」，签到是「每天几点领」。
package scheduler

import (
	"context"
	"log"
	"sync"
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
//
// cfg 可被面板热更新：读写一律经 mu，循环每轮重新读取，改间隔/时点/开关无需重启。
type Scheduler struct {
	mu   sync.Mutex
	cfg  Config
	wait func(context.Context, time.Time) bool
	// wait 等待到指定墙钟时刻，返回 false 表示 ctx 取消。测试注入即时返回。
}

// New 构造调度器。
func New(cfg Config) *Scheduler {
	return &Scheduler{cfg: withDefaults(cfg), wait: sleepUntil}
}

// withDefaults 补齐零值并规范化签到时点。
func withDefaults(cfg Config) Config {
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
	return cfg
}

// Reconfigure 热更新调度参数：开关、轮询/刷新/保活间隔、签到时点。
// Pool 句柄不接受替换（账号池由进程装配），沿用当前值。
func (s *Scheduler) Reconfigure(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.Pool = s.cfg.Pool
	s.cfg = withDefaults(cfg)
}

// config 返回当前配置快照。
func (s *Scheduler) config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// sleepCtx 等待 d 或 ctx 取消（返回 false）。
func (s *Scheduler) sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Minute
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Run 启动定时循环，阻塞直到 ctx 取消。
//
// 三个循环常驻；token/签到每轮重新读配置：watch.enabled / credits_auto_claim 关掉后
// 对应循环只是空转等待，重新打开即恢复。余额采样独立运行，不受这两个开关控制。
func (s *Scheduler) Run(ctx context.Context) {
	if cfg := s.config(); cfg.Enabled {
		log.Printf("token watchdog enabled: poll=%s refresh_skew=%s keepalive=%s",
			cfg.PollInterval, cfg.RefreshSkew, cfg.KeepaliveInterval)
	} else {
		log.Printf("token watchdog disabled")
	}
	go s.runTokenLoop(ctx)
	go s.runBalanceLoop(ctx)
	// 启动补领：今天已经过了最近一个时点却还没签时补一次，避免一整天没领到。
	if cfg := s.config(); cfg.Enabled && cfg.CreditsAutoClaim {
		s.catchUpCheckin(ctx, time.Now())
	} else if !cfg.CreditsAutoClaim {
		log.Printf("credit checkin disabled (credits_auto_claim=false)")
	}
	s.runCheckinLoop(ctx)
}

// runTokenLoop 校验启用账号（自动续期 + 保活），阻塞直到 ctx 取消。
// 间隔每轮重读，面板改 poll_minutes 后下一轮即按新间隔走。
func (s *Scheduler) runTokenLoop(ctx context.Context) {
	for {
		cfg := s.config()
		if !cfg.Enabled {
			if !s.sleepCtx(ctx, time.Minute) {
				return
			}
			continue
		}
		s.Tick(ctx)
		if !s.sleepCtx(ctx, cfg.PollInterval) {
			return
		}
	}
}

// Tick 校验启用账号（自动续期 + 保活）。
//
// 签到**不再**挂在这里：轮询间隔（默认 30 分钟）与「每天几点」是两件事，
// 挂着跑等于一天领几十次上游活动列表。签到由 runCheckinLoop 按时点触发。
func (s *Scheduler) Tick(ctx context.Context) {
	cfg := s.config()
	for _, acct := range cfg.Pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !cfg.Pool.Enabled(acct.Name) {
			continue
		}
		ok, _ := cfg.Pool.Validate(acct)
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
		if err := cfg.Pool.CheckAndRefreshTokenWithin(acct.Name, cfg.RefreshSkew); err != nil {
			log.Printf("proactive refresh failed account=%s err=%v", acct.Name, err)
		} else if remaining <= cfg.RefreshSkew {
			log.Printf("token refreshed account=%s new_remaining=%s", acct.Name,
				acct.Auth.Remaining().Round(time.Minute))
		}
	}
}
