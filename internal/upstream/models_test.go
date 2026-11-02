package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
)

func TestCanonicalModelKnownIndex(t *testing.T) {
	SetAccountModels("t-canon", []ModelInfo{{ID: "Qwen3.9-Test-VL", ContextWindow: 4096}})
	tests := []struct{ in, want string }{
		{"Qwen3.9-Test-VL", "Qwen3.9-Test-VL"},
		{"qwen3.9-test-vl", "Qwen3.9-Test-VL"},
		{"glm-5.2", "GLM-5.2"},
		{"snap-chat", "GLM-5.2"},
		{"unknown-model-xyz", "unknown-model-xyz"},
	}
	for _, tc := range tests {
		if got := CanonicalModel(tc.in); got != tc.want {
			t.Errorf("CanonicalModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 盘古系列上游注册名是全小写，OpenPangu-2.0-Pro 只是展示名：种子和目录发现都必须
// 给出小写 ID，否则每次调用都撞 InferHub.002002009.404（2026-11-02 实测）。
func TestCanonicalModelPanguUsesRegisteredID(t *testing.T) {
	catalog.Lock()
	delete(catalog.known, "openpangu-2.0-pro")
	catalog.Unlock()

	if got := CanonicalModel("OpenPangu-2.0-Pro"); got != "openpangu-2.0-pro" {
		t.Fatalf("目录发现前应回落到小写注册名，got=%q", got)
	}
	SetAccountModels("t-pangu", []ModelInfo{{ID: "openpangu-2.0-pro", ContextWindow: 524288}})

	tests := []struct{ in, want string }{
		{"OpenPangu-2.0-Pro", "openpangu-2.0-pro"},
		{"OPENPANGU-2.0-PRO", "openpangu-2.0-pro"},
		{"openpangu-2.0-pro", "openpangu-2.0-pro"},
		{"OpenPangu-2.0-Flash", "openpangu-2.0-flash"},
	}
	for _, tc := range tests {
		if got := CanonicalModel(tc.in); got != tc.want {
			t.Errorf("CanonicalModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetAccountModelsReplacesCatalog(t *testing.T) {
	SetAccountModels("acct-rot", []ModelInfo{{ID: "rotated-model", ContextWindow: 1024}})
	if _, ok := AccountModels("acct-rot"); !ok {
		t.Error("目录应可读回")
	}
	if !AccountCatalogStale("acct-never-seen") {
		t.Error("未发现的账号目录应视为过期")
	}
	if AccountCatalogStale("acct-rot") {
		t.Error("刚写入的目录不应过期")
	}
}

func TestMergeModelsDeterminism(t *testing.T) {
	agent := []ModelInfo{{ID: "model-a", ContextWindow: 1048576}}
	builtin := []ModelInfo{
		{ID: "GLM-5.2", ContextWindow: 202752, RatioDisplay: "0.7x"},
		{ID: "model-a", MaxTokens: 8192},
	}
	got := MergeModels(agent, builtin)
	if len(got) != 2 || got[0].ID != "GLM-5.2" || got[1].ID != "model-a" {
		t.Fatalf("合并或排序错误：%+v", got)
	}
	if got[1].ContextWindow != 1048576 || got[1].MaxTokens != 8192 {
		t.Errorf("应保留最全的参数：%+v", got[1])
	}
	if got[0].RatioDisplay != "0.7x" {
		t.Errorf("积分倍率丢失：%+v", got[0])
	}
	again := MergeModels(builtin, agent)
	for i := range got {
		if got[i].ID != again[i].ID {
			t.Fatalf("合并结果不稳定：%v vs %v", got, again)
		}
	}
}

func TestFetchModelsUsesAgentAndBuiltinSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent-center/agents/useragents":
			_, _ = io.WriteString(w, `{"agents":[{"agent_id":"a1","agent_name":"Other","is_primary_agent":true},`+
				`{"agent_id":"a2","agent_name":"CodeAgent","show_in_ide":true,"alias":{"alias_zh_cn":"智能体"}}]}`)
		case "/v1/agent-center/agents/detail":
			if r.URL.Query().Get("agent_id") != "a2" {
				t.Errorf("应选中 CodeAgent，got=%s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"gpts":{"models":[{"model_name":"GLM 5.2","notice":"错峰时段5折优惠","model_tag":"闲时折扣","model_parameters":{"context_window":202752,"max_tokens":8192,"model_desc":"最新旗舰模型","display_enabled":false,"supports_images":true}}]}}`)
		case "/v1/model/builtin":
			if r.Header.Get("Agent-Type") != "PromptCenter" {
				t.Errorf("builtin 接口需带 Agent-Type: PromptCenter，got=%q", r.Header.Get("Agent-Type"))
			}
			_, _ = io.WriteString(w, `{"builtinModels":[{"model_id":"openpangu-2.0-pro","context_window":524288,"display_enabled":true,"supports_images":false,"credit":[{"ratio_display":"0.7x"}]}]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	c.snapBase = srv.URL
	acct := &auth.Auth{UserID: "u-merge", UserName: "n", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	infos, err := c.FetchModels(acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("两路合并应得 2 个模型，got=%+v", infos)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[strings.ToLower(mi.ID)] = mi
	}
	if byID["glm-5.2"].ContextWindow != 202752 || byID["glm-5.2"].MaxTokens != 8192 {
		t.Errorf("agent-center 参数未解析：%+v", byID["glm-5.2"])
	}
	if byID["glm-5.2"].Name != "GLM 5.2" {
		t.Errorf("模型名称未解析：%+v", byID["glm-5.2"])
	}
	if byID["glm-5.2"].Desc != "最新旗舰模型" || byID["glm-5.2"].Notice != "错峰时段5折优惠" {
		t.Errorf("模型描述或提示未解析：%+v", byID["glm-5.2"])
	}
	glm := byID["glm-5.2"]
	if glm.Tag != "闲时折扣" || glm.DisplayEnabled == nil || *glm.DisplayEnabled || glm.SupportsImages == nil || !*glm.SupportsImages {
		t.Errorf("模型标签或徽标布尔值未解析：%+v", glm)
	}
	pangu := byID["openpangu-2.0-pro"]
	if pangu.DisplayEnabled == nil || !*pangu.DisplayEnabled || pangu.SupportsImages == nil || *pangu.SupportsImages {
		t.Errorf("builtin 的 display_enabled / supports_images 未解析：%+v", pangu)
	}
	if byID["openpangu-2.0-pro"].RatioDisplay != "0.7x" {
		t.Errorf("积分倍率未解析：%+v", byID["openpangu-2.0-pro"])
	}
	if _, ok := AccountModels("u-merge"); !ok {
		t.Error("发现结果应可读回")
	}
}
