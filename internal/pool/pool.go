// Package pool 管理 CodeArts 账号池：token 校验/自动刷新、冷却、禁用与轮转。
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/upstream"
)

// CoolKind 冷却原因。
type CoolKind int

const (
	CoolSoft CoolKind = iota
	CoolErr
	CoolPlan
)

// Account 一个上游账号。
type Account struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	UserName     string `json:"user_name"`
	DefaultModel string `json:"default_model"`

	Auth   *auth.Auth       `json:"-"`
	Client *upstream.Client `json:"-"`

	mu                  sync.Mutex
	refreshMu           sync.Mutex
	checkinMu           sync.Mutex
	checkin             *CheckinStatus
	lastCheckinDay      string // 每日签到已了结的本地日期（"2006-01-02"），跨零点自然失效
	checkinClaimed      bool   // 该日真的签到成功/已领；只有它为真才显示「已签」
	newUserSettled      bool   // 新手注册活动已了结，单账号只领一次
	balanceMu           sync.Mutex
	creditBalance       *upstream.CreditBalance
	balanceUpdatedAt    time.Time
	creditBalanceError  string
	removed             bool
	lastValidated       time.Time
	errCount            int
	coolUntil           time.Time
	coolKind            CoolKind
	disabled            bool
	disabledReason      string
	lastErr             string
	activeConcurrent    int       // 当前活跃并发请求数
	maxConcurrent       int       // 最大允许并发数
	keepaliveLastPing   time.Time // 最后保活心跳时间
}

// Config 池配置。
type Config struct {
	ErrThreshold     int
	ErrCooldown      time.Duration
	SoftCooldown     time.Duration
	RefreshSkew      time.Duration        // token 到期前多久自动刷新
	MaxConcurrent    int                  // 单账号最大并发数（默认 5）
	KeepaliveWindow  time.Duration        // 保活心跳窗口，超过此时间无活动则发送心跳
	LoginConfig      upstream.LoginConfig // OAuth/refresh 端点；零值使用生产默认
	CheckinClient    CheckinClient        // 可选活动客户端；nil 使用账号 Client
	BalanceClient    BalanceClient
}

// Pool 账号池。
type Pool struct {
	cfg      Config
	state    string
	mu       sync.Mutex
	stateMu  sync.Mutex
	accounts []*Account
}

// New 构建池。
func New(auths []*auth.Auth, cfg Config, stateFile string) (*Pool, error) {
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 30 * time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		// 上游单账号最多 3 个并发会话（TM.00001041），但会话槽位释放慢，
		// 串行（1）最稳，避免撞限。可按多账号情况调高。
		cfg.MaxConcurrent = 1
	}
	if cfg.KeepaliveWindow <= 0 {
		cfg.KeepaliveWindow = 10 * time.Minute // 10 分钟无活动则保活
	}
	if cfg.LoginConfig.ClientID == "" {
		cfg.LoginConfig = upstream.DefaultLoginConfig()
	}
	p := &Pool{cfg: cfg, state: stateFile}
	for _, a := range auths {
		acct := &Account{
			Name:              a.UserID,
			UID:               a.UserID,
			UserName:          a.UserName,
			Auth:              a,
			Client:            upstream.New(120 * time.Second),
			maxConcurrent:     cfg.MaxConcurrent,
			keepaliveLastPing: time.Now(),
		}
			p.accounts = append(p.accounts, acct)
	}
	p.loadState()
	return p, nil
}

// Accounts 返回全部账号。
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Account(nil), p.accounts...)
}

