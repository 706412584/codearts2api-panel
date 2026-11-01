package scheduler

import (
	"context"
	"log"
	"sort"
	"time"
)

// defaultCheckinHours 未配置时的每日签到时点（本地时间）。
// 与参考项目一致：早晚各一次，任一时点没跑成还有另一次兜底。
var defaultCheckinHours = []int{9, 21}

// checkinWaitStep 等待时点的分段长度：主机/容器休眠会冻结单调时钟，
// 分段醒来用墙钟重判，最多迟到一段；正常情况最后一段正好睡到整点。
const checkinWaitStep = time.Minute

// normalizeHours 规范时点列表：只保留 0-23、去重、升序；空列表回落默认值。
func normalizeHours(hours []int) []int {
	seen := make(map[int]bool, len(hours))
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		if h < 0 || h > 23 || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return append([]int(nil), defaultCheckinHours...)
	}
	sort.Ints(out)
	return out
}

// nextFire 返回 now 之后最近的一个整点（本地时区，hours 须已规范化）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		at := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !at.After(now) {
			// 用 AddDate 而不是 +24h：跨夏令时的日期边界仍落在本地整点。
			at = at.AddDate(0, 0, 1)
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest
}

// lastPassedFire 返回「今天已经过去的最近一个时点」；今天的时点都还没到则返回零值。
func lastPassedFire(now time.Time, hours []int) time.Time {
	var latest time.Time
	for _, h := range hours {
		at := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if at.After(now) {
			continue
		}
		if at.After(latest) {
			latest = at
		}
	}
	return latest
}

// catchUpCheckin 启动补领：今天已过最近的时点就补一次。
//
// 只在「今天还没签」时有实际动作 —— pool 的本地账本会让已了结的账号直接短路，
// 因此这里不必逐号判断；今天的时点都还没到（例如 08:00 启动）则什么都不做，
// 等 09:00 的时点。
func (s *Scheduler) catchUpCheckin(ctx context.Context, now time.Time) {
	last := lastPassedFire(now, s.cfg.CheckinHours)
	if last.IsZero() {
		return
	}
	log.Printf("credit checkin catch-up for %s slot", last.Format("2006-01-02 15:04"))
	s.RunCheckinNow(ctx)
}

// runCheckinLoop 等到每个签到时点后执行一次，阻塞直到 ctx 取消。
func (s *Scheduler) runCheckinLoop(ctx context.Context) {
	for {
		next := nextFire(time.Now(), s.cfg.CheckinHours)
		log.Printf("next credit checkin at %s (hours=%v)", next.Format("2006-01-02 15:04"), s.cfg.CheckinHours)
		if !s.wait(ctx, next) {
			return
		}
		s.RunCheckinNow(ctx)
	}
}

// RunCheckinNow 立即对所有启用账号领取每日签到/新手积分。
// 定时、启动补领与手动「全员签到」都走这里；单账号领取由 pool 串行化，
// 已了结的账号在池内直接短路，不会重复请求上游。
func (s *Scheduler) RunCheckinNow(ctx context.Context) {
	for _, acct := range s.cfg.Pool.Accounts() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !s.cfg.Pool.Enabled(acct.Name) {
			continue
		}
		claimCtx, cancel := context.WithTimeout(ctx, time.Minute)
		result, err := s.cfg.Pool.Checkin(claimCtx, acct.Name)
		cancel()
		if err != nil {
			log.Printf("credit checkin failed account=%s err=%v", acct.Name, err)
			continue
		}
		log.Printf("credit checkin account=%s ok=%t message=%s", acct.Name, result.OK, result.Message)
	}
}

// sleepUntil 分段等待到墙钟时刻 t；ctx 取消立即返回 false。
func sleepUntil(ctx context.Context, t time.Time) bool {
	for {
		remaining := time.Until(t)
		if remaining <= 0 {
			return ctx.Err() == nil
		}
		if remaining > checkinWaitStep {
			remaining = checkinWaitStep
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
			// 回到循环顶用墙钟重判：休眠冻结单调时钟时，墙钟已越过时点，
			// 至多一段之后就会发现并补跑。
		}
	}
}
