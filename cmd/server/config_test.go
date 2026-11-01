package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCreditsAutoClaimConfigAndEnv(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "test-key")
	t.Setenv("CA2A_CREDITS_AUTO_CLAIM", "")
	if cfg, err := Load(""); err != nil || !cfg.CreditsAutoClaim {
		t.Fatalf("auto claim default disabled: err=%v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"credits_auto_claim":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil || cfg.CreditsAutoClaim {
		t.Fatalf("config opt-out ignored: err=%v", err)
	}
	t.Setenv("CA2A_CREDITS_AUTO_CLAIM", "1")
	if cfg, err := Load(path); err != nil || !cfg.CreditsAutoClaim {
		t.Fatalf("env override ignored: err=%v", err)
	}
	t.Setenv("CA2A_CREDITS_AUTO_CLAIM", "0")
	if cfg, err := Load(""); err != nil || cfg.CreditsAutoClaim {
		t.Fatalf("env opt-out ignored: err=%v", err)
	}
}

// 仓库自带的 config.example.json 含 // 注释，README 让人直接 cp 成 config.json；
// 带注释的配置必须能加载，否则快起步就断在第一步。
func TestLoadToleratesExampleConfigComments(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	// 没有 api_key 现在会拒绝启动，示例配置的用例显式给一个（env 优先）。
	t.Setenv("CA2A_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("带注释的示例配置应能加载: %v", err)
	}
	if cfg.Listen != ":7866" || cfg.AuthDir != "./auths" || cfg.DefaultModel != "glm-5.2" {
		t.Fatalf("示例配置字段未生效: %+v", cfg)
	}
	if cfg.Cooldown.SoftRate != "60s" || cfg.MaxConcurrent != 3 || cfg.Watch.PollMinutes != 30 {
		t.Fatalf("示例配置嵌套字段未生效: %+v", cfg)
	}
	if cfg.KeepaliveWindow != "10m" {
		t.Errorf("keepalive_window 未生效: %q", cfg.KeepaliveWindow)
	}
}

// 宿主机缺 ./config.json 时，单文件 bind mount 会把挂载点建成同名目录；
// 直接 ReadFile 只会报系统错误，这里要求给出可操作的提示。
func TestLoadRejectsConfigPathThatIsDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CA2A_API_KEY", "test-key")

	_, err := Load(path)
	if err == nil {
		t.Fatal("配置路径是目录时应拒绝启动")
	}
	for _, want := range []string{"是目录而非文件", "cp config.example.json config.json", "只删除该目录无效"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("提示缺少关键信息 %q: %v", want, err)
		}
	}
	// 挂载点是目录时程序写不进配置：不能再承诺「删掉目录就会自动生成」。
	if strings.Contains(err.Error(), "会自动生成配置") {
		t.Errorf("提示不应承诺自动生成配置: %v", err)
	}
}

func TestStripJSONCommentsKeepsStrings(t *testing.T) {
	raw := []byte("{\n" +
		"  // 行注释\n" +
		"  \"oauth_callback_host\": \"https://oneapi.example.com/codearts\", /* 块注释 */\n" +
		"  \"note\": \"含 // 与 /* 的字符串\",\n" +
		"  \"n\": 1\n" +
		"}")
	var v map[string]any
	if err := json.Unmarshal(stripJSONComments(raw), &v); err != nil {
		t.Fatalf("剥离注释后应是合法 JSON: %v", err)
	}
	if v["oauth_callback_host"] != "https://oneapi.example.com/codearts" {
		t.Errorf("URL 值被破坏: %v", v["oauth_callback_host"])
	}
	if v["note"] != "含 // 与 /* 的字符串" {
		t.Errorf("字符串内容被破坏: %v", v["note"])
	}
	if v["n"] != float64(1) {
		t.Errorf("n=%v", v["n"])
	}

	// 无注释的配置保持原样。
	plain := []byte(`{"listen":":7866"}`)
	if got := string(stripJSONComments(plain)); got != string(plain) {
		t.Errorf("无注释配置被改动: %q", got)
	}
}

// 剥离器的边界：CRLF、文件末尾无换行的注释、字符串里的转义引号、
// 跨行块注释、以及孤立的 '/'。这些走错一个就会静默吃掉配置。
func TestStripJSONCommentsEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			name: "CRLF 与行尾注释",
			in:   "{\r\n  \"listen\": \":7866\", // 端口\r\n  \"default_model\": \"glm-5.2\"\r\n}",
			want: map[string]any{"listen": ":7866", "default_model": "glm-5.2"},
		},
		{
			name: "文件末尾注释无换行",
			in:   "{\"listen\":\":7866\"} // 结尾注释",
			want: map[string]any{"listen": ":7866"},
		},
		{
			name: "字符串内转义引号后的 //",
			in:   `{"note":"a\"//b","listen":":7866"}`,
			want: map[string]any{"note": `a"//b`, "listen": ":7866"},
		},
		{
			name: "跨行块注释",
			in:   "{\n/*\n 多行\n 说明\n*/\n\"listen\": \":7866\"\n}",
			want: map[string]any{"listen": ":7866"},
		},
		{
			name: "值后紧跟块注释",
			in:   `{"listen":"/*:7866*/","n":1/* 尾随 */}`,
			want: map[string]any{"listen": "/*:7866*/", "n": float64(1)},
		},
		{
			name: "注释里的引号不改变字符串状态",
			in:   "{\"listen\": \":7866\" // 这里的 \" 不配对\n}",
			want: map[string]any{"listen": ":7866"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			if err := json.Unmarshal(stripJSONComments([]byte(tc.in)), &got); err != nil {
				t.Fatalf("剥离注释后应是合法 JSON: %v\n输入: %s", err, tc.in)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("字段数不符: got=%v want=%v", got, tc.want)
			}
			for k, w := range tc.want {
				if got[k] != w {
					t.Errorf("%s: got=%v want=%v", k, got[k], w)
				}
			}
		})
	}

	// 孤立的 '/' 不能 panic 或死循环（结果是否合法 JSON 无所谓）。
	for _, in := range []string{"{", "/", `{"a":1}/`, `{"a":1}/*`} {
		_ = stripJSONComments([]byte(in))
	}
}

// 签到时点：默认 9/21，配置里允许乱序/重复/非法值，env 用 "9,21" 形式覆盖。
func TestCheckinHoursConfigAndEnv(t *testing.T) {
	t.Setenv("CA2A_API_KEY", "test-key")
	t.Setenv("CA2A_CHECKIN_HOURS", "")
	if cfg, err := Load(""); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("default checkin hours = %v err=%v", cfg.CheckinHours, err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"checkin_hours":[21,9,9,25]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("normalized checkin hours = %v err=%v", cfg.CheckinHours, err)
	}

	// 全部非法等同于没配置：回落默认，而不是「一个时点都不跑」。
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"checkin_hours":[42,-1]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(bad); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("invalid hours must fall back to default: %v err=%v", cfg.CheckinHours, err)
	}

	t.Setenv("CA2A_CHECKIN_HOURS", "7,22")
	if cfg, err := Load(path); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{7, 22}) {
		t.Fatalf("env override ignored: %v err=%v", cfg.CheckinHours, err)
	}
	// 非法 env 不覆盖配置文件里的值。
	t.Setenv("CA2A_CHECKIN_HOURS", "nine")
	if cfg, err := Load(path); err != nil || !reflect.DeepEqual(cfg.CheckinHours, []int{9, 21}) {
		t.Fatalf("invalid env must keep config value: %v err=%v", cfg.CheckinHours, err)
	}
}