// Get 按名字取账号。
func (p *Pool) Get(name string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// AddAccount 动态添加账号（WebUI 登录成功后调用）。
func (p *Pool) AddAccount(a *auth.Auth) *Account {
	acct := &Account{
		Name:              a.UserID,
		UID:               a.UserID,
		UserName:          a.UserName,
		Auth:              a,
		Client:            upstream.New(120 * time.Second),
		maxConcurrent:     p.cfg.MaxConcurrent,
		keepaliveLastPing: time.Now(),
	}
	p.mu.Lock()
	for i, existing := range p.accounts {
		if existing.Name == acct.Name {
			existing.mu.Lock()
			existing.Auth.ReplaceFrom(a)
			existing.maxConcurrent = p.cfg.MaxConcurrent
			existing.lastValidated = time.Time{}
			existing.disabled = false
			existing.disabledReason = ""
			existing.lastErr = ""
			existing.removed = false
			existing.mu.Unlock()
			p.mu.Unlock()
			p.saveState()
			return existing
		}
		p.accounts[i] = existing
	}
	p.accounts = append(p.accounts, acct)
	p.mu.Unlock()
	p.saveState()
	log.Printf("pool account added name=%s uid=%s", acct.Name, a.UserID)
	return acct
}

// List 状态快照（脱敏；字段对齐 WorkBuddy 面板）。
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.accounts))
	now := time.Now()
	today := checkinDay(now)
	for _, a := range p.accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		until := ""
		if cooling {
			until = a.coolUntil.Format(time.RFC3339)
		}
		reason := a.disabledReason
		if reason == "" {
			reason = a.lastErr
		}
		var credit any
		if a.creditBalance != nil {
			credit = a.creditBalance.Total
		}
		expires := ""
		remaining := a.Auth.Remaining().Round(time.Minute).String()
		credentialType := "permanent"
		if t := a.Auth.ExpiresAt(); !t.IsZero() {
			expires = t.Format(time.RFC3339)
			credentialType = "temporary"
		} else {
			remaining = "permanent"
		}
		if a.Auth.Refreshable() {
			credentialType = "oauth"
		}
		out = append(out, map[string]any{
			"name":                  a.Name,
			"uid":                   a.UID,
			"nickname":              a.UserName,
			"user_name":             a.UserName,
			"default_model":         a.DefaultModel,
			"credits":               credit,
			"credit_balance":        a.creditBalance,
			"balance_updated_at":    optionalTime(a.balanceUpdatedAt),
			"credit_balance_error":  a.creditBalanceError,
			"refreshable":           a.Auth.Refreshable(),
			"credential_type":       credentialType,
			"checkin":               a.checkin,
			"checkin_done":          a.lastCheckinDay == today && a.checkinClaimed,
			"last_checkin_day":      a.lastCheckinDay,
			"new_user_settled":      a.newUserSettled,
			"disabled":              a.disabled,
			"cooling":               cooling,
			"until":                 until,
			"err_count":             a.errCount,
			"reason":                reason,
			"last_error":            a.lastErr,
			"active_concurrent":     a.activeConcurrent,
			"max_concurrent":        a.maxConcurrent,
			"token_remaining":       remaining,
			"expires_at":            expires,
		})
		a.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["uid"].(string) < out[j]["uid"].(string) })
	return out
}

// Stats 汇总。
func (p *Pool) Stats() (total, healthy, disabled, cooling int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		total++
		a.mu.Lock()
		if a.creditBalance != nil {
			credits += a.creditBalance.Total
		}
		if a.disabled {
			disabled++
		} else if now.Before(a.coolUntil) {
			cooling++
		} else {
			healthy++
		}
		a.mu.Unlock()
	}
	return
}

// Enable 解禁账号。
func (p *Pool) Enable(name string) bool {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.disabled = false
			a.disabledReason = ""
			a.lastErr = ""
			a.mu.Unlock()
			return true
		}
	}
	return false
}

// ClearCooldown 清冷却。
func (p *Pool) ClearCooldown(name string) bool {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name || a.UID == name {
			a.mu.Lock()
			a.coolUntil = time.Time{}
			a.coolKind = CoolSoft
			a.mu.Unlock()
			return true
		}
	}
	return false
}

