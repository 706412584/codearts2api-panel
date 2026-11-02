// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"
)

// DefaultRetentionDays 归档保留天数的缺省值：配置没给（<=0）时按 30 天清理
// data/requests/。与 cmd/server 的 request_log_retention_days 默认值同源。
const DefaultRetentionDays = 30

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir     string
	Enabled bool
	// RetentionDays 归档文件保留天数（按文件 mtime 判断）；<=0 用 DefaultRetentionDays。
	// 面板保存配置后由 SetRetentionDays 热更新，不必重启进程。
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Path      string    `json:"path"`
	Account   string    `json:"account,omitempty"`
	Model     string    `json:"model,omitempty"`
	Status    int       `json:"status"`
	OK        bool      `json:"ok"`
	Outcome   string    `json:"outcome"`
	// Error 是最终失败的错误摘要（已脱敏截断），成功请求为空。
	Error            string  `json:"error,omitempty"`
	DurationMs       int64   `json:"duration_ms"`
	TTFBMs           int64   `json:"ttfb_ms,omitempty"`
	Attempts         int     `json:"attempts,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	Credit           float64 `json:"credit,omitempty"`
	HasCredit        bool    `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// HistoryLimit 面板回看归档时的默认条数（一个页面窗口的量，不必读全部历史）。
const HistoryLimit = 200

// History 归档回看窗口：进程内指标随重启清零，磁盘归档仍在，面板据此在重启后
// 继续显示用量。各计数只覆盖 Recent 这一批记录，不是整个归档的累计。
type History struct {
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	Completed     int64     `json:"completed"`
	Succeeded     int64     `json:"succeeded"`
	Failed        int64     `json:"failed"`
	SuccessRate   float64   `json:"success_rate"`
	AvgDurationMs float64   `json:"avg_duration_ms"`
	TotalTokens   int64     `json:"total_tokens"`
	Recent        []Event   `json:"recent"`
}

// Snapshot 一次面板读取的完整指标快照。
// History 由调用方按需填充：进程已有数据时不必回看归档。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
	History         *History     `json:"history,omitempty"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	archive     *archiveWriter

	histMu sync.Mutex
	histAt time.Time
	hist   *History
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	r.recent = append([]Event{e}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    append([]Event(nil), r.recent...),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter)
}

// historyTTL 归档回看的缓存时长：面板日志页每 5 秒轮询一次 usage，而归档保留
// 30 天，不能每次都重扫目录。缓存只挡重复扫描 —— 取值要与轮询间隔同量级，否则
// 刚落盘的请求要等缓存过期才出现。
const historyTTL = 5 * time.Second

// History 从归档回看最近 limit 条请求（事件时间倒序），汇总成与进程指标同形的数字，
// 供面板在重启后继续显示用量。归档未启用或没有任何记录时返回 nil。
func (r *Recorder) History(limit int) *History {
	if r == nil || r.archive == nil || !r.archive.enabled() {
		return nil
	}
	if limit <= 0 {
		limit = HistoryLimit
	}
	r.histMu.Lock()
	defer r.histMu.Unlock()
	if r.hist != nil && time.Since(r.histAt) < historyTTL {
		return r.hist
	}
	events, err := r.archive.read(limit, Filter{})
	if err != nil || len(events) == 0 {
		return nil
	}
	h := &History{Recent: events, To: events[0].Time, From: events[len(events)-1].Time}
	var durationSum int64
	for _, e := range events {
		h.Completed++
		if e.OK {
			h.Succeeded++
		}
		durationSum += e.DurationMs
		h.TotalTokens += e.TotalTokens
	}
	h.Failed = h.Completed - h.Succeeded
	h.SuccessRate = round1(float64(h.Succeeded) / float64(h.Completed) * 100)
	h.AvgDurationMs = float64(durationSum) / float64(h.Completed)
	r.histAt, r.hist = time.Now(), h
	return h
}

// Stats 一个时间窗口内的聚合结果。面板「请求用量」直接渲染这些数字：30 天的归档
// 可能有几十万条记录，逐条传给浏览器既慢又没人看，聚合才是页面真正要的东西。
// From/To 为零值表示该侧不设界。
type Stats struct {
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	Archive       bool      `json:"archive"` // 是否有归档记录参与（false＝只有本进程的内存记录）
	Completed     int64     `json:"completed"`
	Succeeded     int64     `json:"succeeded"`
	Failed        int64     `json:"failed"`
	SuccessRate   float64   `json:"success_rate"`
	AvgDurationMs float64   `json:"avg_duration_ms"`
	// TTFTMs 只平均真正返回过首字的请求；TTFTSamples 为 0 时面板显示「—」。
	TTFTMs      float64 `json:"ttft_ms"`
	TTFTSamples int64   `json:"ttft_samples"`
	// 缓存命中/未命中 token 合计；CacheSamples 为 0 表示这批记录都没上报缓存用量。
	CacheHitTokens  int64 `json:"cache_hit_tokens"`
	CacheMissTokens int64 `json:"cache_miss_tokens"`
	CacheSamples    int64 `json:"cache_samples"`
	// TokenSamples 为上报过用量的请求数：为 0 时三类 token 合计都没有意义。
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	TokenSamples     int64 `json:"token_samples"`

	Models   []ModelStat   `json:"models"`
	Accounts []AccountStat `json:"accounts"`
	Hours    []HourStat    `json:"hours"`
}

// ModelStat 按模型汇总。TPS 是输出速度（token/秒），按「输出 token ÷ (耗时 - 首字时间)」
// 在样本上加权：不是逐条 tps 的平均，快请求不该和慢请求等权。
type ModelStat struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Succeeded        int64   `json:"succeeded"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheSamples     int64   `json:"cache_samples"`
	TTFTMs           float64 `json:"ttft_ms"`
	TTFTSamples      int64   `json:"ttft_samples"`
	TPS              float64 `json:"tps"`
}

