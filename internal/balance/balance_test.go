package balance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func checkFloat(t *testing.T, label string, got, want float64) {
	t.Helper()
	const tolerance = 1e-9
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func TestAddThrottleIsPerAccountAndKeepsDecimals(t *testing.T) {
	r := New("")
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	cur := base
	r.clock = func() time.Time { return cur }

	if r.Add("", 1) {
		t.Error("空 UID 应被拒绝")
	}
	if !r.Add("A", 100.25) || !r.Add("B", 200.5) || !r.Add("zero", 0) {
		t.Fatal("不同账号的首次采样及零余额都应被接受")
	}
	if r.Add("A", 101) {
		t.Error("同账号同窗口的重复采样应被节流")
	}
	cur = base.Add(4 * time.Minute)
	if r.Add("A", 102) || r.Add("B", 201) {
		t.Error("4 分钟仍应被节流")
	}
	cur = base.Add(5 * time.Minute)
	if !r.Add("A", 103.75) || !r.Add("B", 201.5) {
		t.Error("各账号满 5 分钟后都应接受采样")
	}

	series, gotNow := r.Snapshot(time.Time{})
	if !gotNow.Equal(cur) {
		t.Errorf("Snapshot now = %v, want injected clock %v", gotNow, cur)
	}
	if len(series) != 3 {
		t.Fatalf("series len = %d, want 3", len(series))
	}
	for _, s := range series {
		switch s.UID {
		case "A":
			checkFloat(t, "A current", s.Current, 103.75)
			checkFloat(t, "A first raw value", s.Points[0].Credits, 100.25)
			checkFloat(t, "A change", s.Change, 3.5)
		case "B":
			checkFloat(t, "B current", s.Current, 201.5)
		case "zero":
			checkFloat(t, "zero current", s.Current, 0)
		}
	}
}

func TestRecorderConcurrentAdd(t *testing.T) {
	r := New("")
	r.clock = fixedClock(time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local))
	const accounts = 24
	var wg sync.WaitGroup
	accepted := make(chan bool, accounts)
	for i := 0; i < accounts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			accepted <- r.Add(fmt.Sprintf("u%02d", i), float64(i)+0.25)
		}(i)
	}
	wg.Wait()
	close(accepted)
	for ok := range accepted {
		if !ok {
			t.Error("不同账号的并发首次采样都应被接受")
		}
	}
	if len(r.points) != accounts {
		t.Fatalf("points = %d, want %d", len(r.points), accounts)
	}

	same := New("")
	same.clock = r.clock
	const callers = 24
	accepted = make(chan bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			accepted <- same.Add("same", 1.25)
		}()
	}
	wg.Wait()
	close(accepted)
	count := 0
	for ok := range accepted {
		if ok {
			count++
		}
	}
	if count != 1 || len(same.points) != 1 {
		t.Errorf("同账号同窗口并发采样接受 %d 次、%d 点, want 1/1", count, len(same.points))
	}
}

func TestPersistReloadAndRestoreThrottle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "balance_history.json")
	base := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	cur := base
	r := New(path)
	r.clock = func() time.Time { return cur }
	if !r.Add("A", 1234.75) {
		t.Fatal("Add A failed")
	}
	cur = base.Add(10 * time.Minute)
	if !r.Add("B", 2000.5) {
		t.Fatal("Add B failed")
	}
	r.Save()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	var saved file
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("decode saved file: %v", err)
	}
	if saved.Version != 1 || len(saved.Points) != 2 {
		t.Fatalf("saved file = version %d, %d points; want version 1, 2 points", saved.Version, len(saved.Points))
	}
	checkFloat(t, "persisted decimal", saved.Points[0].Credits, 1234.75)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %o, want 600", info.Mode().Perm())
		}
	}

	reloaded := New(path)
	reloaded.clock = fixedClock(base.Add(4 * time.Minute))
	if reloaded.Add("A", 1) {
		t.Fatal("A 在重载前采样后仅 4 分钟，应被节流")
	}
	reloaded.clock = fixedClock(base.Add(5 * time.Minute))
	if !reloaded.Add("A", 1235.75) {
		t.Fatal("A 在重载前采样后满 5 分钟，应接受")
	}
	if reloaded.Add("A", 1) {
		t.Fatal("A 的新采样在同窗口内应被节流")
	}
	series, _ := reloaded.Snapshot(time.Time{})
	if len(series) != 2 {
		t.Fatalf("unexpected reloaded series: %+v", series)
	}
	for _, s := range series {
		if s.UID == "A" && s.Current != 1235.75 {
			t.Fatalf("unexpected reloaded A balance: %+v", s)
		}
	}
}