// SyncToDir 用磁盘 auths 全量对齐。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	byUID := map[string]*Account{}
	for _, a := range p.accounts {
		byUID[a.UID] = a
	}
	next := make([]*Account, 0, len(auths))
	for _, a := range auths {
		if existing, ok := byUID[a.UserID]; ok {
			existing.mu.Lock()
			existing.Auth.ReplaceFrom(a)
			existing.maxConcurrent = p.cfg.MaxConcurrent
			existing.mu.Unlock()
			next = append(next, existing)
			delete(byUID, a.UserID)
		} else {
			acct := &Account{
				Name:              a.UserID,
				UID:               a.UserID,
				UserName:          a.UserName,
				Auth:              a,
				Client:            upstream.New(120 * time.Second),
				maxConcurrent:     p.cfg.MaxConcurrent,
				keepaliveLastPing: time.Now(),
			}
			next = append(next, acct)
		}
	}
	p.accounts = next
}

// PickExcluding 挑一个健康账号。调度优先级：
//
//  1. 有空闲并发槽位（activeConcurrent < maxConcurrent）的账号优先于满载账号——
//     多账号池里「第一个账号满载就死等它」会让其他账号空转，实测会造成
//     3 分钟级排队。
//  2. 同档位内按积分余额从高到低排——余额高的多承担流量，避免单账号被抽干
//     （余额未知的账号排在已知余额之后，但不被排除）。
//  3. 仍相同则保持账号原有顺序（稳定，便于复现）。
//
// 满载账号仍然会被返回（当没有空闲账号时），调用方配合 AcquireLockWait 短超时
// 使用：等不到槽位就换下一个账号重试。
func (p *Pool) PickExcluding(tried map[string]bool) *Account {
	now := time.Now()
	type cand struct {
		acct    *Account
		idle    bool
		credits float64
		known   bool
	}
	p.mu.Lock()
	cands := make([]cand, 0, len(p.accounts))
	for _, a := range p.accounts {
		if tried != nil && tried[a.Name] {
			continue
		}
		a.mu.Lock()
		healthy := !a.disabled && now.After(a.coolUntil)
		idle := a.activeConcurrent < a.maxConcurrent
		var credits float64
		known := a.creditBalance != nil
		if known {
			credits = a.creditBalance.Total
		}
		a.mu.Unlock()
		if healthy {
			cands = append(cands, cand{acct: a, idle: idle, credits: credits, known: known})
		}
	}
	p.mu.Unlock()
	if len(cands) == 0 {
		return nil
	}
	// 稳定排序：空闲优先 → 余额高优先 → 已知余额优先 → 原顺序。
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].idle != cands[j].idle {
			return cands[i].idle
		}
		if cands[i].known != cands[j].known {
			return cands[i].known
		}
		return cands[i].credits > cands[j].credits
	})
	return cands[0].acct
}

// Validate 校验并（必要时）自动刷新 token。带 5 分钟缓存。
func (p *Pool) Validate(a *Account) (bool, error) {
	a.mu.Lock()
	if a.removed {
		a.mu.Unlock()
		return false, fmt.Errorf("account removed")
	}
	if time.Since(a.lastValidated) < 5*time.Minute && !a.disabled {
		ok := true
		a.mu.Unlock()
		return ok, nil
	}
	a.mu.Unlock()

	authz := a.Auth
	if authz.Expired() && !authz.Refreshable() {
		p.Disable(a.Name, "token expired; re-login required")
		return false, nil
	}
	if authz.ExpiringSoon(p.cfg.RefreshSkew) || authz.Expired() {
		if !authz.Refreshable() {
			// 没有 refresh_token（华为云 ticket 通道不返回），不立即 disable，
			// 未过期时只打告警；到期后由上面的分支明确标记为需重新登录。
			log.Printf("pool token account=%s expiring soon (remaining=%s) but no refresh_token, re-login required",
				a.Name, authz.Remaining().Round(time.Minute))
			a.mu.Lock()
			a.lastValidated = time.Now()
			a.mu.Unlock()
			return true, nil
		}
		if err := p.RefreshToken(a.Name); err != nil {
			var expired *upstream.RefreshTokenExpiredError
			if errors.As(err, &expired) {
				p.Disable(a.Name, "refresh_token expired; re-login required")
				return false, nil
			}
			// 网络/5xx 属于可重试错误：保持账号可调度，由后续
			// watchdog tick 重试，不得把短暂故障固化为永久禁用。
			return false, err
		}
	}
	a.mu.Lock()
	a.lastValidated = time.Now()
	a.mu.Unlock()
	return true, nil
}

