package server

import (
	"context"
	"log"
	"time"

	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/upstream"
)

// 模型目录后台续期。
//
// 目录（每个账号的上游模型列表）缓存 23h。首次发现仍由用户请求惰性触发
// （/v1/models 或 analyze 图片路径），一旦发现过，这里在过期前把它续上，
// 于是目录常驻、用户请求永远命中缓存，不会再出现「缓存刚过期、下一个请求
// 先卡着拉目录」的抖动。
//
// 只续「已经发现过」的账号：从未发现过的账号留给按需路径，避免后台替
// 用户主动打上游——这与「空闲时零额外请求」的原则一致：有缓存才续，
// 没有缓存说明这个账号本来就没被用过。
const (
	// modelRefreshInterval 续期巡检间隔。23h TTL 下每小时看一眼，最坏情况下
	// 目录过期不超过 1h，且续期任务本身失败也有下一轮兜底。
	modelRefreshInterval = time.Hour
	// modelRefreshStartDelay 启动后多久开始首次巡检，避开接入初期的真实请求。
	modelRefreshStartDelay = 2 * time.Minute
)

// StartModelRefresher 后台按 modelRefreshInterval 续期已发现的账号目录。
func (h *Handler) StartModelRefresher(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(modelRefreshStartDelay):
		}
		ticker := time.NewTicker(modelRefreshInterval)
		defer ticker.Stop()
		h.refreshStaleCatalogs()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.refreshStaleCatalogs()
			}
		}
	}()
}

// refreshStaleCatalogs 重新发现「已发现且已过期」的健康账号目录。
func (h *Handler) refreshStaleCatalogs() {
	for _, acct := range h.cfg.Pool.Accounts() {
		if !h.shouldRefreshCatalog(acct) {
			continue
		}
		if _, err := h.cfg.Upstream.FetchModels(acct.Auth); err != nil {
			log.Printf("model catalog refresh failed for account %s: %v", acct.Name, err)
			continue
		}
		log.Printf("model catalog refreshed for account %s", acct.Name)
	}
}

// shouldRefreshCatalog 报告该账号目录是否该由后台续期。
// 三个前提缺一不可：账号健康且有 STS 凭据、目录**曾经发现过**（否则交给按需
// 路径，后台不替用户主动打上游）、且已过期。
func (h *Handler) shouldRefreshCatalog(acct *pool.Account) bool {
	if acct == nil || acct.Auth == nil || !h.cfg.Pool.Healthy(acct.Name) {
		return false
	}
	if _, known := upstream.AccountModels(acct.UID); !known {
		return false // 从未发现过：交给按需懒发现
	}
	if !upstream.AccountCatalogStale(acct.UID) {
		return false
	}
	_, ak, sk := acct.Auth.Credentials()
	return ak != "" && sk != ""
}
