package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/reqlog"
	"codearts2api-panel/internal/upstream"
)

func panelRequest(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdminKeySeparatesManagementFromInference(t *testing.T) {
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p, APIKey: "inference-key", AdminKey: "management-key"})
	for _, path := range []string{"/admin/api/overview", "/admin/api/models", "/admin/api/usage", "/admin/api/logs", "/admin/api/config"} {
		if w := panelRequest(h, "GET", path, "", "inference-key"); w.Code != 401 {
			t.Fatalf("%s leaked to API key: %d", path, w.Code)
		}
	}
	if w := panelRequest(h, "GET", "/admin/api/logs", "", "management-key"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := panelRequest(h, "GET", "/v1/models", "", "inference-key"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := panelRequest(h, "GET", "/v1/models", "", "management-key"); w.Code != 401 {
		t.Fatal("admin key unexpectedly grants inference access")
	}
}

func TestImportBatchValidationPersistenceAndDelete(t *testing.T) {
	dir := t.TempDir()
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p, AuthDir: dir, AdminKey: "admin"})
	valid := `{"user_id":"import-a","user_name":"A","access_key_id":"ak-a","secret_access_key":"never-show-this"}`
	w := panelRequest(h, "POST", "/admin/api/accounts/import", "["+valid+`,{"user_id":"bad"}]`, "admin")
	if w.Code != 400 {
		t.Fatalf("invalid batch=%d %s", w.Code, w.Body.String())
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 || len(p.Accounts()) != 0 {
		t.Fatal("part of invalid batch was imported")
	}
	w = panelRequest(h, "POST", "/admin/api/accounts/import", "["+valid+`,{"user_id":"import-b","access_key_id":"ak-b","secret_access_key":"sk-b"}]`, "admin")
	if w.Code != 200 || strings.Contains(w.Body.String(), "never-show-this") {
		t.Fatalf("import=%d %s", w.Code, w.Body.String())
	}
	stored, err := auth.LoadDir(dir)
	if err != nil || len(stored) != 2 || len(p.Accounts()) != 2 {
		t.Fatalf("accounts were not persisted: %v %d", err, len(stored))
	}
	w = panelRequest(h, "POST", "/admin/api/accounts/import", `[{"user_id":"import-c","access_key_id":"ak-c","secret_access_key":"sk-c"},`+valid+`]`, "admin")
	if w.Code != 409 || p.Get("import-c") != nil {
		t.Fatal("conflicting batch partially imported")
	}
	stored, _ = auth.LoadDir(dir)
	if len(stored) != 2 {
		t.Fatal("conflicting import changed saved accounts")
	}
	w = panelRequest(h, "POST", "/admin/api/accounts/delete", `{"uid":"import-a"}`, "admin")
	if w.Code != 200 || p.Get("import-a") != nil {
		t.Fatalf("delete=%d %s", w.Code, w.Body.String())
	}
	stored, _ = auth.LoadDir(dir)
	if len(stored) != 1 || stored[0].UserID != "import-b" {
		t.Fatal("delete changed the wrong credential")
	}
}

func TestImageContentSurvivesUpstreamMappingAndAffinity(t *testing.T) {
	makeRequest := func(image string) *chatRequest {
		req, err := parseChatRequest([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"}}]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	a, b := makeRequest("AAAA"), makeRequest("BBBB")
	wire, _ := json.Marshal(buildUpstreamMessages(a, false))
	if !strings.Contains(string(wire), `"image_url":{"url":"data:image/png;base64,AAAA"}`) {
		t.Fatalf("image dropped: %s", wire)
	}
	if conversationIDFor(a, "") == conversationIDFor(b, "") {
		t.Fatal("different images shared the same implicit conversation")
	}
	_, err := parseChatRequest([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"bad"}]}]}`))
	if err == nil {
		t.Fatal("malformed image object accepted")
	}
}

func TestRequestMetricsUseActualUsageAndRejectTruncatedSuccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, complete := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/complete=%t", stream, complete), func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n")
					w.(http.Flusher).Flush()
					if complete {
						io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":13,\"completion_tokens\":7,\"total_tokens\":20}}\n\ndata: [DONE]\n\n")
					}
				}))
				defer up.Close()
				a := auth.New("metrics-account", "test", "domain", "private-token", "private-ak", "private-sk", time.Now().Add(time.Hour).Format(time.RFC3339), "", "")
				p, _ := pool.New([]*auth.Auth{a}, pool.Config{}, "")
				p.Accounts()[0].Client = upstream.NewWithChatEndpoint(5*time.Second, up.URL)
				h := NewHandler(Config{Pool: p, MaxRotate: 1})
				w := panelRequest(h, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":"GLM-5.2","stream":%t,"messages":[{"role":"user","content":"private-prompt"}]}`, stream), "")
				snapshot := h.cfg.RequestLog.Snapshot()
				if snapshot.Completed != 1 || snapshot.InFlight != 0 || snapshot.Recent[0].OK != complete {
					t.Fatalf("incorrect metrics: %+v", snapshot)
				}
				if complete && snapshot.Recent[0].TotalTokens != 20 {
					t.Fatal("actual usage was not recorded")
				}
				if !complete && !stream && w.Code == 200 {
					t.Fatalf("truncated response succeeded: %s", w.Body.String())
				}
				if !complete && stream && !strings.Contains(w.Body.String(), "event: error") {
					t.Fatal("truncated stream did not emit error")
				}
				raw, _ := json.Marshal(snapshot)
				for _, secret := range []string{"private-prompt", "private-token", "private-sk", "private-ak"} {
					if strings.Contains(string(raw), secret) {
						t.Fatal("request metrics exposed private request data")
					}
				}
			})
		}
	}
}

