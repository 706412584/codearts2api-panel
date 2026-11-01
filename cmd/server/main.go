package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/reqlog"
	"codearts2api-panel/internal/scheduler"
	"codearts2api-panel/internal/server"
	"codearts2api-panel/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	if err := ensureConfig(*cfgPath); err != nil {
		log.Fatalf("initialize config: %v", err)
	}
	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	resolveDataPaths(cfg, *cfgPath)

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	if len(auths) == 0 {
		log.Printf("No accounts yet. Add a CodeArts account from the panel.")
	}

	if cfg.StateFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700)
	}
	p, err := pool.New(auths, cfg.ToPoolConfig(), cfg.StateFile)
	if err != nil {
		log.Fatalf("build pool: %v", err)
	}
	p.SetBenefitAutoClaim(cfg.BenefitAutoClaim)

	c := upstream.New(time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second)
	c.SetBenefitAutoClaim(cfg.BenefitAutoClaim)
	requestLog := reqlog.New(reqlog.Config{
		Dir:     filepath.Join(filepath.Dir(cfg.StateFile), "requests"),
		Enabled: true, RetentionDays: 7,
	})
	defer requestLog.Close()

	sch := scheduler.New(scheduler.Config{
		Pool:              p,
		Enabled:           cfg.Watch.Enabled,
		CreditsAutoClaim:  cfg.CreditsAutoClaim,
		PollInterval:      time.Duration(cfg.Watch.PollMinutes) * time.Minute,
		RefreshSkew:       time.Duration(cfg.Watch.RefreshSkewM) * time.Minute,
		KeepaliveInterval: time.Duration(cfg.Watch.KeepaliveInterval) * time.Minute,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     c,
		APIKey:       cfg.APIKey,
		AdminKey:     cfg.AdminKey,
		Settings:     &fileSettings{path: *cfgPath},
		RequestLog:   requestLog,
		MaxRotate:    3,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		DefaultModel: cfg.DefaultModel,
		QueueRetryDelay: func() time.Duration {
			if cfg.QueueRetrySeconds > 0 {
				return time.Duration(cfg.QueueRetrySeconds) * time.Second
			}
			return 10 * time.Second
		}(),
		QueueMaxAttempts: cfg.QueueMaxAttempts,
		ConvStateFile:    cfg.StateFile + ".chats.json",
		WatchInfo: map[string]any{
			"enabled":              cfg.Watch.Enabled,
			"credits_auto_claim":   cfg.CreditsAutoClaim,
			"poll_minutes":         cfg.Watch.PollMinutes,
			"refresh_skew_minutes": cfg.Watch.RefreshSkewM,
			"keepalive_interval":   cfg.Watch.KeepaliveInterval,
			"max_concurrent":       cfg.MaxConcurrent,
			"keepalive_window":     cfg.KeepaliveWindow,
		},
		AuthDir:           cfg.AuthDir,
		Listen:            cfg.Listen,
		OAuthClient:       upstream.New(15 * time.Second),
		LoginConfig:       loginConfig(cfg),
		OAuthCallbackHost: cfg.OAuthCallbackHost,
	})
	defer h.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, h.LogWriter()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("CodeArts2API Panel listening on %s; settings: %s", cfg.Listen, *cfgPath)
	log.Printf("Open /panel and use admin_key from the configuration file. API clients use api_key.")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// Relative storage paths belong to the selected config, including Docker volumes.
func resolveDataPaths(cfg *Config, configPath string) {
	base, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return
	}
	if !filepath.IsAbs(cfg.AuthDir) {
		cfg.AuthDir = filepath.Join(base, cfg.AuthDir)
	}
	if !filepath.IsAbs(cfg.StateFile) {
		cfg.StateFile = filepath.Join(base, cfg.StateFile)
	}
}

// loginConfig 决定 WebUI 登录使用的 OAuth 配置。
//
// client_id 优先取已有账号记录的取值（refresh_token 与 client_id 绑定，混用会让
// 老账号刷新失败），其次取配置项，最后才是内置默认值。其余字段必须来自
// DefaultLoginConfig（否则 authorize 链接会缺 host / plugin 信息）。
func loginConfig(cfg *Config) upstream.LoginConfig {
	lc := upstream.DefaultLoginConfig()
	if cfg.LoginClientID != "" {
		lc.ClientID = cfg.LoginClientID
	}
	if auths, err := auth.LoadDir(cfg.AuthDir); err == nil {
		for _, a := range auths {
			if id := a.ClientIDOr(""); id != "" {
				lc.ClientID = id
				break
			}
		}
	}
	return lc
}
