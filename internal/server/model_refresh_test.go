package server

import (
	"testing"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// 后台续期只处理「已发现且已过期」的健康账号：从未发现过的账号必须留给按需
// 懒发现，否则就变成「空闲时也主动打上游」，与删除探测的初衷相悖。
func TestShouldRefreshCatalog(t *testing.T) {
	a := &auth.Auth{UserID: "u-refresh", UserName: "u-refresh", AccessKeyID: "AK", SecretAccessKey: "SK", CloudDragonTok: "ST"}
	p, err := pool.New([]*auth.Auth{a}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(time.Second)})
	acct := p.Get("u-refresh")
	if acct == nil {
		t.Fatal("账号应存在")
	}

	// 从未发现过：不续期（交给按需路径）。
	if h.shouldRefreshCatalog(acct) {
		t.Error("未发现过的账号不应由后台续期")
	}

	// 刚发现：未过期，不续期。
	upstream.SetAccountModels("u-refresh", []upstream.ModelInfo{{ID: "m1"}})
	if h.shouldRefreshCatalog(acct) {
		t.Error("刚写入的目录不应续期")
	}

	// 目录陈旧（把发现时刻回退到 TTL 之前）：应续期。
	upstream.SetAccountModelsAt("u-refresh", []upstream.ModelInfo{{ID: "m1"}}, time.Now().Add(-upstream.CatalogTTL-time.Minute))
	if !h.shouldRefreshCatalog(acct) {
		t.Error("已过期的目录应续期")
	}

	// 账号不健康：不续期。
	p.Disable("u-refresh", "test")
	if h.shouldRefreshCatalog(acct) {
		t.Error("不健康账号不应续期")
	}
}