func TestRecentFailureIncludesRedactedErrorAndSuccessClearsIt(t *testing.T) {
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p})
	w := panelRequest(h, "POST", "/v1/chat/completions", `{"messages":[]}`, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", w.Code)
	}
	recent := h.cfg.RequestLog.Snapshot().Recent
	if len(recent) != 1 || recent[0].OK || recent[0].Error != "no accounts configured" {
		t.Fatalf("failed request not recorded: %+v", recent)
	}
	entries := h.logs.Snapshot()
	if len(entries) != 1 || entries[0].Ch != "chat" || !strings.Contains(entries[0].Text, "no accounts configured") {
		t.Fatalf("failure must be one chat log line: %+v", entries)
	}

	observed := &responseObserver{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK, started: time.Now()}
	h.finishRequest(reqlog.Event{Error: `authorization=private-token`, OK: true}, observed, time.Now())
	recent = h.cfg.RequestLog.Snapshot().Recent
	if !recent[0].OK || recent[0].Error != "" {
		t.Fatalf("successful retry must not retain failure: %+v", recent[0])
	}
}

func TestFailureErrorCapturesResponseSummary(t *testing.T) {
	h := NewHandler(Config{})
	observed := &responseObserver{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK, started: time.Now()}
	writeOpenAIError(observed, http.StatusBadRequest, "invalid_request", "messages must not be empty")
	h.finishRequest(reqlog.Event{}, observed, time.Now())
	recent := h.cfg.RequestLog.Snapshot().Recent
	if len(recent) != 1 || recent[0].Error != "messages must not be empty" {
		t.Fatalf("response error not recorded: %+v", recent)
	}
}

func TestFailureErrorIsBoundedAndRedacted(t *testing.T) {
	h := NewHandler(Config{})
	observed := &responseObserver{ResponseWriter: httptest.NewRecorder(), status: http.StatusBadGateway, started: time.Now()}
	h.finishRequest(reqlog.Event{Error: "authorization=private-token"}, observed, time.Now())
	recent := h.cfg.RequestLog.Snapshot().Recent
	if len(recent) != 1 || strings.Contains(recent[0].Error, "private-token") || !strings.Contains(recent[0].Error, "[REDACTED]") {
		t.Fatalf("unsafe failure error: %+v", recent)
	}
	h.finishRequest(reqlog.Event{Error: strings.Repeat("错误", 100)}, observed, time.Now())
	if recent = h.cfg.RequestLog.Snapshot().Recent; len(recent[0].Error) > errorTextLimit || !utf8.ValidString(recent[0].Error) {
		t.Fatalf("unbounded or invalid error: %q", recent[0].Error)
	}
}

func TestLogRingRedactsCredentialsAndIsBounded(t *testing.T) {
	ring := NewRing(2)
	io.WriteString(ring, "first\n")
	io.WriteString(ring, `secret_access_key="private-sk" refresh_token=private-refresh`+"\n")
	io.WriteString(ring, "last\n")
	entries := ring.Snapshot()
	if len(entries) != 2 || strings.Contains(entries[0].Text, "private-") {
		t.Fatalf("unsafe ring: %+v", entries)
	}
}

