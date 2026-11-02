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

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

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