// Cooldown 冷却账号。
func (p *Pool) Cooldown(name string, kind CoolKind, dur time.Duration, reason string) {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.coolUntil = time.Now().Add(dur)
		a.coolKind = kind
		a.lastErr = reason
		a.mu.Unlock()
		log.Printf("pool cooldown account=%s kind=%d dur=%s reason=%s", name, kind, dur, reason)
	}
}

// NoteError 累计错误。
func (p *Pool) NoteError(name string, threshold int, cooldown time.Duration) {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.errCount++
		if a.errCount >= threshold {
			a.coolUntil = time.Now().Add(cooldown)
			a.lastErr = "consecutive errors"
		}
		a.mu.Unlock()
	}
}

// NoteSuccess 清零错误计数。
func (p *Pool) NoteSuccess(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.errCount = 0
			a.mu.Unlock()
		}
	}
}

// Disable 禁用账号。
func (p *Pool) Disable(name, reason string) {
	defer p.saveState()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.disabled = true
			a.disabledReason = reason
			a.lastErr = reason
			a.mu.Unlock()
			log.Printf("pool disable account=%s reason=%s", name, reason)
		}
	}
}

// Healthy 查询账号可用。
// Busy 报告账号当前是否有活跃请求占用并发槽位（供探测等后台任务让位）。
func (p *Pool) Busy(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.activeConcurrent > 0
}

func (p *Pool) Healthy(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			h := !a.disabled && time.Now().After(a.coolUntil)
			a.mu.Unlock()
			return h
		}
	}
	return false
}

// Enabled 报告账号是否启用；冷却期间仍需续期。
func (p *Pool) Enabled(name string) bool {
	a := p.Get(name)
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.disabled
}

// AcquireLock 尝试获取账号的并发请求锁。返回 true 表示成功，false 表示已达上限。
func (p *Pool) AcquireLock(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.activeConcurrent >= a.maxConcurrent {
			log.Printf("pool concurrent limit reached account=%s current=%d max=%d",
				name, a.activeConcurrent, a.maxConcurrent)
			return false
		}
		a.activeConcurrent++
		return true
	}
	return false
}

// ReleaseLock 释放账号的并发请求锁，并唤醒等待槽位的请求。
func (p *Pool) ReleaseLock(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			if a.activeConcurrent > 0 {
				a.activeConcurrent--
			}
			a.mu.Unlock()
			break
		}
	}
	// 唤醒等待槽位的 AcquireLockWait（轮询方式无需 broadcast，此处保留语义占位）。
}

// AcquireLockWait 阻塞等待账号并发槽位，超时返回 false。
//
// 上游单账号并发会话上限 3，本池 maxConcurrent 默认 2 留余量。
// 高并发请求在此排队等待槽位释放，而非立即失败跳号。
// timeout=0 时等价于非阻塞 AcquireLock。
func (p *Pool) AcquireLockWait(name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		acquired := false
		for _, a := range p.accounts {
			if a.Name != name {
				continue
			}
			a.mu.Lock()
			if a.activeConcurrent < a.maxConcurrent {
				a.activeConcurrent++
				acquired = true
			}
			a.mu.Unlock()
			break
		}
		p.mu.Unlock()
		if acquired {
			return true
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return false
		}
		// 避免忙轮询：每次 100ms 重试。
		time.Sleep(100 * time.Millisecond)
	}
}

