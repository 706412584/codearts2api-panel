package scheduler

import (
	"context"
	"log"
	"time"

	"codearts2api-panel/internal/balance"
)

// runBalanceLoop 独立于 token 看护和签到：即使面板一直不打开，也会持续留下余额水位。
func (s *Scheduler) runBalanceLoop(ctx context.Context) {
	ticker := time.NewTicker(balance.Interval())
	defer ticker.Stop()
	for {
		s.RunBalanceRefreshNow(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunBalanceRefreshNow 刷新全部启用账号。采样由 pool 的 BalanceObserver 统一完成，
// 因而这里和面板手动刷新都不会遗漏，也不会记录失败响应里的旧缓存。
func (s *Scheduler) RunBalanceRefreshNow(ctx context.Context) {
	p := s.config().Pool
	if p == nil {
		return
	}
	for _, acct := range p.Accounts() {
		if ctx.Err() != nil {
			return
		}
		if !p.Enabled(acct.Name) {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, time.Minute)
		_, err := p.RefreshBalance(refreshCtx, acct.Name)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("balance refresh failed account=%s err=%v", acct.Name, err)
		}
	}
}
