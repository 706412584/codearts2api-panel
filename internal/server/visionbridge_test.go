package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

const (
	testVLModel   = "Qwen3-VL-235B"
	testTextModel = "deepseek-v4.1-flash-1017"
	testDesc      = "一张彩色噪点图，主要颜色是蓝、紫、粉、绿、黄、青。"
)

type visionBackend struct {
	vlCalls   atomic.Int32
	vlFail    atomic.Int32 // >0 表示接下来的 VL 调用要按「并发会话已达上限」失败
	vlFailAt  atomic.Int32 // 第 N 次（1-based）VL 调用直接 500 失败（0=不启用）
	lastVL    []byte
	lastMain  []byte
	mu        sync.Mutex
	mainCalls int
	vlChatIDs []string
}

// resetVisionCache 清掉转述缓存，保证各测试互不干扰（同一张噪点图字节完全一样）。
func resetVisionCache() {
	visionCache.mu.Lock()
	visionCache.items = map[string]string{}
	visionCache.order = nil
	visionCache.bytes = 0
	visionCache.mu.Unlock()
}

// fakeVisionBackend 同时扮演 VL 模型与纯文本模型：按请求里的 model 分流。
func fakeVisionBackend(t *testing.T) (*httptest.Server, *visionBackend) {
	t.Helper()
	vb := &visionBackend{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		if req.Model == testVLModel {
			vb.vlCalls.Add(1)
			var meta struct {
				ChatID string `json:"chat_id"`
			}
			_ = json.Unmarshal(body, &meta)
			vb.mu.Lock()
			vb.lastVL = body
			vb.vlChatIDs = append(vb.vlChatIDs, meta.ChatID)
			vb.mu.Unlock()
			if vb.vlFail.Load() > 0 {
				vb.vlFail.Add(-1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)，请关闭部分会话后重试。"}`)
				return
			}
			if idx := int(vb.vlFailAt.Load()); idx > 0 && int(vb.vlCalls.Load()) == idx {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "boom")
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", testDesc)
			return
		}
		vb.mu.Lock()
		vb.lastMain = body
		vb.mainCalls++
		vb.mu.Unlock()
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	return srv, vb
}

// visionTestHandler 建一个 handler，并把上游模型目录种成「文本模型不会看图 / VL 模型会看图」。
func visionTestHandler(t *testing.T, endpoint string, policy *VisionBridgePolicy) *Handler {
	t.Helper()
	auths := []*auth.Auth{
		auth.New(t.Name()+"-a", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", ""),
	}
	p, err := pool.New(auths, pool.Config{MaxConcurrent: 1, ErrThreshold: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, acct := range p.Accounts() {
		acct.Client = upstream.NewWithChatEndpoint(5*time.Second, endpoint)
	}
	// 种子目录：避免测试去真拉上游；同时让「模型能力」有明确定论。
	no, yes := false, true
	upstream.SetAccountModels(p.Accounts()[0].UID, []upstream.ModelInfo{
		{ID: testTextModel, SupportsImages: &no},
		{ID: testVLModel, SupportsImages: &yes},
	})
	// 全局目录缓存必须清掉，否则别的测试留下的快照会让能力查询落空。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	return NewHandler(Config{
		Pool: p, DefaultModel: testTextModel, MaxRotate: 2, ErrThreshold: 1,
		VisionBridge: policy,
	})
}

func visionPayload(model string, b64 string) string {
	return fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"这张图说什么"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, model, b64)
}

// 纯文本模型 + 带图：应当先由 VL 模型转述，再把「文字版图片」交给原模型。
func TestVisionBridgeDescribesImageForTextModel(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	resetVisionCache()
	img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{Enabled: true, Model: testVLModel, MaxImages: 4})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(visionPayload(testTextModel, img))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := vb.vlCalls.Load(); got != 1 {
		t.Fatalf("VL 模型应当被调用 1 次，实际 %d", got)
	}
	vb.mu.Lock()
	main := string(vb.lastMain)
	vb.mu.Unlock()
	if strings.Contains(main, "image_url") {
		t.Fatalf("转给纯文本模型的请求里不该再有图片：%s", truncateForTest(main))
	}
	if !strings.Contains(main, "第 1 张图片") || !strings.Contains(main, "蓝、紫、粉、绿、黄、青") {
		t.Fatalf("转述内容没有进到消息里：%s", truncateForTest(main))
	}
}

