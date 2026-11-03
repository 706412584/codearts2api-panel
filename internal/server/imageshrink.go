package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"

	"codearts2api-panel/internal/imgshrink"
)

// shrinkStats 是一次「按需压图」的结果汇总（用于日志与测试）。
type shrinkStats struct {
	Changed bool
	Images  int
	Before  int
	After   int
}

// 代理类会话同一张截图会在每一轮请求里重复出现（客户端每次都重发整段历史），
// 缩图结果必须缓存，否则每轮都白烧一次解码+缩放。
// 只存「已压好的 data URL」，按条目数与总字节双限额做 FIFO 淘汰。
const (
	imgCacheMaxEntries = 64
	imgCacheMaxBytes   = 64 << 20
)

type shrinkCache struct {
	mu    sync.Mutex
	order []string
	items map[string]string
	bytes int
}

var imageCache = &shrinkCache{items: map[string]string{}}

// cacheKey 把「图片内容 + 生效参数」一起做键：换预算/质量必须重新压。
func cacheKey(url string, opt imgshrink.Options) string {
	sum := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%s|%d|%d|%d", hex.EncodeToString(sum[:16]), opt.TargetBytes, opt.MaxDimension, opt.Quality)
}

func (c *shrinkCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	return v, ok
}

func (c *shrinkCache) put(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[key]; exists {
		return
	}
	c.items[key] = value
	c.order = append(c.order, key)
	c.bytes += len(value)
	for len(c.order) > imgCacheMaxEntries || (c.bytes > imgCacheMaxBytes && len(c.order) > 1) {
		oldest := c.order[0]
		c.order = c.order[1:]
		if old, ok := c.items[oldest]; ok {
			c.bytes -= len(old)
			delete(c.items, oldest)
		}
	}
}

// shrinkMessageImages 在把请求转给上游之前，把内联图里超预算的压小。
//
// 为什么必须有这一步：上游媒体转换对单张图片有硬上限（实测请求体约 4.3MB /
// 图片约 3.2MB），超了回 `ModelArts.81029 ... downstream returned 413`，
// 网关把 body 上限放宽也没用 —— 只能压小再发。
//
// 只在「请求体超阈值 / 合计超预算 / 单张超预算」时才动手，正常小图一个字节都不碰。
func shrinkMessageImages(req *chatRequest, policy imgshrink.Policy, bodyLen int64) shrinkStats {
	var st shrinkStats
	policy = policy.Normalized()
	if req == nil {
		return st
	}

	type ref struct {
		holder map[string]any // image_url 对象本体
		url    string
		size   int // base64 解码后的估算字节数
	}
	var refs []ref
	total := 0
	for _, m := range req.Messages {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := part["type"].(string); kind != "image_url" {
				continue
			}
			img, ok := part["image_url"].(map[string]any)
			if !ok {
				continue
			}
			url, _ := img["url"].(string)
			mediaType, payload, ok := imgshrink.IsDataURL(url)
			// webp / gif 等标准库解不了的格式原样放过（宁可少压，不能压坏）。
			if !ok || (mediaType != "image/png" && mediaType != "image/jpeg") {
				continue
			}
			size := len(payload) * 3 / 4
			refs = append(refs, ref{holder: img, url: url, size: size})
			total += size
		}
	}
	if len(refs) == 0 {
		return st
	}

	oversize := bodyLen > policy.TriggerBytes || total > policy.TargetTotalBytes
	if !oversize {
		for _, r := range refs {
			if r.size > policy.TargetImageBytes {
				oversize = true
				break
			}
		}
	}
	if !oversize {
		return st
	}

	// 单张预算还要被「合计预算 ÷ 图片数」夹一次，多图会话才不会各压到 2MB 又超总量。
	budget := policy.TargetImageBytes
	if per := policy.TargetTotalBytes / len(refs); per < budget {
		budget = per
	}
	opt := policy.Options()
	opt.TargetBytes = budget

	for _, r := range refs {
		key := cacheKey(r.url, opt)
		if cached, ok := imageCache.get(key); ok {
			r.holder["url"] = cached
			st.Images++
			st.Before += r.size
			st.After += len(cached) * 3 / 4
			continue
		}
		out, res, err := imgshrink.ShrinkDataURL(r.url, opt)
		if err != nil {
			// 像素数超过护栏（解码会吃掉几百 MB）：原样转发，让上游去判。
			if errors.Is(err, imgshrink.ErrTooLargeToProcess) {
				log.Printf("image shrink 跳过像素数过大的图（约 %s），原样转发", humanBytes(int64(len(r.url))))
			}
			continue
		}
		if !res.Changed {
			continue
		}
		imageCache.put(key, out)
		r.holder["url"] = out
		st.Images++
		st.Before += res.Before
		st.After += res.After
	}
	st.Changed = st.Images > 0
	if st.Changed {
		log.Printf("image shrink images=%d %s -> %s (budget/张=%s, body=%s)",
			st.Images, humanBytes(int64(st.Before)), humanBytes(int64(st.After)),
			humanBytes(int64(budget)), humanBytes(bodyLen))
	}
	return st
}

// humanBytes 把字节数写成 32MB / 512KB / 800B 这类直观单位（错误提示/日志用）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%dMB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dKB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
