package server

import (
	"testing"

	"codearts2api-panel/internal/upstream"
)

func TestModelEntriesAliasesAndMetadata(t *testing.T) {
	hidden, vision := false, true
	entries := modelEntries([]upstream.ModelInfo{
		{ID: "GLM-5.2", Name: "GLM 5.2", Desc: "最新旗舰模型", Notice: "错峰时段5折优惠", Tag: "闲时折扣", DisplayEnabled: &hidden, SupportsImages: &vision, ContextWindow: 202752, MaxTokens: 8192, RatioDisplay: "0.7x"},
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

	for _, want := range []string{"GLM-5.2", "glm-5.2", "deepseek-v4-pro-0813"} {
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
	// 面板 tooltip 数据源：描述 + 上游提示。
	if byID["GLM-5.2"]["model_desc"] != "最新旗舰模型" || byID["GLM-5.2"]["notice"] != "错峰时段5折优惠" {
		t.Errorf("模型描述与提示应传给面板，got=%v", byID["GLM-5.2"])
	}
	if _, ok := byID["deepseek-v4-pro-0813"]["model_desc"]; ok {
		t.Errorf("没有描述时不应输出 model_desc，got=%v", byID["deepseek-v4-pro-0813"])
	}
	// 面板徽标数据源：标签、隐藏模型、视觉理解。
	glm := byID["GLM-5.2"]
	if glm["model_tag"] != "闲时折扣" || glm["display_enabled"] != false || glm["supports_images"] != true {
		t.Errorf("徽标字段应传给面板，got=%v", glm)
	}
	plain := byID["deepseek-v4-pro-0813"]
	for _, key := range []string{"model_tag", "display_enabled", "supports_images"} {
		if _, ok := plain[key]; ok {
			// 上游没给 display_enabled（nil）不得被当成 false，否则会被标成「隐藏模型」。
			t.Errorf("上游没有表态时不应输出 %s，got=%v", key, plain)
		}
	}

	// 没有目录就没有条目：不再有硬编码兜底表，空输入必须返回空列表。
	if empty := modelEntries(nil); len(empty) != 0 {
		t.Errorf("空目录不应产生任何模型条目，got=%v", empty)
	}
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
