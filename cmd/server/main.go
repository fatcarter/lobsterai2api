// main.go lobsterai2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lobsterai2api/internal/auth"
	"lobsterai2api/internal/checkin"
	"lobsterai2api/internal/models"
	"lobsterai2api/internal/pool"
	"lobsterai2api/internal/schedule"
	"lobsterai2api/internal/scheduler"
	"lobsterai2api/internal/server"
	"lobsterai2api/internal/stats"
	"lobsterai2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	for _, a := range auths {
		p.Add(a)
	}

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.UpdateURL = cfg.Upstream.UpdateURL

	records := checkin.Load(cfg.CheckinFile, checkin.MaxRecords)

	// 运行时模型存储：启动时从静态表种子，文件存在时优先；管理页 / 定时器可触发刷新。
	modelStore := models.Load(cfg.ModelsFile)

	// 定时设置：schedule_file 存在时优先于 config/env，管理页保存后写入该文件。
	settings := schedule.Load(cfg.ScheduleFile, cfg.Schedule.CheckinHours, cfg.Schedule.KeepaliveHours, cfg.Schedule.ModelRefreshHours, cfg.CreditRefreshDur)

	sch := scheduler.New(scheduler.Config{
		Pool:     p,
		Upstream: up,
		Records:  records,
		Schedule: settings,
		Models:   modelStore,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 请求计数器：每次 chat 完成（成功/失败/非流/流）后累加，30 秒落盘一次，
	// 管理页"统计"页从这里读数据。
	statsRecorder := stats.Load(cfg.StatsFile)
	go statsRecorder.Run(ctx.Done())

	go sch.Run(ctx)

	// 管理页登录复用上游连接池；门户或回调端口缺失时登录接口报配置缺失，其余接口不受影响。
	oauthClient := &auth.OAuthClient{
		BaseURL:     upstream.ServerBase(),
		PortalURL:   cfg.Login.Portal,
		RedirectURI: cfg.CallbackURI(server.OAuthCallbackPath),
		HTTP:        up.HTTP,
	}
	log.Printf("admin page on /admin (oauth_login=%v)", oauthClient.Ready())

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		OAuth:        oauthClient,
		AuthDir:      cfg.AuthDir,
		Records:      records,
		Schedule:     settings,
		Stats:        statsRecorder,
		Models:       modelStore,
	})

	go sch.Run(ctx)
	// 启动后延迟签到一次（立即刷新 pool.credits，不用等整点）；签到被关闭时跳过。
	if checkinH, _, _, _ := settings.Get(); len(checkinH) > 0 {
		go func() {
			time.Sleep(5 * time.Second)
			sch.RunCheckinNow()
		}()
	}

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

	log.Printf("lobsterai2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