// AcquireLockAny 全局排队：在**任意**健康账号上抢一个并发槽位，返回抢到的账号名。
//
// 与 AcquireLockWait 的区别：那个只等指定账号，满载时调用方只能换号（各账号
// 各自排队）；这个把整个池当一个资源池排队，谁先空出来就用谁——不会出现
// 「A 满载、B/C 空闲，请求却卡在 A 的队列里」。
//
// exclude 里的账号不参与（本轮已试过并失败的账号）；ctx 取消立即返回。
// 没有任何可等待的账号时立即返回 false（不必空等）；否则等到 timeout。
// 返回空串表示超时/取消/无候选。
func (p *Pool) AcquireLockAny(ctx context.Context, exclude map[string]bool, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		name, ok, eligible := p.tryAcquireIdle(exclude)
		if ok {
			return name, true
		}
		if eligible == 0 {
			// 排除集之外已无健康账号：再等也不会有人空出槽位。
			return "", false
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return "", false
		}
		wait := 100 * time.Millisecond
		if remain := time.Until(deadline); remain < wait {
			wait = remain
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return "", false
			case <-time.After(wait):
			}
			continue
		}
		time.Sleep(wait)
	}
}

// tryAcquireIdle 在空闲账号里按与 PickExcluding 相同的优先级抢一个槽位。
// eligible 是排除集之外的健康账号总数（无论是否满载），供调用方判断
// 「还有没有等下去的必要」。
func (p *Pool) tryAcquireIdle(exclude map[string]bool) (name string, acquired bool, eligible int) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	// 先按优先级找出候选，再逐个尝试加锁；竞争失败就继续下一个候选。
	type cand struct {
		acct    *Account
		credits float64
		known   bool
	}
	cands := make([]cand, 0, len(p.accounts))
	for _, a := range p.accounts {
		if exclude != nil && exclude[a.Name] {
			continue
		}
		a.mu.Lock()
		healthy := !a.disabled && now.After(a.coolUntil)
		free := a.activeConcurrent < a.maxConcurrent
		var credits float64
		known := a.creditBalance != nil
		if known {
			credits = a.creditBalance.Total
		}
		a.mu.Unlock()
		if !healthy {
			continue
		}
		eligible++
		if free {
			cands = append(cands, cand{acct: a, credits: credits, known: known})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].known != cands[j].known {
			return cands[i].known
		}
		return cands[i].credits > cands[j].credits
	})
	// 候选已在 p.mu 内、且各自的 activeConcurrent 已复核；直接加锁。
	for _, c := range cands {
		c.acct.mu.Lock()
		if c.acct.activeConcurrent < c.acct.maxConcurrent {
			c.acct.activeConcurrent++
			name := c.acct.Name
			c.acct.mu.Unlock()
			return name, true, eligible
		}
		c.acct.mu.Unlock()
	}
	return "", false, eligible
}

// NeedKeepalive 检查账号是否需要保活心跳。
func (p *Pool) NeedKeepalive(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		// 超过 KeepaliveWindow 无活动则需保活
		return time.Since(a.keepaliveLastPing) > p.cfg.KeepaliveWindow
	}
	return false
}

// PingKeepalive 更新账号的保活心跳时间。
func (p *Pool) PingKeepalive(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.mu.Lock()
			a.keepaliveLastPing = time.Now()
			a.mu.Unlock()
			break
		}
	}
}

// CheckAndRefreshTokenWithin 在剩余有效期低于 skew 时主动刷新 token。
//
// skew 来自 scheduler 的 refresh_skew（配置项 watch.refresh_skew_minutes）。
// 此前该配置从未生效：原实现硬编码 1 小时阈值，调度器传进来的 RefreshSkew
// 只出现在日志里，README 写的「提前刷新时间」实际不起作用。
func (p *Pool) CheckAndRefreshTokenWithin(name string, skew time.Duration) error {
	if skew <= 0 {
		skew = time.Hour
	}
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}

	acct.mu.Lock()
	remaining := acct.Auth.Remaining()
	hasRefresh := acct.Auth.Refreshable()
	acct.mu.Unlock()

	if remaining > skew {
		return nil
	}
	if !hasRefresh {
		// ticket 登录不返回 refresh_token，跳过无意义的周期刷新。
		return nil
	}
	log.Printf("pool proactive refresh account=%s remaining=%s skew=%s",
		name, remaining.Round(time.Minute), skew)
	return p.RefreshToken(name)
}