func TestRollupPreservesFirstLastAndFlows(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New("")
	// 500.5 -> 480.25 -> 600 -> 600 -> 550.75。
	r.points = []Point{
		{TS: base.Unix(), UID: "u1", Credits: 500.5},
		{TS: base.Add(2 * time.Hour).Unix(), UID: "u1", Credits: 480.25},
		{TS: base.Add(4 * time.Hour).Unix(), UID: "u1", Credits: 600},
		{TS: base.Add(6 * time.Hour).Unix(), UID: "u1", Credits: 600},
		{TS: base.Add(8 * time.Hour).Unix(), UID: "u1", Credits: 550.75},
	}
	now := base.Add(31 * 24 * time.Hour)
	r.clock = fixedClock(now)
	r.Rollup(now)
	if len(r.points) != 1 {
		t.Fatalf("folded points = %d, want 1", len(r.points))
	}
	p := r.points[0]
	if p.Day != "2026-08-01" {
		t.Errorf("Day = %q, want 2026-08-01", p.Day)
	}
	checkFloat(t, "folded first", p.First, 500.5)
	checkFloat(t, "folded last", p.Credits, 550.75)
	checkFloat(t, "folded consumed", p.Consumed, 69.5)
	checkFloat(t, "folded gained", p.Gained, 119.75)

	series, _ := r.Snapshot(time.Time{})
	if len(series) != 1 || len(series[0].Daily) != 1 {
		t.Fatalf("Snapshot = %+v, want one series and one daily", series)
	}
	d := series[0].Daily[0]
	checkFloat(t, "daily first", d.First, 500.5)
	checkFloat(t, "daily last", d.Last, 550.75)
	checkFloat(t, "daily change", d.Change, 50.25)
	checkFloat(t, "daily consumed", d.Consumed, 69.5)
	checkFloat(t, "daily gained", d.Gained, 119.75)
	if !d.Full {
		t.Error("非今日的折叠日应视为完整，即使只剩一个日点")
	}
}

func TestFoldedZeroFirstSurvivesSnapshotAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "balance.json")
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	r := New(path)
	r.points = []Point{
		{TS: base.Unix(), UID: "u1", Credits: 0},
		{TS: base.Add(time.Hour).Unix(), UID: "u1", Credits: 12.5},
		{TS: base.Add(2 * time.Hour).Unix(), UID: "u1", Credits: 5.25},
	}
	now := base.Add(31 * 24 * time.Hour)
	r.clock = fixedClock(now)
	r.Rollup(now)
	if len(r.points) != 1 {
		t.Fatalf("folded points = %d, want 1", len(r.points))
	}
	checkFloat(t, "stored zero first", r.points[0].First, 0)
	r.Save()

	reloaded := New(path)
	reloaded.clock = fixedClock(now)
	series, _ := reloaded.Snapshot(time.Time{})
	if len(series) != 1 {
		t.Fatalf("series len = %d, want 1", len(series))
	}
	s := series[0]
	checkFloat(t, "series change from zero", s.Change, 5.25)
	checkFloat(t, "series min includes zero", s.Min, 0)
	checkFloat(t, "series max", s.Max, 5.25)
	if len(s.Daily) != 1 {
		t.Fatalf("daily len = %d, want 1", len(s.Daily))
	}
	d := s.Daily[0]
	checkFloat(t, "daily zero first", d.First, 0)
	checkFloat(t, "daily last", d.Last, 5.25)
	checkFloat(t, "daily change from zero", d.Change, 5.25)
	checkFloat(t, "daily min includes zero", d.Min, 0)
	checkFloat(t, "daily consumed", d.Consumed, 7.25)
	checkFloat(t, "daily gained", d.Gained, 12.5)
}

func TestDailyFullUsesRawCountAndExcludesToday(t *testing.T) {
	day1 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.Local)
	day2 := day1.Add(24 * time.Hour)
	today := day2.Add(24 * time.Hour)
	points := make([]Point, 0, expectedPerDay*2+expectedPerDay-1)
	addSamples := func(day time.Time, n int) {
		for i := 0; i < n; i++ {
			points = append(points, Point{TS: day.Add(time.Duration(i) * 30 * time.Minute).Unix(), Credits: float64(i)})
		}
	}
	addSamples(day1, expectedPerDay)
	addSamples(day2, expectedPerDay-1)
	addSamples(today, expectedPerDay)
	daily := dailyOf(points, today.Add(12*time.Hour))
	if len(daily) != 3 {
		t.Fatalf("daily len = %d, want 3", len(daily))
	}
	if !daily[0].Full {
		t.Error("非今日且原始点数达到 40，应为 Full")
	}
	if daily[1].Full {
		t.Error("非今日但原始点数不足 40，不应为 Full")
	}
	if daily[2].Full {
		t.Error("今日即使达到 40 点也不应为 Full")
	}
}

