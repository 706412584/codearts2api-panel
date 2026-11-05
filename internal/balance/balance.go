// Package balance 记录账号积分水位，并提供余额趋势与每日变化快照。
package balance

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	sampleInterval  = 5 * time.Minute
	refreshInterval = 30 * time.Minute
	detailedKeep    = 30 * 24 * time.Hour
	dayKeep         = 365 * 24 * time.Hour
	flushInterval   = 30 * time.Second
	maxPoints       = 200_000
	dayLayout       = "2006-01-02"
	expectedPerDay  = 40
)

// Point 是一次原始采样；Day 非空时表示按账号和本地日折叠后的日点。
type Point struct {
	TS       int64   `json:"t"`
	UID      string  `json:"u"`
	Credits  float64 `json:"c"`
	Day      string  `json:"d,omitempty"`
	Consumed float64 `json:"cons,omitempty"`
	Gained   float64 `json:"gain,omitempty"`
	First    float64 `json:"f,omitempty"`
}

type file struct {
	Version int     `json:"version"`
	Saved   string  `json:"saved"`
	Points  []Point `json:"points"`
}

// Recorder 是并发安全的余额采样记录器。
type Recorder struct {
	mu         sync.Mutex
	path       string
	points     []Point
	dirty      bool
	lastSample map[string]time.Time
	names      map[string]string
	clock      func() time.Time

	saveMu   sync.Mutex
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	started  bool
	stopped  bool
}

// New 构造记录器。path 为空时仅保存在内存中。
func New(path string) *Recorder {
	r := &Recorder{
		path:       path,
		lastSample: make(map[string]time.Time),
		names:      make(map[string]string),
		clock:      time.Now,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[balance] 载入失败（按空历史启动）: %v", err)
		}
	}
	return r
}

// Interval 返回上游余额刷新间隔，与历史采样去重窗口独立。
func Interval() time.Duration { return refreshInterval }

// SetName 注入仅用于展示的账号昵称，不持久化。
func (r *Recorder) SetName(uid, name string) {
	if r == nil || uid == "" {
		return
	}
	r.mu.Lock()
	r.names[uid] = name
	r.mu.Unlock()
}

// Add 记录一次采样；同一账号 5 分钟内的重复采样会被节流。
func (r *Recorder) Add(uid string, credits float64) bool {
	if r == nil || uid == "" {
		return false
	}
	now := r.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastSample[uid]; ok && now.Sub(last) < sampleInterval {
		return false
	}
	r.lastSample[uid] = now
	r.points = append(r.points, Point{TS: now.Unix(), UID: uid, Credits: credits})
	r.dirty = true
	if len(r.points) > maxPoints {
		r.rollupLocked(now)
	}
	return true
}

// Rollup 将超过 30 天的原始点折叠为每账号、每本地日一个日点。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.rollupLocked(now)
	r.mu.Unlock()
}

func (r *Recorder) rollupLocked(now time.Time) {
	cutoff := now.Add(-detailedKeep).Unix()
	type key struct {
		uid string
		day string
	}
	type fold struct {
		last            Point
		first, consumed float64
		gained, prev    float64
		hasPrev         bool
	}
	folds := make(map[key]*fold)
	var order []key
	kept := make([]Point, 0, len(r.points))
	folded := 0
	for _, p := range r.points {
		if p.Day != "" || p.TS >= cutoff {
			kept = append(kept, p)
			continue
		}
		day := time.Unix(p.TS, 0).In(time.Local).Format(dayLayout)
		k := key{uid: p.UID, day: day}
		f, ok := folds[k]
		if !ok {
			f = &fold{first: p.Credits}
			folds[k] = f
			order = append(order, k)
		}
		if f.hasPrev {
			delta := p.Credits - f.prev
			if delta > 0 {
				f.gained += delta
			} else if delta < 0 {
				f.consumed -= delta
			}
		}
		f.prev, f.hasPrev = p.Credits, true
		if !ok || p.TS > f.last.TS {
			f.last = p
		}
		folded++
	}
	if folded > 0 {
		for _, k := range order {
			f := folds[k]
			kept = append(kept, Point{
				TS: f.last.TS, UID: f.last.UID, Credits: f.last.Credits, Day: k.day,
				First: f.first, Consumed: f.consumed, Gained: f.gained,
			})
		}
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].TS < kept[j].TS })
		r.points = kept
		r.dirty = true
		log.Printf("[balance] 折叠 %d 个原始采样点为 %d 个日点", folded, len(order))
	}
	r.pruneDaysLocked(now)
}

func (r *Recorder) pruneDaysLocked(now time.Time) {
	cutoff := now.Add(-dayKeep)
	kept := make([]Point, 0, len(r.points))
	dropped := 0
	for _, p := range r.points {
		if p.Day != "" {
			day, err := time.ParseInLocation(dayLayout, p.Day, time.Local)
			if err == nil && day.Before(cutoff) {
				dropped++
				continue
			}
		}
		kept = append(kept, p)
	}
	if dropped > 0 {
		r.points = kept
		r.dirty = true
	}
}

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	if f.Version != 1 {
		return fmt.Errorf("不支持的版本 %d", f.Version)
	}
	points := make([]Point, 0, len(f.Points))
	for _, p := range f.Points {
		if p.UID == "" || p.TS <= 0 {
			continue
		}
		points = append(points, p)
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].TS < points[j].TS })
	r.points = points
	for _, p := range points {
		if p.Day != "" {
			continue
		}
		at := time.Unix(p.TS, 0)
		if previous, ok := r.lastSample[p.UID]; !ok || at.After(previous) {
			r.lastSample[p.UID] = at
		}
	}
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	// 串行化写盘，避免并发 Save 与后台刷新互相覆盖临时文件或清除 dirty。
	r.saveMu.Lock()
	defer r.saveMu.Unlock()

	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{
		Version: 1,
		Saved:   r.clock().Format(time.RFC3339),
		Points:  append([]Point(nil), r.points...),
	}
	r.dirty = false
	r.mu.Unlock()

	saved := false
	defer func() {
		if !saved {
			r.mu.Lock()
			r.dirty = true
			r.mu.Unlock()
		}
	}()
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[balance] 序列化失败: %v", err)
		return
	}
	if err := writeAtomic(r.path, raw); err != nil {
		log.Printf("[balance] 落盘失败: %v", err)
		return
	}
	saved = true
}

func writeAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Save 立即落盘；纯内存记录器为空操作。
func (r *Recorder) Save() { r.flush(true) }

// Start 启动周期折叠和 30 秒防抖落盘。重复调用不会启动多个协程。
func (r *Recorder) Start() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-ticker.C:
				r.Rollup(r.clock())
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台协程并最终落盘；未 Start 时也不会阻塞。
func (r *Recorder) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	r.stopped = true
	started := r.started
	r.mu.Unlock()
	if started {
		<-r.done
		return
	}
	r.flush(true)
}

// Series 是单账号的余额序列。
type Series struct {
	UID     string  `json:"uid"`
	Name    string  `json:"name,omitempty"`
	Points  []Point `json:"points"`
	Current float64 `json:"current"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Change  float64 `json:"change"`
	Daily   []Daily `json:"daily,omitempty"`
}

// Daily 是单账号某本地日的首末水位及采样内可观测的消耗和到账。
type Daily struct {
	Day      string  `json:"day"`
	First    float64 `json:"first"`
	Last     float64 `json:"last"`
	Change   float64 `json:"change"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	N        int     `json:"n"`
	Full     bool    `json:"full"`
	Consumed float64 `json:"consumed"`
	Gained   float64 `json:"gained"`
}

// Snapshot 返回 since（含）之后的序列；since 为零值时返回全部。
func (r *Recorder) Snapshot(since time.Time) ([]Series, time.Time) {
	if r == nil {
		return nil, time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	byUID := make(map[string][]Point)
	for _, p := range r.points {
		if !since.IsZero() && p.TS < since.Unix() {
			continue
		}
		byUID[p.UID] = append(byUID[p.UID], p)
	}
	out := make([]Series, 0, len(byUID))
	for uid, points := range byUID {
		sort.SliceStable(points, func(i, j int) bool { return points[i].TS < points[j].TS })
		s := Series{UID: uid, Name: r.names[uid], Points: points}
		last := points[len(points)-1]
		s.Current = last.Credits
		lo, hi := points[0].Credits, points[0].Credits
		for _, p := range points {
			lo = minFloat(lo, p.Credits)
			hi = maxFloat(hi, p.Credits)
			if p.Day != "" {
				lo = minFloat(lo, p.First)
				hi = maxFloat(hi, p.First)
			}
		}
		s.Min, s.Max = lo, hi
		first := points[0].Credits
		if points[0].Day != "" {
			first = points[0].First
		}
		s.Change = s.Current - first
		s.Daily = dailyOf(points, now)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current > out[j].Current
		}
		return out[i].UID < out[j].UID
	})
	return out, now
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func dailyOf(points []Point, now time.Time) []Daily {
	if len(points) == 0 {
		return nil
	}
	type acc struct {
		first, last, min, max float64
		n                     int
		consumed, gained      float64
		prev                  float64
		hasPrev, folded       bool
	}
	byDay := make(map[string]*acc)
	var days []string
	for _, p := range points {
		day := p.Day
		if day == "" {
			day = time.Unix(p.TS, 0).In(time.Local).Format(dayLayout)
		}
		first, lo, hi := p.Credits, p.Credits, p.Credits
		if p.Day != "" {
			// Day 非空即说明 First 有效；0 是合法首值，不能用它判断字段是否存在。
			first = p.First
			lo = minFloat(lo, first)
			hi = maxFloat(hi, first)
		}
		a, ok := byDay[day]
		if !ok {
			a = &acc{first: first, min: lo, max: hi}
			byDay[day] = a
			days = append(days, day)
		}
		a.last = p.Credits
		a.min = minFloat(a.min, lo)
		a.max = maxFloat(a.max, hi)
		if a.hasPrev {
			delta := p.Credits - a.prev
			if delta > 0 {
				a.gained += delta
			} else if delta < 0 {
				a.consumed -= delta
			}
		}
		a.consumed += p.Consumed
		a.gained += p.Gained
		a.prev, a.hasPrev = p.Credits, true
		a.folded = a.folded || p.Day != ""
		a.n++
	}
	sort.Strings(days)
	today := now.In(time.Local).Format(dayLayout)
	out := make([]Daily, 0, len(days))
	for _, day := range days {
		a := byDay[day]
		out = append(out, Daily{
			Day: day, First: a.first, Last: a.last, Change: a.last - a.first,
			Min: a.min, Max: a.max, N: a.n,
			Full:     day != today && (a.folded || a.n >= expectedPerDay),
			Consumed: a.consumed, Gained: a.gained,
		})
	}
	return out
}

// Describe 返回一行状态摘要。
func (r *Recorder) Describe() string {
	if r == nil {
		return "off"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	uids := make(map[string]struct{})
	raw, daily := 0, 0
	for _, p := range r.points {
		uids[p.UID] = struct{}{}
		if p.Day == "" {
			raw++
		} else {
			daily++
		}
	}
	return fmt.Sprintf("%d 点（原始 %d / 日粒度 %d）, %d 账号", len(r.points), raw, daily, len(uids))
}