func TestLogRingRedactsWholeAuthorizationAndNestedSecrets(t *testing.T) {
	for _, line := range []string{
		"Authorization: Bearer PRIVATE-TOKEN",
		"Authorization: SDK-HMAC-SHA256 Access=PRIVATE-AK, SignedHeaders=host, Signature=PRIVATE-SIGNATURE",
		`{"authorization":"Bearer PRIVATE-TOKEN","other":"field"}`,
		`{"dpop_private_key":{"kty":"EC","d":"PRIVATE-KEY","x":"PUBLIC"}}`,
	} {
		ring := NewRing(1)
		io.WriteString(ring, line)
		if strings.Contains(ring.Snapshot()[0].Text, "PRIVATE") {
			t.Fatalf("credentials leaked: %s", ring.Snapshot()[0].Text)
		}
	}
}

type usagePayload struct {
	Completed int64           `json:"completed"`
	Recent    []reqlog.Event  `json:"recent"`
	History   *reqlog.History `json:"history"`
}

// 进程指标随重启清零，但 data/requests 里的归档还在：/admin/api/usage 始终带上它，
// 否则重启后只要跑过一条请求，用量页就塌成一条（本地进程数据不是完整账本）。
func TestUsageAlwaysIncludesArchive(t *testing.T) {
	dir := t.TempDir()
	before := reqlog.New(reqlog.Config{Enabled: true, Dir: dir, RetentionDays: 7})
	before.Record(reqlog.Event{
		RequestID: "persisted", Path: "/v1/chat/completions", Model: "glm-5.2",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 120, TotalTokens: 42,
	})
	before.Close()

	fresh := reqlog.New(reqlog.Config{Enabled: true, Dir: dir, RetentionDays: 7})
	defer fresh.Close()
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p, RequestLog: fresh})

	var body usagePayload
	raw := panelRequest(h, http.MethodGet, "/admin/api/usage", "", "")
	if err := json.Unmarshal(raw.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode usage: %v (%s)", err, raw.Body.String())
	}
	if body.Completed != 0 || len(body.Recent) != 0 {
		t.Fatalf("restarted process must start empty: %+v", body)
	}
	if body.History == nil || body.History.Completed != 1 || len(body.History.Recent) != 1 || body.History.Recent[0].RequestID != "persisted" {
		t.Fatalf("usage did not read the archive: %s", raw.Body.String())
	}

	// 本进程跑过请求之后归档仍然要在：页面据此保留历史，而不是只剩这一条。
	fresh.Record(reqlog.Event{RequestID: "live", Path: "/v1/chat/completions", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess})
	body = usagePayload{}
	raw = panelRequest(h, http.MethodGet, "/admin/api/usage", "", "")
	if err := json.Unmarshal(raw.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Completed != 1 || len(body.Recent) != 1 || body.Recent[0].RequestID != "live" {
		t.Fatalf("process counters must stay live: %s", raw.Body.String())
	}
	if body.History == nil || len(body.History.Recent) == 0 || body.History.Recent[0].RequestID != "persisted" {
		t.Fatalf("archive must stay attached once the process has data: %s", raw.Body.String())
	}
}

// 归档关闭时不能凭空造 history：页面退回纯进程数据。
func TestUsageWithoutArchiveHasNoHistory(t *testing.T) {
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p})
	var body usagePayload
	raw := panelRequest(h, http.MethodGet, "/admin/api/usage", "", "")
	if err := json.Unmarshal(raw.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.History != nil {
		t.Fatalf("history without an archive: %s", raw.Body.String())
	}
}