// AccountStat 按账号汇总（账号是「昵称(uid8)」这种脱敏标签，不是完整 UID）。
type AccountStat struct {
	Account          string `json:"account"`
	Requests         int64  `json:"requests"`
	Succeeded        int64  `json:"succeeded"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CacheHitTokens   int64  `json:"cache_hit_tokens"`
	CacheMissTokens  int64  `json:"cache_miss_tokens"`
	CacheSamples     int64  `json:"cache_samples"`
}

// HourStat 按小时汇总，Time 是该小时的起始时刻（Unix 毫秒）。用绝对整点而不是
// 「服务器本地 9 点」：前端按浏览器时区渲染标签，两边不会各说各话。
type HourStat struct {
	Time  int64 `json:"t"`
	Count int64 `json:"count"`
}

// WindowStats 聚合 [from,to] 内的请求：内存里还没落盘的那几十条 + 归档里的全部匹配记录，
// 按 request_id 去重（同一条请求可能既在内存里又已经刷到磁盘）。
// 读取失败的文件会被跳过并返回第一个错误 —— 一个坏文件不该让整页数字消失。
func (r *Recorder) WindowStats(from, to time.Time) (Stats, error) {
	acc := newStatsAcc()
	seen := map[string]bool{}
	if r != nil {
		// 先收内存里的记录：它们一定比归档里的新，且可能还没刷盘。
		r.mu.Lock()
		for _, e := range r.recent {
			if !inWindow(e.Time, from, to) {
				continue
			}
			if e.RequestID != "" {
				seen[e.RequestID] = true
			}
			acc.add(e)
		}
		r.mu.Unlock()
	}
	var err error
	if r != nil && r.archive != nil && r.archive.enabled() {
		var archived int64
		err = r.archive.scan(Filter{From: from, To: to}, func(e Event) {
			if e.RequestID != "" && seen[e.RequestID] {
				return
			}
			archived++
			acc.add(e)
		})
		acc.archive = archived > 0
	}
	return acc.finish(from, to), err
}

// inWindow 判断事件时间是否落在窗口内；零值边界表示该侧不设界。
func inWindow(at, from, to time.Time) bool {
	if !from.IsZero() && at.Before(from) {
		return false
	}
	if !to.IsZero() && at.After(to) {
		return false
	}
	return true
}

// groupAcc 一组请求的累计量：窗口总计、按模型、按账号共用同一套口径。
type groupAcc struct {
	requests     int64
	succeeded    int64
	durationSum  int64
	prompt       int64
	completion   int64
	total        int64
	cacheHit     int64
	cacheMiss    int64
	cacheSamples int64
	ttftSum      int64
	ttftSamples  int64
	tokenSamples int64
	// genMs / genTokens 只累计「既有首字又有总耗时」的那些请求：
	// 没有首字时间的记录算不出生成段落，混进去会把 TPS 拉成假的。
	genMs     int64
	genTokens int64
}

func (g *groupAcc) observe(e Event) {
	g.requests++
	if e.OK {
		g.succeeded++
	}
	g.durationSum += e.DurationMs

	hit, miss := cacheTokens(e)
	if hit > 0 || miss > 0 {
		g.cacheHit += hit
		g.cacheMiss += miss
		g.cacheSamples++
	}

	prompt, completion, total := e.PromptTokens, e.CompletionTokens, e.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	g.prompt += prompt
	g.completion += completion
	g.total += total
	if prompt > 0 || completion > 0 || total > 0 {
		g.tokenSamples++
	}

	// 首字只统计真的吐出过响应的请求：失败在首字之前、非流式没记录的都不参与。
	if e.TTFBMs > 0 {
		g.ttftSum += e.TTFBMs
		g.ttftSamples++
		// 生成段落 = 总耗时 - 首字时间；非正数说明时间戳异常，别把 TPS 算成无穷大。
		if gen := e.DurationMs - e.TTFBMs; gen > 0 {
			g.genMs += gen
			g.genTokens += completion
		}
	}
}

func (g *groupAcc) ttftMs() float64 {
	if g.ttftSamples == 0 {
		return 0
	}
	return round1(float64(g.ttftSum) / float64(g.ttftSamples))
}

func (g *groupAcc) tps() float64 {
	if g.genMs <= 0 {
		return 0
	}
	return round1(float64(g.genTokens) * 1000 / float64(g.genMs))
}

// cacheTokens 取一条请求的缓存命中/未命中 token。老归档只存了命中数，未命中按
// prompt 差值补；两个都是 0 就是「上游没上报缓存」，调用方据此跳过。
func cacheTokens(e Event) (hit, miss int64) {
	hit, miss = e.CacheHitTokens, e.CacheMissTokens
	// 全命中时 prompt-hit=0，和「上报了 0 未命中」得到同一个数，不必区分。
	if miss == 0 && hit > 0 {
		if derived := e.PromptTokens - hit; derived > 0 {
			miss = derived
		}
	}
	return hit, miss
}

// statsAcc 聚合过程中的可变状态；对外只暴露 finish 出来的不可变 Stats。
type statsAcc struct {
	archive  bool
	group    groupAcc
	models   map[string]*groupAcc
	accounts map[string]*groupAcc
	hours    map[int64]int64
}

func newStatsAcc() *statsAcc {
	return &statsAcc{models: map[string]*groupAcc{}, accounts: map[string]*groupAcc{}, hours: map[int64]int64{}}
}

func (a *statsAcc) add(e Event) {
	a.group.observe(e)
	model := a.models[e.Model]
	if model == nil {
		model = &groupAcc{}
		a.models[e.Model] = model
	}
	model.observe(e)
	account := a.accounts[e.Account]
	if account == nil {
		account = &groupAcc{}
		a.accounts[e.Account] = account
	}
	account.observe(e)
	a.hours[e.Time.UTC().Truncate(time.Hour).UnixMilli()]++
}

func (a *statsAcc) finish(from, to time.Time) Stats {
	g := a.group
	s := Stats{
		From: from, To: to, Archive: a.archive,
		Completed: g.requests, Succeeded: g.succeeded,
		CacheHitTokens: g.cacheHit, CacheMissTokens: g.cacheMiss, CacheSamples: g.cacheSamples,
		PromptTokens: g.prompt, CompletionTokens: g.completion, TotalTokens: g.total,
		TTFTMs: g.ttftMs(), TTFTSamples: g.ttftSamples, TokenSamples: g.tokenSamples,
		Models:   make([]ModelStat, 0, len(a.models)),
		Accounts: make([]AccountStat, 0, len(a.accounts)),
		Hours:    make([]HourStat, 0, len(a.hours)),
	}
	s.Failed = s.Completed - s.Succeeded
	if s.Completed > 0 {
		s.SuccessRate = round1(float64(s.Succeeded) / float64(s.Completed) * 100)
		s.AvgDurationMs = round1(float64(g.durationSum) / float64(s.Completed))
	}
	for name, m := range a.models {
		s.Models = append(s.Models, ModelStat{
			Model: name, Requests: m.requests, Succeeded: m.succeeded,
			PromptTokens: m.prompt, CompletionTokens: m.completion, TotalTokens: m.total,
			CacheHitTokens: m.cacheHit, CacheMissTokens: m.cacheMiss, CacheSamples: m.cacheSamples,
			TTFTMs: m.ttftMs(), TTFTSamples: m.ttftSamples, TPS: m.tps(),
		})
	}
	for name, ac := range a.accounts {
		s.Accounts = append(s.Accounts, AccountStat{
			Account: name, Requests: ac.requests, Succeeded: ac.succeeded,
			PromptTokens: ac.prompt, CompletionTokens: ac.completion, TotalTokens: ac.total,
			CacheHitTokens: ac.cacheHit, CacheMissTokens: ac.cacheMiss, CacheSamples: ac.cacheSamples,
		})
	}
	// 按 token 降序、小时按时间升序：面板直接照序渲染，不用再排一遍。
	sort.Slice(s.Models, func(i, j int) bool {
		if s.Models[i].TotalTokens == s.Models[j].TotalTokens {
			return s.Models[i].Model < s.Models[j].Model
		}
		return s.Models[i].TotalTokens > s.Models[j].TotalTokens
	})
	sort.Slice(s.Accounts, func(i, j int) bool {
		if s.Accounts[i].TotalTokens == s.Accounts[j].TotalTokens {
			return s.Accounts[i].Account < s.Accounts[j].Account
		}
		return s.Accounts[i].TotalTokens > s.Accounts[j].TotalTokens
	})
	for at, count := range a.hours {
		s.Hours = append(s.Hours, HourStat{Time: at, Count: count})
	}
	sort.Slice(s.Hours, func(i, j int) bool { return s.Hours[i].Time < s.Hours[j].Time })
	return s
}

// SetRetentionDays 热更新归档保留天数（面板保存配置后调用）。后台清理每秒一轮，
// 新值最多一秒后生效，不必触发一次同步清理。
func (r *Recorder) SetRetentionDays(days int) {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.setRetentionDays(days)
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
	// retMu 保护 cfg.RetentionDays：prune 每秒读，面板保存时热更新写。
	retMu sync.Mutex
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = DefaultRetentionDays
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

func (w *archiveWriter) enqueue(e Event) {
	if !w.enabled() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case <-ticker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := "requests-" + day
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		idx := 0
		if name[len(prefix):len(name)-len(".jsonl")] != "" {
			part := strings.TrimSuffix(strings.TrimPrefix(name[len(prefix):], "."), ".jsonl")
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			idx = n
		}
		if idx > bestIndex {
			bestIndex = idx
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return best
}

func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, "requests-"+day)
	path := base + ".jsonl"
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d.jsonl", base, i)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	cutoff := time.Now().AddDate(0, 0, -w.retentionDays())
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime.Equal(items[j].mtime) {
			return items[i].path < items[j].path
		}
		return items[i].mtime.Before(items[j].mtime)
	})
	for _, it := range items {
		if it.path != w.path && (it.mtime.Before(cutoff) || total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
			}
		}
	}
}

func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗），
	// os.ReadDir 的字典序又会把装着最早事件的基准文件 requests-<day>.jsonl 排在
	// requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	var out []Event
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		rows, err := readFile(filepath.Join(w.cfg.Dir, name), filter)
		if err != nil {
			return out, err
		}
		out = append(out, rows...)
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// scan 逐行把窗口内的归档事件交给 fn，不把整批记录留在内存里 —— 30 天的归档可能
// 有几十万条，聚合不需要同时持有它们。单个文件读失败不中断：其余文件的记录照旧算数，
// 最后把第一个错误交回去说明「这份统计不完整」。
func (w *archiveWriter) scan(filter Filter, fn func(Event)) error {
	if !w.enabled() {
		return nil
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if err := scanFile(filepath.Join(w.cfg.Dir, name), filter, fn); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func readFile(path string, filter Filter) ([]Event, error) {
	out := []Event{}
	err := scanFile(path, filter, func(e Event) { out = append(out, e) })
	return out, err
}

func scanFile(path string, filter Filter, fn func(Event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(e) {
			continue
		}
		fn(e)
	}
	return scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.Model, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	s := ArchiveStats{Enabled: w.enabled(), Dir: w.cfg.Dir, DroppedWrites: w.dropped.Load(), LastError: w.errString()}
	if !s.Enabled {
		return s
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			s.Files++
			s.Bytes += info.Size()
		}
	}
	return s
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

// retentionDays 当前保留天数；配置没给（<=0）时回落 DefaultRetentionDays。
func (w *archiveWriter) retentionDays() int {
	w.retMu.Lock()
	defer w.retMu.Unlock()
	if w.cfg.RetentionDays > 0 {
		return w.cfg.RetentionDays
	}
	return DefaultRetentionDays
}

func (w *archiveWriter) setRetentionDays(days int) {
	w.retMu.Lock()
	w.cfg.RetentionDays = days
	w.retMu.Unlock()
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