// CheckAndRefreshToken 检查 token 是否快过期并主动刷新（比 scheduler 更积极）。
func (p *Pool) CheckAndRefreshToken(name string) error {
	return p.CheckAndRefreshTokenWithin(name, time.Hour)
}

// RefreshToken 手动刷新指定账号的 token。
func (p *Pool) RefreshToken(name string) error {
	p.mu.Lock()
	var acct *Account
	for _, a := range p.accounts {
		if a.Name == name {
			acct = a
			break
		}
	}
	p.mu.Unlock()
	if acct == nil {
		return fmt.Errorf("account not found: %s", name)
	}
	requestedToken := acct.Auth.Refresh()
	acct.refreshMu.Lock()
	defer acct.refreshMu.Unlock()

	authz := acct.Auth
	refreshToken := authz.Refresh()
	if refreshToken != requestedToken {
		return nil
	}
	acct.mu.Lock()
	removed := acct.removed
	acct.mu.Unlock()
	if removed {
		return fmt.Errorf("account removed")
	}
	if refreshToken == "" {
		return fmt.Errorf("no refresh_token available")
	}
	if !authz.Refreshable() {
		return fmt.Errorf("incomplete refresh credentials; re-login required")
	}
	dpopPrivateJWK := authz.DPoPPrivateJWK()
	if dpopPrivateJWK["d"] == "" {
		return fmt.Errorf("no DPoP private key available; re-login required")
	}

	cfg := p.loginConfig()
	cfg.ClientID = authz.ClientIDOr(cfg.ClientID)
	resp, err := acct.Client.RefreshToken(context.Background(), cfg, refreshToken, authz.Verifier(), upstream.DPoPPrivateJWK(dpopPrivateJWK))
	if err != nil {
		if authz.Refresh() != refreshToken {
			return nil
		}
		return fmt.Errorf("refresh failed: %w", err)
	}
	if authz.Refresh() != refreshToken {
		return nil
	}

	if _, err := authz.UpdateCredentialsIfCurrent(refreshToken,
		resp.Credentials.SecurityToken,
		resp.Credentials.AccessKeyID,
		resp.Credentials.SecretAccessKey,
		resp.Credentials.Expiration,
		resp.RefreshToken,
	); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	acct.mu.Lock()
	acct.disabled = false
	acct.disabledReason = ""
	acct.lastErr = ""
	acct.lastValidated = time.Now()
	acct.mu.Unlock()
	log.Printf("pool token refreshed manually account=%s", name)
	return nil
}

// RefreshAllTokens 批量刷新所有临期 token（用于 cron 或脚本）。
func (p *Pool) RefreshAllTokens() int {
	count := 0
	for _, acct := range p.Accounts() {
		if err := p.CheckAndRefreshToken(acct.Name); err == nil {
			count++
		}
	}
	return count
}

// Acquire 尝试获取账号并发 slot（返回 true 表示成功，调用方需随后 Release）。
func (p *Pool) Acquire(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	target.mu.Lock()
	defer target.mu.Unlock()

	// 检查是否超过最大并发限制
	if target.activeConcurrent >= target.maxConcurrent {
		return false
	}
	// 检查是否正在冷却
	if !time.Now().After(target.coolUntil) {
		return false
	}
	// 增加活跃计数
	target.activeConcurrent++
	target.keepaliveLastPing = time.Now()
	return true
}

// Release 释放账号并发 slot。
func (p *Pool) Release(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.activeConcurrent > 0 {
		target.activeConcurrent--
	}
}

// CheckKeepalive 检查是否需要保活心跳。
func (p *Pool) CheckKeepalive(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return false
	}
	// 先取窗口再加锁：与 NeedKeepalive（p.mu → a.mu）保持同一把锁顺序。
	window := p.keepaliveWindow()
	target.mu.Lock()
	defer target.mu.Unlock()

	// 如果超过保活窗口无活动，需要保活
	needsKeepalive := time.Since(target.keepaliveLastPing) > window
	if needsKeepalive {
		target.keepaliveLastPing = time.Now()
	}
	return needsKeepalive
}

