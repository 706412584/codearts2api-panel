package server

import (
	"encoding/json"
	"strings"
	"testing"

	"codearts2api-panel/internal/reqlog"
)

// 缓存未命中数由 prompt - hit 推出：面板的命中率要用 hit/(hit+miss) 做分母，
// 只留命中数的话「命中 0」和「上游没上报缓存」在归档里长得一样。
// 上游没报缓存时两个字段都必须空着，不能被当成命中率 0%。
func TestCaptureUsageCacheTokens(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		hit   int64
		miss  int64
	}{
		{
			name:  "prompt_tokens_details",
			usage: map[string]any{"prompt_tokens": float64(1000), "prompt_tokens_details": map[string]any{"cached_tokens": float64(800)}},
			hit:   800, miss: 200,
		},
		{
			name:  "prompt_cache_hit_tokens",
			usage: map[string]any{"prompt_tokens": float64(1000), "prompt_cache_hit_tokens": float64(300)},
			hit:   300, miss: 700,
		},
		{
			// 全命中：miss 为 0 且被 omitempty 省掉，命中数仍在，不会退化成「没上报」。
			name:  "full_hit",
			usage: map[string]any{"prompt_tokens": float64(500), "prompt_cache_hit_tokens": float64(500)},
			hit:   500, miss: 0,
		},
		{
			// 上游数字比 prompt 还大时不能算出负的未命中数。
			name:  "hit_exceeds_prompt",
			usage: map[string]any{"prompt_tokens": float64(500), "prompt_cache_hit_tokens": float64(800)},
			hit:   800, miss: 0,
		},
		{
			name:  "no_cache_report",
			usage: map[string]any{"prompt_tokens": float64(500), "completion_tokens": float64(20)},
			hit:   0, miss: 0,
		},
	}
	for _, tc := range cases {
		var event reqlog.Event
		captureUsage(&event, tc.usage)
		if event.CacheHitTokens != tc.hit || event.CacheMissTokens != tc.miss {
			t.Errorf("%s: hit/miss = %d/%d, want %d/%d", tc.name, event.CacheHitTokens, event.CacheMissTokens, tc.hit, tc.miss)
		}
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		// 没上报缓存的请求不能带上半个字段：面板据此判断「这条记录参不参与命中率」。
		if strings.Contains(string(raw), `"cache_hit_tokens"`) != (tc.hit > 0) {
			t.Errorf("%s: cache_hit_tokens 落盘与否不对: %s", tc.name, raw)
		}
		if strings.Contains(string(raw), `"cache_miss_tokens"`) != (tc.miss > 0) {
			t.Errorf("%s: cache_miss_tokens 落盘与否不对: %s", tc.name, raw)
		}
	}
}
