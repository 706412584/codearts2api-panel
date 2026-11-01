// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：面板保存配置后应当立即生效，但 Handler 的配置字段是被所有请求 goroutine
// 直读的普通结构体，原地改写会 data race。这里用「不可变快照 + atomic 指针」：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁、无竞争。
//
// 只承载**请求期读取**的字段。监听地址、凭据目录、状态文件、上游 HTTP 超时等
// 装配期依赖仍留在启动配置里，改它们必须重启进程。
package livecfg

import (
	"sync/atomic"
	"time"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey            string
	AdminKey          string
	DefaultModel      string
	SoftCooldown      time.Duration // 429 软冷却时长
	ErrThreshold      int           // 连续错误阈值
	ErrCooldown       time.Duration // 错误冷却时长
	QueueRetryDelay   time.Duration // 上游排队重试间隔
	QueueMaxAttempts  int           // 上游排队重试次数上限
	OAuthCallbackHost string        // 授权回调 host 覆盖
	LoginClientID     string        // WebUI 登录 client_id

	// 以下字段供面板 overview 如实展示调度/池参数（本身由 pool/scheduler 应用）。
	WatchEnabled             bool
	CreditsAutoClaim         bool
	PollMinutes              int
	RefreshSkewMinutes       int
	KeepaliveIntervalMinutes int
	MaxConcurrent            int
	KeepaliveWindow          string
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }
