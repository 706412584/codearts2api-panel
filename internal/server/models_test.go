package server

import (
	"reflect"
	"strings"
	"testing"

	"codearts2api-panel/internal/upstream"
)

// /v1/models 对外只暴露小写 ID：只差大小写、其余属性相同的条目视为同一个模型，
// 只保留一条；上游注册名由发送时（CanonicalModel / CatalogModelID）还原。
func TestModelEntriesLowercaseAndDeduped(t *testing.T) {
	hidden, vision := false, true
	glm := upstream.ModelInfo{ID: "GLM-5.2", Name: "GLM 5.2", Desc: "最新旗舰模型", Notice: "错峰时段5折优惠", Tag: "闲时折扣", DisplayEnabled: &hidden, SupportsImages: &vision, ContextWindow: 202752, MaxTokens: 8192, RatioDisplay: "0.7x"}
	alias := glm
	alias.ID = "glm-5.2" // 只差大小写，其余属性完全一致

	entries := modelEntries([]upstream.ModelInfo{
		glm,
		alias,
		{ID: "Qwen3-VL-235B", ContextWindow: 131072},
		{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576},
	})
	if len(entries) != 3 {
		t.Fatalf("只差大小写的条目应合并为一条，got %d 条: %v", len(entries), entries)
	}

	byID := map[string]map[string]any{}
	for _, e := range entries {
		id, _ := e["id"].(string)
		if id != strings.ToLower(id) {
			t.Errorf("对外 ID 必须小写，got %q", id)
		}
		if byID[id] != nil {
			t.Fatalf("/v1/models 出现重复 ID: %s", id)
		}
		byID[id] = e
	}

	for _, want := range []string{"glm-5.2", "qwen3-vl-235b", "deepseek-v4-pro-0813"} {
		if byID[want] == nil {
			t.Errorf("缺少模型条目 %q，got=%v", want, keysOf(byID))
		}
	}
	if e := byID["glm-5.2"]; e["context_length"] != int64(202752) || e["max_output_tokens"] != int64(8192) {
		t.Errorf("模型参数应取自上游目录，got=%v", e)
	}
	if byID["glm-5.2"]["ratio_display"] != "0.7x" {
		t.Errorf("积分倍率应传给面板，got=%v", byID["glm-5.2"])
	}
	if byID["glm-5.2"]["model_name"] != "GLM 5.2" {
		t.Errorf("模型名称应传给面板，got=%v", byID["glm-5.2"])
	}
	// 面板 tooltip 数据源：描述 + 上游提示。
	if byID["glm-5.2"]["model_desc"] != "最新旗舰模型" || byID["glm-5.2"]["notice"] != "错峰时段5折优惠" {
		t.Errorf("模型描述与提示应传给面板，got=%v", byID["glm-5.2"])
	}
	if _, ok := byID["deepseek-v4-pro-0813"]["model_desc"]; ok {
		t.Errorf("没有描述时不应输出 model_desc，got=%v", byID["deepseek-v4-pro-0813"])
	}
	// 面板徽标数据源：标签、隐藏模型、视觉理解。
	if glm := byID["glm-5.2"]; glm["model_tag"] != "闲时折扣" || glm["display_enabled"] != false || glm["supports_images"] != true {
		t.Errorf("徽标字段应传给面板，got=%v", glm)
	}
	plain := byID["deepseek-v4-pro-0813"]
	for _, key := range []string{"model_tag", "display_enabled", "supports_images"} {
		if _, ok := plain[key]; ok {
			// 上游没给 display_enabled（nil）不得被当成 false，否则会被标成「隐藏模型」。
			t.Errorf("上游没有表态时不应输出 %s，got=%v", key, plain)
		}
	}

	// 模态声明：视觉模型多一个 image 输入，其余纯文本；输出恒为 text。
	if got := byID["glm-5.2"]["input_modalities"]; !reflect.DeepEqual(got, []string{"text", "image"}) {
		t.Errorf("视觉模型的 input_modalities 应含 image，got=%v", got)
	}
	if got := plain["input_modalities"]; !reflect.DeepEqual(got, []string{"text"}) {
		t.Errorf("纯文本模型的 input_modalities 只有 text，got=%v", got)
	}
	for id, e := range byID {
		if got := e["output_modalities"]; !reflect.DeepEqual(got, []string{"text"}) {
			t.Errorf("%s 的 output_modalities 应为 text，got=%v", id, got)
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
