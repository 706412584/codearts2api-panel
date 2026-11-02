package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// 面板 tooltip 用的错误文本放宽到 300 字节（标签仍是 160），且不切断 UTF-8。
func TestSafeRequestErrorKeeps300Bytes(t *testing.T) {
	if got := safeRequestError(strings.Repeat("x", 1000)); len(got) != errorTextLimit {
		t.Errorf("ASCII 错误应截到 %d 字节，got=%d", errorTextLimit, len(got))
	}
	han := safeRequestError(strings.Repeat("错", 200)) // 600 字节
	if !utf8.ValidString(han) || len(han) != errorTextLimit || utf8.RuneCountInString(han) != 100 {
		t.Errorf("300 字节应恰好是 100 个汉字：len=%d runes=%d valid=%v",
			len(han), utf8.RuneCountInString(han), utf8.ValidString(han))
	}
	if got := safeLogLabel(strings.Repeat("m", 1000)); len(got) != logLabelLimit {
		t.Errorf("标签仍应按 %d 字节截断，got=%d", logLabelLimit, len(got))
	}
}

// 面板「尝试次数」列只留最近 50 条错误，超出按写入顺序滚动淘汰。
func TestAttemptErrorsKeepsLastEntries(t *testing.T) {
	ring := newAttemptErrors(attemptErrorLimit)
	for i := 0; i < attemptErrorLimit+7; i++ {
		ring.note(fmt.Sprintf("req-%02d", i), fmt.Sprintf("upstream %d", i))
	}
	snap := ring.snapshot()
	if len(snap) != attemptErrorLimit {
		t.Fatalf("只应保留 %d 条，got=%d", attemptErrorLimit, len(snap))
	}
	if _, ok := snap["req-00"]; ok {
		t.Error("最早的记录应被挤出")
	}
	newest := fmt.Sprintf("req-%02d", attemptErrorLimit+6)
	if snap[newest] != fmt.Sprintf("upstream %d", attemptErrorLimit+6) {
		t.Errorf("最新一条应保留，got=%q", snap[newest])
	}
}

// 同一个 request_id 重试多次只留最后一条；空 id 或空消息不占位。
func TestAttemptErrorsOverwritesSameRequest(t *testing.T) {
	ring := newAttemptErrors(2)
	ring.note("req-a", "第一次失败")
	ring.note("req-a", "第二次失败")
	ring.note("", "no id")
	ring.note("req-b", "")
	if snap := ring.snapshot(); len(snap) != 1 || snap["req-a"] != "第二次失败" {
		t.Fatalf("同一 request_id 应覆盖、且空值不占位：%v", snap)
	}
}

// 客户端错误报文按同一上限截断，且不会把汉字切成半个（否则 JSON 里会出现 U+FFFD）。
func TestTruncateMsgStopsOnUTF8Boundary(t *testing.T) {
	got := truncateMsg(strings.Repeat("错", 200), errorTextLimit) // 600 字节
	if len(got) != errorTextLimit || !utf8.ValidString(got) {
		t.Fatalf("应截到 %d 字节且保持合法 UTF-8：len=%d valid=%v", errorTextLimit, len(got), utf8.ValidString(got))
	}
	if short := truncateMsg("短消息", errorTextLimit); short != "短消息" {
		t.Errorf("未超限时不应改动，got=%q", short)
	}
	if edge := truncateMsg(strings.Repeat("错", 200), errorTextLimit+1); !utf8.ValidString(edge) || len(edge) > errorTextLimit+1 {
		t.Errorf("截断点落在汉字中间时应回退，len=%d valid=%v", len(edge), utf8.ValidString(edge))
	}
}

// 重试过的请求：/admin/api/usage 同时给出 attempts 与最后一次上游错误；
// 一次就成功的请求不写错误，也就不会在面板上弹 tooltip。
func TestUsageExposesLastUpstreamErrorForRetriedRequest(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = io.WriteString(w, "data: {\"error_code\":\"InferHub.ModelArts.81111.429\",\"error_msg\":\"TPM limit reached\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	a := auth.New("u-attempt", "tester", "domain", "token", "ak", "sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
	p, err := pool.New([]*auth.Auth{a}, pool.Config{MaxConcurrent: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
	// 排队上限 2、重试间隔 1ms：第一次吃 429 后立刻重试并成功。
	h := NewHandler(Config{Pool: p, APIKey: "k", MaxRotate: 1, QueueMaxAttempts: 2, QueueRetryDelay: time.Millisecond})

	if w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k"); w.Code != http.StatusOK {
		t.Fatalf("重试后应成功，status=%d body=%s", w.Code, w.Body.String())
	}
	// 第二次请求一次就成功：不该产生新的错误记录。
	if w := panelRequest(h, http.MethodPost, "/v1/chat/completions", chatBody("glm-5.2"), "k"); w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	rec := panelRequest(h, http.MethodGet, "/admin/api/usage", "", "k")
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status=%d", rec.Code)
	}
	var payload struct {
		Recent []struct {
			RequestID string `json:"request_id"`
			Attempts  int    `json:"attempts"`
		} `json:"recent"`
		AttemptErrors map[string]string `json:"attempt_errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("parse usage: %v body=%s", err, rec.Body.String())
	}

	var retriedID, firstTryID string
	for _, row := range payload.Recent {
		switch row.Attempts {
		case 2:
			retriedID = row.RequestID
		case 1:
			firstTryID = row.RequestID
		}
	}
	if retriedID == "" || firstTryID == "" {
		t.Fatalf("应各有一条 attempts=2 与 attempts=1 的记录，got=%+v", payload.Recent)
	}
	if msg := payload.AttemptErrors[retriedID]; !strings.Contains(msg, "81111.429") {
		t.Errorf("重试请求应带上最后一次上游错误，got=%q", msg)
	}
	if _, ok := payload.AttemptErrors[firstTryID]; ok {
		t.Error("一次就成功的请求不应记录错误")
	}
	if len(payload.AttemptErrors) != 1 {
		t.Errorf("只应有一条错误记录，got=%v", payload.AttemptErrors)
	}
}
