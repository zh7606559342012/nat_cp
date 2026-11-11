// Package modules 负责模块初始化与启动编排（对应 jxtx modules）。
//
// 启动顺序严格对应说明书 5.10.1：
//  1 加载 SQLite 静态配置（main 中 DbConnInit 已完成）→
//  2 构建内存索引 → 3 启动 Callback/OAM 服务 →
//  4 获取 Token → 5 Ping UPF → 6 获取 DNN List → 7 计算 EffectiveDNN →
//  8 按 DNN 拉取一次全量 Session → 9 匹配并生成规则 → 10 向 UP 下发规则 →
//  11 注册 UE 上下线订阅 → 12 进入 RUNNING。
// 其中 8~10 由 handler.FullSyncDNN（分页拉取 + 逐条匹配，只收集期望规则）
// 与 handler.SnapshotSync（RULE_SYNC SNAPSHOT 批量下发，见说明书 V1.1 3.9.23）完成。
//
// Standby（active=false）只加载配置、提供 OAM 与回调服务，
// 不向 UPF 注册订阅、不向 UP 写规则（说明书 11.2）。
package modules

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/sirupsen/logrus"

	"natcp/main/conf"
	"natcp/main/controllers"
	"natcp/main/crontab"
	"natcp/main/handler"
	"natcp/main/httpServer"
	"natcp/main/models"
	"natcp/main/monitor"
	"natcp/main/server"
	"natcp/main/up"
	"natcp/main/upf"
)

// Run 组装全部模块并阻塞到 ctx 取消。
func Run(ctx context.Context, cfg *conf.Config) error {
	log := logrus.WithField("module", "modules")

	// 2. 构建内存索引（不可变 Snapshot）。
	repo := models.New(httpServer.DB)
	snap0, err := buildInitialSnapshot(repo)
	if err != nil {
		return fmt.Errorf("build initial snapshot: %w", err)
	}
	holder := conf.NewHolder(snap0)

	upfCli := upf.New(cfg.UPF.BaseURL, cfg.UPF.User, cfg.UPF.PwdMD5,
		time.Duration(cfg.UPF.TimeoutSec)*time.Second, cfg.UPF.TokenTTLMin, cfg.UPF.TokenRefreshBeforeSec)
	upCli := up.New(cfg.UP.Socket, time.Duration(cfg.UP.TimeoutSec)*time.Second, cfg.UP.MaxRetry)
	eng := handler.New(cfg, holder, upfCli, upCli)

	// 3. 先启动 Callback 与 OAM 服务，再向 UPF 注册订阅。
	//    这样订阅建立前回调已可监听，且 OAM 配置接口始终可用。
	cbSrv := &http.Server{Addr: cfg.Callback.Listen, Handler: server.NewCallbackEngine(eng, cfg.Callback.Path)}
	oamSrv := &http.Server{Addr: cfg.OAM.Listen, Handler: server.NewOAMEngine(controllers.NewOAMServer(repo, holder, eng), cfg.OAM.Path)}
	go func() {
		if err := cbSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Error("callback server exited")
		}
	}()
	go func() {
		if err := oamSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Error("oam server exited")
		}
	}()
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cbSrv.Shutdown(shCtx)
		_ = oamSrv.Shutdown(shCtx)
	}()

	eng.StartEventLoop(ctx)

	if !cfg.Active {
		log.Info("cp started as STANDBY: config and oam/callback servers only, no upf subscribe, no up writes")
		<-ctx.Done()
		return nil
	}

	// 4~7. Token → Ping → DNN List → EffectiveDNN（带退避重试，直到成功或退出）。
	if err := waitUpfReady(ctx, upfCli, eng); err != nil {
		return err
	}
	if err := eng.RefreshUpfDNNs(ctx); err != nil {
		return fmt.Errorf("initial dnn list: %w", err)
	}
	effective := eng.EffectiveDNNs()
	log.WithField("effectiveDNNs", effective).Info("effective dnn set")

	// 8~10. 按 DNN 拉取一次全量 Session，只收集期望规则（不逐条下发）。
	for _, dnn := range effective {
		if _, err := eng.FullSyncDNN(ctx, dnn, false); err != nil {
			log.WithError(err).WithField("dnn", dnn).Error("initial full sync failed, continue with other dnns")
		}
	}

	// 10b. 把全量恢复得到的全部期望规则以 RULE_SYNC SNAPSHOT 批量下发 UP
	//     （说明书 V1.1 3.9.23），处理批次 ACK 后再注册订阅。
	//     快照失败只告警，不阻塞进入 RUNNING；失败规则已在快照内单条重试，
	//     仍失败的按 3.9.19 删除，可由后续事件或手动快照重新生成。
	if err := eng.SnapshotSync(ctx); err != nil {
		log.WithError(err).Warn("initial snapshot sync failed")
	}

	// 11. 注册 UE 上下线订阅；失败则由 monitor 在后续心跳中补注册。
	subscribed := false
	if err := upfCli.Subscribe(ctx, cfg.UPF.CallbackURL, "fusion-access-gw cp"); err != nil {
		log.WithError(err).Warn("initial subscribe failed, monitor will retry")
	} else {
		subscribed = true
	}

	// 12. 进入 RUNNING：链路监控与定时任务接管后续维护。
	mon := monitor.New(upfCli, eng, cfg.UPF.CallbackURL,
		time.Duration(cfg.UPF.PingIntervalSec)*time.Second, subscribed)
	go mon.Start(ctx)
	cron := crontab.New(eng, time.Duration(cfg.UPF.PingIntervalSec)*time.Second)
	go cron.Start(ctx)
	if cfg.Pprof.Listen != "" {
		go func() {
			_ = http.ListenAndServe(cfg.Pprof.Listen, nil)
		}()
		log.WithField("listen", cfg.Pprof.Listen).Info("pprof enabled")
	}
	log.Info("cp entered RUNNING")

	<-ctx.Done()
	// 退出时尽力删除订阅，避免 Standby 抢占最新回调地址。
	if subscribed {
		delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = upfCli.Unsubscribe(delCtx, cfg.UPF.CallbackURL)
		cancel()
	}
	return nil
}

func buildInitialSnapshot(repo *models.Repository) (*conf.Snapshot, error) {
	vxlan, err := repo.ListVxlanConfigs()
	if err != nil {
		return nil, err
	}
	vb, err := repo.ListVxlanBindings()
	if err != nil {
		return nil, err
	}
	fttrb, err := repo.ListFttrbConfigs()
	if err != nil {
		return nil, err
	}
	fb, err := repo.ListFttrbBindings()
	if err != nil {
		return nil, err
	}
	return conf.Build(0, vxlan, vb, fttrb, fb), nil
}

// waitUpfReady 循环执行 Token → Ping，直到成功或 ctx 取消。
// Token 失败退避重试（说明书 5.10.1 步骤 4）。
func waitUpfReady(ctx context.Context, upfCli *upf.Client, eng *handler.Engine) error {
	for {
		if _, err := upfCli.GetToken(ctx); err != nil {
			logrus.WithError(err).Warn("get upf token failed, retry in 5s")
		} else if err := upfCli.Ping(ctx); err != nil {
			logrus.WithError(err).Warn("upf ping failed, retry in 5s")
			eng.SetLinkDown(true)
		} else {
			eng.SetLinkDown(false)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