// SetKeepalive 手动更新保活时间。
func (p *Pool) SetKeepalive(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	target.keepaliveLastPing = time.Now()
}

// GetConcurrentStats 获取账号并发统计信息。
func (p *Pool) GetConcurrentStats() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]map[string]any, 0, len(p.accounts))
	now := time.Now()
	for _, a := range p.accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		concurrent := a.activeConcurrent
		maxConc := a.maxConcurrent
		lastPing := a.keepaliveLastPing
		a.mu.Unlock()

		out = append(out, map[string]any{
			"name":              a.Name,
			"uid":               a.UID,
			"user_name":         a.UserName,
			"active_concurrent": concurrent,
			"max_concurrent":    maxConc,
			"utilization":       float64(concurrent) / float64(maxConc),
			"last_keepalive":    lastPing.Format(time.RFC3339),
			"cooling":           cooling,
			"disabled":          a.disabled,
		})
	}
	return out
}

// TryAcquire 尝试获取账号的并发槽位，成功返回 true 并增加计数。
func (p *Pool) TryAcquire(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target == nil {
		return false
	}

	target.mu.Lock()
	defer target.mu.Unlock()

	if target.disabled || time.Now().Before(target.coolUntil) {
		return false
	}

	if target.activeConcurrent >= target.maxConcurrent {
		log.Printf("pool concurrent limit reached account=%s active=%d max=%d",
			name, target.activeConcurrent, target.maxConcurrent)
		return false
	}

	target.activeConcurrent++
	target.keepaliveLastPing = time.Now()
	return true
}

// NeedsKeepalive 检查账号是否需要保活心跳。
func (p *Pool) NeedsKeepalive(name string) bool {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target == nil {
		return false
	}

	// 先取窗口再加锁：与 NeedKeepalive（p.mu → a.mu）保持同一把锁顺序。
	window := p.keepaliveWindow()
	target.mu.Lock()
	defer target.mu.Unlock()

	// 超过窗口期无活动则需保活
	return time.Since(target.keepaliveLastPing) > window
}

// UpdateKeepalive 更新账号的最后保活时间。
func (p *Pool) UpdateKeepalive(name string) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target != nil {
		target.mu.Lock()
		target.keepaliveLastPing = time.Now()
		target.mu.Unlock()
	}
}

// SetMaxConcurrentAll 热更新单账号并发上限：同步池配置与所有已加载账号，
// 之后新加入/同步进来的账号也沿用该值（面板保存 max_concurrent 后立即生效）。
func (p *Pool) SetMaxConcurrentAll(max int) {
	if max <= 0 {
		return
	}
	p.mu.Lock()
	p.cfg.MaxConcurrent = max
	targets := append([]*Account(nil), p.accounts...)
	p.mu.Unlock()
	for _, a := range targets {
		a.mu.Lock()
		a.maxConcurrent = max
		a.mu.Unlock()
	}
}

// SetKeepaliveWindow 热更新保活窗口（保活判定依据）。
func (p *Pool) SetKeepaliveWindow(window time.Duration) {
	if window <= 0 {
		return
	}
	p.mu.Lock()
	p.cfg.KeepaliveWindow = window
	p.mu.Unlock()
}

// SetLoginClientID 热更新 OAuth client_id（token 续期路径使用）。
// 空值表示"沿用现有值"，与密钥留空的语义一致。
func (p *Pool) SetLoginClientID(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	p.cfg.LoginConfig.ClientID = id
	p.mu.Unlock()
}

// keepaliveWindow 读当前保活窗口。读方不加锁会与面板热更新竞争，统一走这里。
func (p *Pool) keepaliveWindow() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.KeepaliveWindow
}

// loginConfig 当前 OAuth 配置快照（client_id 可热改）。
func (p *Pool) loginConfig() upstream.LoginConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.LoginConfig
}

