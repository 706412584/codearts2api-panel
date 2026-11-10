package gateway

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"codearts2api-panel/internal/auth"
	"codearts2api-panel/internal/balance"
	"codearts2api-panel/internal/livecfg"
	"codearts2api-panel/internal/pool"
	"codearts2api-panel/internal/reqlog"
	"codearts2api-panel/internal/scheduler"
	"codearts2api-panel/internal/server"
	"codearts2api-panel/internal/upstream"
)

// Run 启动网关并阻塞直到 ctx 被取消。
//
// 从 main() 抽出来是为了让 Android 版的 c-shared 库复用同一份启动逻辑：那边没有
// 进程信号可用，靠 Java 侧取消 context 来停机。返回值取代了原来的 log.Fatalf，
// 由调用方决定是打日志退出还是回传给 Java。
func Run(cfgPath string, ctx context.Context) error {
	return RunWithReady(cfgPath, ctx, nil)
}

// RunWithReady 与 Run 相同，但额外通过 ready 通道回报启动结果：端口绑定成功发
// nil，绑定失败（如端口被占）发错误。
//
// Android 侧需要这个信号才能把「启动失败」同步回传给 Java——exec 版靠进程退出码，
// in-process 没有退出码可看。ready 为 nil 时与 Run 行为完全一致。
func RunWithReady(cfgPath string, ctx context.Context, ready chan<- error) error {
	reported := false
	notifyReady := func(err error) {
		if ready != nil && !reported {
			reported = true
			ready <- err
		}
	}
	if err := ensureConfig(cfgPath); err != nil {
		notifyReady(err)
		return err
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		notifyReady(err)
		return err
	}
	resolveDataPaths(cfg, cfgPath)

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		notifyReady(err)
		return err
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)
	if len(auths) == 0 {
		log.Printf("No accounts yet. Add a CodeArts account from the panel.")
	}

	if cfg.StateFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700)
	}
	balPath := filepath.Join(filepath.Dir(cfg.StateFile), "balance_history.json")
	bal := balance.New(balPath)
	bal.Start()
	defer bal.Stop()
	log.Printf("balance history enabled: %s (upstream refresh every %s, history samples throttled separately)", balPath, balance.Interval())
	poolCfg := cfg.ToPoolConfig()
	poolCfg.BalanceObserver = bal
	p, err := pool.New(auths, poolCfg, cfg.StateFile)
	if err != nil {
		return err
	}

	c := upstream.New(time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second)
	requestLog := reqlog.New(reqlog.Config{
		Dir:           filepath.Join(filepath.Dir(cfg.StateFile), "requests"),
		Enabled:       true,
		RetentionDays: cfg.RequestLogRetentionDays,
	})
	defer requestLog.Close()

	sch := scheduler.New(scheduler.Config{
		Pool:              p,
		Enabled:           cfg.Watch.Enabled,
		CreditsAutoClaim:  cfg.CreditsAutoClaim,
		CheckinHours:      cfg.CheckinHours,
		PollInterval:      time.Duration(cfg.Watch.PollMinutes) * time.Minute,
		RefreshSkew:       time.Duration(cfg.Watch.RefreshSkewM) * time.Minute,
		KeepaliveInterval: time.Duration(cfg.Watch.KeepaliveInterval) * time.Minute,
	})

	// 运行期配置快照：面板保存后热生效的字段都从这里读（见 internal/livecfg）。
	live := livecfg.New(liveSnapshot(cfg))
	settings := &fileSettings{path: cfgPath}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     c,
		APIKey:       cfg.APIKey,
		AdminKey:     cfg.AdminKey,
		Settings:     settings,
		Live:         live,
		RequestLog:   requestLog,
		Balance:      bal,
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
	// 面板保存：校验 → 落盘 → 热应用（settings.update 内串行），返回需重启字段。
	settings.apply = func(c *Config) []string { return applyHotConfig(c, live, p, sch, requestLog) }
	log.SetOutput(io.MultiWriter(os.Stderr, h.LogWriter()))

	go sch.Run(ctx)
	// 模型目录缓存 23h：到期前后台自动续期，用户请求永远命中缓存。
	h.StartModelRefresher(ctx)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	// 先绑定端口再回报就绪：绑定失败（端口被占）要能同步告诉调用方，而不是等到
	// ListenAndServe 才在后台报错。
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		notifyReady(err)
		return err
	}
	notifyReady(nil)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("CodeArts2API Panel listening on %s; settings: %s", cfg.Listen, cfgPath)
	log.Printf("Open /panel and use admin_key from the configuration file. API clients use api_key.")
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	log.Printf("bye")
	return nil
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