// 目标模型本身能看图：不许桥接，图片原样透传。
func TestVisionBridgeSkipsVisionCapableModel(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	resetVisionCache()
	img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{Enabled: true, Model: testVLModel})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(visionPayload(testVLModel, img))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := vb.vlCalls.Load(); got != 1 {
		// 主请求本身也是打给 VL 模型的，所以总数应当是 1（没有额外的转述调用）。
		t.Fatalf("VL 调用次数异常：%d（应为 1，即主请求，没有额外桥接）", got)
	}
	vb.mu.Lock()
	sent := string(vb.lastVL)
	vb.mu.Unlock()
	if !strings.Contains(sent, "image_url") {
		t.Fatalf("能看图的模型不该被转述，图片必须原样透传：%s", truncateForTest(sent))
	}
}

// 关掉开关：一切照原样（回滚路径）。
func TestVisionBridgeDisabledPassesThrough(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	resetVisionCache()
	img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{Enabled: false, Model: testVLModel})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(visionPayload(testTextModel, img))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := vb.vlCalls.Load(); got != 0 {
		t.Fatalf("关闭后不应调用 VL：%d", got)
	}
	vb.mu.Lock()
	main := string(vb.lastMain)
	vb.mu.Unlock()
	if !strings.Contains(main, "image_url") {
		t.Fatalf("关闭后图片必须原样透传：%s", truncateForTest(main))
	}
}

// 同一张图重复出现（客户端每轮重放历史）：第二次必须命中缓存，不再打 VL。
func TestVisionBridgeCachesDescription(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	resetVisionCache()
	img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	policy := &VisionBridgePolicy{Enabled: true, Model: testVLModel}
	h := visionTestHandler(t, backend.URL, policy)
	payload := visionPayload(testTextModel, img)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := vb.vlCalls.Load(); got != 1 {
		t.Fatalf("同一张图只应转述一次，实际 %d 次", got)
	}
	vb.mu.Lock()
	main := string(vb.lastMain)
	vb.mu.Unlock()
	if !strings.Contains(main, "蓝、紫、粉、绿、黄、青") {
		t.Fatalf("第二次也应带上（缓存来的）转述：%s", truncateForTest(main))
	}
}

func truncateForTest(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// 上游对每个账号只允许 3 个并发会话，识图子请求很容易撞 TM.00001041。
// 必须自己排队重试（并让出账号槽位），否则整次带图请求就退化成 406。
func TestVisionBridgeRetriesOnConcurrentLimit(t *testing.T) {
	oldDelay := visionQueueRetryDelay
	visionQueueRetryDelay = 10 * time.Millisecond
	defer func() { visionQueueRetryDelay = oldDelay }()

	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	vb.vlFail.Store(1) // 第一次识图先撞并发上限

	resetVisionCache()
	img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{Enabled: true, Model: testVLModel, Timeout: 5 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(visionPayload(testTextModel, img))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := vb.vlCalls.Load(); got != 2 {
		t.Fatalf("撞并发上限后应重试一次，VL 调用数=%d", got)
	}
	vb.mu.Lock()
	main := string(vb.lastMain)
	vb.mu.Unlock()
	if !strings.Contains(main, "蓝、紫、粉、绿、黄、青") {
		t.Fatalf("重试成功后描述没有进到消息里：%s", truncateForTest(main))
	}
}

// 识图子请求必须复用固定的会话 id：每次新建会把上游 3 个会话槽位吃满。
func TestVisionBridgeUsesStableChatID(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{Enabled: true, Model: testVLModel, Timeout: 5 * time.Second})

	// 两张不同的图 → 两次识图调用 → chat_id 必须一致。
	for _, size := range []int{64, 65} {
		resetVisionCache()
		img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, size, size))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(visionPayload(testTextModel, img))))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	vb.mu.Lock()
	ids := append([]string(nil), vb.vlChatIDs...)
	vb.mu.Unlock()
	if len(ids) != 2 {
		t.Fatalf("应有 2 次识图调用，实际 %d", len(ids))
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("识图会话 id 必须固定复用，实际 %v", ids)
	}
	if len(ids[0]) != 32 {
		t.Fatalf("会话 id 应为 32 位十六进制，实际 %q", ids[0])
	}
}