// 用量页的 30 天视图走 window=1：服务端扫归档并聚合，只回数字和按模型/按小时的汇总，
// 不把逐条记录传下来；不带 window 的请求保持原样，日志页还要逐条记录。
func TestUsageWindowAggregates(t *testing.T) {
	dir := t.TempDir()
	rec := reqlog.New(reqlog.Config{Enabled: true, Dir: dir, RetentionDays: 30})
	defer rec.Close()
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	rec.Record(reqlog.Event{
		Time: at, RequestID: "win-1", Model: "glm-5.2", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		DurationMs: 100, TTFBMs: 200, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
	})
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p, RequestLog: rec})

	query := "/admin/api/usage?window=1&from=" + url.QueryEscape(at.Add(-time.Minute).Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(at.Add(time.Minute).Format(time.RFC3339))
	raw := panelRequest(h, http.MethodGet, query, "", "")
	var stats reqlog.Stats
	if err := json.Unmarshal(raw.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode window stats: %v (%s)", err, raw.Body.String())
	}
	if stats.Completed != 1 || stats.Succeeded != 1 || stats.TotalTokens != 15 || stats.TTFTSamples != 1 {
		t.Fatalf("window stats = %+v", stats)
	}
	if len(stats.Models) != 1 || stats.Models[0].Model != "glm-5.2" || len(stats.Hours) != 1 {
		t.Fatalf("window breakdown = %+v / %+v", stats.Models, stats.Hours)
	}

	// 窗口外一条都不算：面板据此显示空状态，而不是拿全量糊上去。
	outside := "/admin/api/usage?window=1&from=" + url.QueryEscape(at.Add(time.Hour).Format(time.RFC3339))
	raw = panelRequest(h, http.MethodGet, outside, "", "")
	stats = reqlog.Stats{}
	if err := json.Unmarshal(raw.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Completed != 0 || len(stats.Models) != 0 {
		t.Fatalf("out-of-window stats = %+v", stats)
	}

	// 非窗口请求仍是逐条记录（运行日志页用）。
	var body usagePayload
	raw = panelRequest(h, http.MethodGet, "/admin/api/usage", "", "")
	if err := json.Unmarshal(raw.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Recent) != 1 || body.Recent[0].RequestID != "win-1" {
		t.Fatalf("plain usage must keep the per-request rows: %s", raw.Body.String())
	}
}

// 备注是纯本地字段：设置后要落盘、出现在 overview 里、可被搜索匹配；清空即移除。
func TestAccountNotePersistsAndClears(t *testing.T) {
	dir := t.TempDir()
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p, AuthDir: dir, AdminKey: "admin"})
	valid := `{"user_id":"note-a","user_name":"A","access_key_id":"ak-a","secret_access_key":"sk-a"}`
	if w := panelRequest(h, "POST", "/admin/api/accounts/import", "["+valid+"]", "admin"); w.Code != 200 {
		t.Fatalf("import=%d %s", w.Code, w.Body.String())
	}

	// 设置备注：overview 回读 + 文件落盘
	if w := panelRequest(h, "POST", "/admin/api/accounts/note", `{"uid":"note-a","note":"13800001111"}`, "admin"); w.Code != 200 {
		t.Fatalf("set note=%d %s", w.Code, w.Body.String())
	}
	overview := panelRequest(h, "GET", "/admin/api/overview", "", "admin").Body.String()
	if !strings.Contains(overview, `"note":"13800001111"`) {
		t.Fatalf("note missing from overview: %s", overview)
	}
	stored, err := auth.LoadDir(dir)
	if err != nil || len(stored) != 1 || stored[0].Note != "13800001111" {
		t.Fatalf("note not persisted to disk: %v %+v", err, stored)
	}

	// 清空：overview 不再有该值，磁盘也清掉
	if w := panelRequest(h, "POST", "/admin/api/accounts/note", `{"uid":"note-a","note":""}`, "admin"); w.Code != 200 {
		t.Fatalf("clear note=%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(panelRequest(h, "GET", "/admin/api/overview", "", "admin").Body.String(), "13800001111") {
		t.Fatal("cleared note still visible")
	}
	if stored, _ := auth.LoadDir(dir); len(stored) != 1 || stored[0].Note != "" {
		t.Fatalf("note not cleared on disk: %+v", stored)
	}
}

// 备注不得影响凭据本身，也不该接受超长值。
func TestAccountNoteRejectsOversizeAndUnknownAccount(t *testing.T) {
	dir := t.TempDir()
	p, _ := pool.New(nil, pool.Config{}, "")
	h := NewHandler(Config{Pool: p, AuthDir: dir, AdminKey: "admin"})
	valid := `{"user_id":"note-b","user_name":"B","access_key_id":"ak-b","secret_access_key":"sk-b"}`
	_ = panelRequest(h, "POST", "/admin/api/accounts/import", "["+valid+"]", "admin")

	long := strings.Repeat("x", 201)
	if w := panelRequest(h, "POST", "/admin/api/accounts/note", `{"uid":"note-b","note":"`+long+`"}`, "admin"); w.Code != 400 {
		t.Fatalf("oversize note accepted: %d", w.Code)
	}
	if w := panelRequest(h, "POST", "/admin/api/accounts/note", `{"uid":"nope","note":"x"}`, "admin"); w.Code != 404 {
		t.Fatalf("unknown account=%d", w.Code)
	}
	// 凭据未被备注操作破坏
	stored, _ := auth.LoadDir(dir)
	if len(stored) != 1 || stored[0].AccessKeyID != "ak-b" || stored[0].SecretAccessKey != "sk-b" {
		t.Fatalf("credentials damaged: %+v", stored)
	}
}
