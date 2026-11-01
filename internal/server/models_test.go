package server

import (
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// 混合账号池：/v1/models 展示的是并集，聊天必须先挑目录里真有该模型的账号。
func TestPickAccountPrefersAccountWithModel(t *testing.T) {
	p, err := pool.New([]*auth.Auth{
		{UserID: "acct-a", UserName: "a"},
		{UserID: "acct-b", UserName: "b"},
		{UserID: "acct-c", UserName: "c"},
		{UserID: "acct-d", UserName: "d"},
	}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})

	upstream.SetAccountModels("acct-a", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	upstream.SetAccountModels("acct-b", []upstream.ModelInfo{{ID: "GLM-5.2"}})
	upstream.SetAccountModels("acct-c", []upstream.ModelInfo{
		{ID: "GLM-5.2"}, {ID: "deepseek-v4-pro-0813"},
	})

	got := h.pickAccount(map[string]bool{}, "deepseek-v4-pro-0813")
	if got == nil || got.Name != "acct-c" {
		t.Fatalf("应挑目录里有该模型的账号，got=%v", got)
	}
	got = h.pickAccount(map[string]bool{}, "glm-5.2")
	if got == nil || got.Name != "acct-a" {
		t.Fatalf("小写别名应命中第一个有能力账号，got=%v", got)
	}
	got = h.pickAccount(map[string]bool{"acct-c": true}, "deepseek-v4-pro-0813")
	if got == nil || got.Name == "acct-c" {
		t.Fatalf("被排除的账号不应再被选中，got=%v", got)
	}
	if got = h.pickAccount(map[string]bool{}, "unknown-model-xyz"); got == nil {
		t.Fatal("应退回普通轮转而不是拒绝请求")
	}

	if h.accountCanServe(p.Get("acct-a"), "deepseek-v4-pro-0813") {
		t.Error("acct-a 目录里没有该模型")
	}
	if !h.accountCanServe(p.Get("acct-c"), "DEEPSEEK-V4-PRO-0813") {
		t.Error("有该模型的账号应通过能力检查（大小写不敏感）")
	}
	if !h.accountCanServe(p.Get("acct-d"), "deepseek-v4-pro-0813") {
		t.Error("目录未知时应乐观放行")
	}
	if !h.accountCanServe(nil, "deepseek-v4-pro-0813") {
		t.Error("账号不存在时应乐观放行")
	}
}

func TestModelEntriesAliasesStaticAndRatio(t *testing.T) {
	entries := modelEntries([]upstream.ModelInfo{
		{ID: "GLM-5.2", Name: "GLM 5.2", ContextWindow: 202752, MaxTokens: 8192, RatioDisplay: "0.7x"},
		{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576},
	})
	byID := map[string]map[string]any{}
	for _, e := range entries {
		id, _ := e["id"].(string)
		if byID[id] != nil {
			t.Fatalf("/v1/models 出现重复 ID: %s", id)
		}
		byID[id] = e
	}

	for _, want := range []string{"GLM-5.2", "glm-5.2", "deepseek-v4-pro-0813", "Qwen3-VL-235B", "qwen3-vl-235b"} {
		if byID[want] == nil {
			t.Errorf("缺少模型条目 %q，got=%v", want, keysOf(byID))
		}
	}
	if byID["GLM-5.2"]["context_length"] != int64(202752) || byID["GLM-5.2"]["max_output_tokens"] != int64(8192) {
		t.Errorf("模型参数应取自上游目录，got=%v", byID["GLM-5.2"])
	}
	if byID["GLM-5.2"]["ratio_display"] != "0.7x" {
		t.Errorf("积分倍率应传给面板，got=%v", byID["GLM-5.2"])
	}
	if byID["GLM-5.2"]["model_name"] != "GLM 5.2" {
		t.Errorf("模型名称应传给面板，got=%v", byID["GLM-5.2"])
	}

	fallback := modelEntries(nil)
	if len(fallback) < len(staticModels) {
		t.Fatalf("静态兜底条目过少: %d", len(fallback))
	}
	seen := map[string]bool{}
	for _, e := range fallback {
		id, _ := e["id"].(string)
		if seen[id] {
			t.Fatalf("静态兜底出现重复 ID: %s", id)
		}
		seen[id] = true
	}
	if !seen["GLM-5.2"] || !seen["glm-5.2"] {
		t.Errorf("静态兜底也要给大写 ID 补小写别名，got=%v", keysOf(seen))
	}
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