func TestSnapshotWindowStatsAndSort(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	r := New("")
	r.clock = fixedClock(now)
	r.points = []Point{
		{TS: now.Add(-48 * time.Hour).Unix(), UID: "u1", Credits: 1.25},
		{TS: now.Add(-2 * time.Hour).Unix(), UID: "u1", Credits: 10.5},
		{TS: now.Add(-time.Hour).Unix(), UID: "u1", Credits: 8.25},
		{TS: now.Unix(), UID: "u3", Credits: 20.5},
		{TS: now.Unix(), UID: "u2", Credits: 20.5},
	}
	r.SetName("u1", "主号")
	series, gotNow := r.Snapshot(now.Add(-2 * time.Hour))
	if !gotNow.Equal(now) {
		t.Errorf("now = %v, want %v", gotNow, now)
	}
	if len(series) != 3 {
		t.Fatalf("series len = %d, want 3", len(series))
	}
	if series[0].UID != "u2" || series[1].UID != "u3" || series[2].UID != "u1" {
		t.Errorf("排序 = %s/%s/%s, want u2/u3/u1", series[0].UID, series[1].UID, series[2].UID)
	}
	s := series[2]
	if s.Name != "主号" || len(s.Points) != 2 {
		t.Errorf("窗口或昵称错误: %+v", s)
	}
	checkFloat(t, "window current", s.Current, 8.25)
	checkFloat(t, "window min", s.Min, 8.25)
	checkFloat(t, "window max", s.Max, 10.5)
	checkFloat(t, "window change", s.Change, -2.25)
	if len(s.Daily) != 1 || s.Daily[0].N != 2 {
		t.Errorf("窗口 Daily 应只含两个原始点: %+v", s.Daily)
	}
}

func TestRollupPrunesExpiredDailyPoints(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	old := now.Add(-400 * 24 * time.Hour)
	r := New("")
	r.points = []Point{
		{TS: old.Unix(), UID: "u1", Credits: 1, Day: old.Format(dayLayout)},
		{TS: now.Add(-time.Hour).Unix(), UID: "u1", Credits: 2.5},
	}
	r.Rollup(now)
	if len(r.points) != 1 || r.points[0].Day != "" {
		t.Errorf("应只保留近期原始点: %+v", r.points)
	}
}

func TestLoadSkipsInvalidPoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "balance.json")
	raw := `{"version":1,"points":[{"t":0,"u":"u1","c":1},{"t":100,"u":"","c":2},{"t":200,"u":"u2","c":3.5}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	if len(r.points) != 1 || r.points[0].UID != "u2" {
		t.Fatalf("非法点应被过滤: %+v", r.points)
	}
	checkFloat(t, "valid decimal", r.points[0].Credits, 3.5)
}

func TestNilAndLifecycleSafety(t *testing.T) {
	var nilRecorder *Recorder
	if nilRecorder.Add("u1", 1) {
		t.Error("nil Add 应返回 false")
	}
	nilRecorder.SetName("u1", "name")
	nilRecorder.Rollup(time.Now())
	nilRecorder.Start()
	nilRecorder.Stop()
	nilRecorder.Save()
	series, now := nilRecorder.Snapshot(time.Time{})
	if series != nil || !now.IsZero() {
		t.Errorf("nil Snapshot = %v, %v; want nil, zero", series, now)
	}

	path := filepath.Join(t.TempDir(), "balance.json")
	r := New(path)
	r.clock = fixedClock(time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local))
	r.Add("u1", 1.25)
	stopped := make(chan struct{})
	go func() {
		r.Stop() // 未 Start 的 Stop 必须最终落盘并返回。
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop deadlocked when Start was not called")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Stop without Start should save: %v", err)
	}
	r.Start() // Stop 后的 Start 是空操作。
	r.Stop()  // 重复 Stop 安全。

	started := New("")
	started.Start()
	started.Start()
	started.Stop()
	started.Stop()
}

func TestJSONFieldNames(t *testing.T) {
	pointJSON, err := json.Marshal(Point{TS: 1, UID: "u", Credits: 2.5, Day: "2026-01-01", Consumed: 1.25, Gained: 3.75, First: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	var pointFields map[string]any
	if err := json.Unmarshal(pointJSON, &pointFields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"t", "u", "c", "d", "cons", "gain", "f"} {
		if _, ok := pointFields[key]; !ok {
			t.Errorf("Point JSON 缺少字段 %q: %s", key, pointJSON)
		}
	}
	if len(pointFields) != 7 {
		t.Errorf("Point JSON 字段数 = %d, want 7: %s", len(pointFields), pointJSON)
	}

	seriesJSON, err := json.Marshal(Series{UID: "u", Name: "n", Points: []Point{}, Daily: []Daily{{Day: "2026-01-01"}}})
	if err != nil {
		t.Fatal(err)
	}
	var seriesFields map[string]any
	if err := json.Unmarshal(seriesJSON, &seriesFields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"uid", "name", "points", "current", "min", "max", "change", "daily"} {
		if _, ok := seriesFields[key]; !ok {
			t.Errorf("Series JSON 缺少字段 %q: %s", key, seriesJSON)
		}
	}
	if len(seriesFields) != 8 {
		t.Errorf("Series JSON 字段数 = %d, want 8: %s", len(seriesFields), seriesJSON)
	}
	if _, ok := seriesFields["realm"]; ok {
		t.Errorf("Series 不应包含 realm: %s", seriesJSON)
	}
}