// 多图时第 2 张失败：必须整单不落地 —— 不能留下「前一张变文字、后一张还是图」的畸形请求。
func TestVisionBridgeAllOrNothingOnFailure(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	vb.vlFailAt.Store(2)

	resetVisionCache()
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{Enabled: true, Model: testVLModel, Timeout: 5 * time.Second})
	img1 := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	img2 := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 66, 66))
	payload := fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"看这两张"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, testTextModel, img1, img2)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))

	vb.mu.Lock()
	main := string(vb.lastMain)
	vb.mu.Unlock()
	// 注意别数 "image_url"：每个图片块里它出现两次（type 与 image_url 键），要数真正的图片载荷。
	if got := strings.Count(main, "data:image/png;base64,"); got != 2 {
		t.Fatalf("失败后应当原样保留两张图，实际图片数=%d（出现半替换）", got)
	}
	if strings.Contains(main, "第 1 张图片") {
		t.Fatal("失败时不该把第一张已经替换成文字")
	}
}

// 识图失败必须把账号槽位还回去，不能把池子的 active_concurrent 漏掉。
func TestVisionBridgeReleasesPoolSlotOnFailure(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	vb.vlFailAt.Store(1)

	resetVisionCache()
	img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{Enabled: true, Model: testVLModel, Timeout: 5 * time.Second})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(visionPayload(testTextModel, img))))

	total, _, _, cooling, _ := h.cfg.Pool.Stats()
	if total != 1 || cooling != 0 {
		t.Fatalf("账号状态被污染：total=%d cooling=%d", total, cooling)
	}
	for _, st := range h.cfg.Pool.List() {
		if st["active_concurrent"] != 0 {
			t.Fatalf("识图失败后槽位没还：%#v", st)
		}
		if st["err_count"] != 0 {
			t.Fatalf("识图失败不该给账号记错误：%#v", st)
		}
	}
}

// 描述超长时按字符截断：按字节切会把汉字劈成 U+FFFD。
func TestVisionBridgeTruncatesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("图", 50) // 150 字节
	got := truncateRunes(long, 10)
	if len([]rune(got)) != 10 {
		t.Fatalf("应保留 10 个字符，实际 %d", len([]rune(got)))
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatal("出现了替换字符，说明切在了字符中间")
	}
	// 字节数没超时原样返回（快速路径）。
	if same := truncateRunes("短文本", 100); same != "短文本" {
		t.Fatalf("不该改动：%q", same)
	}
}

// 总预算耗尽时必须整单不落地（不能把前几张替换掉、后几张留着图）。
func TestVisionBridgeStopsWhenBudgetExhausted(t *testing.T) {
	backend, vb := fakeVisionBackend(t)
	defer backend.Close()
	resetVisionCache()
	h := visionTestHandler(t, backend.URL, &VisionBridgePolicy{
		Enabled: true, Model: testVLModel, Timeout: 5 * time.Second,
		TotalTimeout: time.Nanosecond, // 一开始就没预算
	})
	img := base64.StdEncoding.EncodeToString(noisePNGBytes(t, 64, 64))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(visionPayload(testTextModel, img))))

	if vb.vlCalls.Load() != 0 {
		t.Fatalf("预算为 0 时不该发起识图调用，实际 %d 次", vb.vlCalls.Load())
	}
	vb.mu.Lock()
	main := string(vb.lastMain)
	vb.mu.Unlock()
	if !strings.Contains(main, "data:image/png;base64,") {
		t.Fatal("预算耗尽后图片必须原样保留")
	}
}