// SetMaxConcurrent 设置账号的最大并发数。
func (p *Pool) SetMaxConcurrent(name string, max int) {
	p.mu.Lock()
	var target *Account
	for _, a := range p.accounts {
		if a.Name == name {
			target = a
			break
		}
	}
	p.mu.Unlock()

	if target != nil {
		target.mu.Lock()
		target.maxConcurrent = max
		target.mu.Unlock()
	}
}

func (p *Pool) loadState() {
	if p.state == "" {
		return
	}
	raw, err := os.ReadFile(p.state)
	if err != nil {
		return
	}
	var data []struct {
		Name      string                   `json:"name"`
		ErrCount  int                      `json:"err_count"`
		CoolUntil time.Time                `json:"cool_until"`
		Disabled  bool                     `json:"disabled"`
		Reason    string                   `json:"reason"`
		LastErr   string                   `json:"last_error"`
		Credit    *upstream.CreditBalance  `json:"credit_balance"`
		BalanceAt time.Time                `json:"balance_updated_at"`
		// 签到账本：重启后仍要知道「今天已签」「新手已领」，否则重启即重复打上游。
		// checkin_claimed 用指针读：更早的账本只有日期（写入即代表已签），不能因缺字段
		// 把已签的账号在升级当天显示成未签。
		LastCheckinDay *string `json:"last_checkin_day"`
		CheckinClaimed *bool   `json:"checkin_claimed"`
		NewUserSettled bool    `json:"new_user_settled"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	for _, a := range p.accounts {
		for _, s := range data {
			if s.Name == a.Name {
				a.mu.Lock()
				a.errCount = s.ErrCount
				a.coolUntil = s.CoolUntil
				a.disabled = s.Disabled
				a.disabledReason = s.Reason
				a.lastErr = s.LastErr
				a.creditBalance, a.balanceUpdatedAt = s.Credit, s.BalanceAt
				a.lastCheckinDay, a.checkinClaimed = "", false
				if s.LastCheckinDay != nil {
					a.lastCheckinDay = *s.LastCheckinDay
					a.checkinClaimed = s.CheckinClaimed == nil || *s.CheckinClaimed
				}
				a.newUserSettled = s.NewUserSettled
				a.mu.Unlock()
			}
		}
	}
}

func (p *Pool) saveState() {
	if err := p.saveStateErr(); err != nil {
		log.Printf("save pool state: %v", err)
	}
}

func (p *Pool) saveStateErr() error {
	if p.state == "" {
		return nil
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	type entry struct {
		Name      string                   `json:"name"`
		ErrCount  int                      `json:"err_count"`
		CoolUntil time.Time                `json:"cool_until"`
		Disabled  bool                     `json:"disabled"`
		Reason    string                   `json:"reason"`
		LastErr   string                   `json:"last_error"`
		Credit    *upstream.CreditBalance  `json:"credit_balance"`
		BalanceAt time.Time                `json:"balance_updated_at"`
		// 签到账本：持久化「今日已签」「新手已领」，重启后不重复请求上游。
		LastCheckinDay string `json:"last_checkin_day,omitempty"`
		CheckinClaimed bool   `json:"checkin_claimed,omitempty"`
		NewUserSettled bool   `json:"new_user_settled,omitempty"`
	}
	accounts := p.Accounts()
	out := make([]entry, 0, len(accounts))
	for _, a := range accounts {
		a.mu.Lock()
		out = append(out, entry{
			Name:      a.Name,
			ErrCount:  a.errCount,
			CoolUntil: a.coolUntil,
			Disabled:  a.disabled,
			Reason:    a.disabledReason,
			LastErr:   a.lastErr,
			Credit:    a.creditBalance, BalanceAt: a.balanceUpdatedAt,
			LastCheckinDay: a.lastCheckinDay, CheckinClaimed: a.checkinClaimed, NewUserSettled: a.newUserSettled,
		})
		a.mu.Unlock()
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.state), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(p.state), ".pool-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p.state)
}
