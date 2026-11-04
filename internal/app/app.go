// Package app 负责 CP 的组装与启动编排。
//
// 启动顺序严格对应说明书 5.10.1：
//  1 加载 SQLite 静态配置 → 2 构建内存索引 → 3 启动 Callback Server →
//  4 获取 Token → 5 Ping UPF → 6 获取 DNN List → 7 计算 EffectiveDNN →
//  8 按 DNN 拉取一次全量 Session → 9 匹配并生成规则 → 10 向 UP 下发规则 →
//  11 注册 UE 上下线订阅 → 12 进入 RUNNING。
// 其中 8~10 由 FullSyncDNN（分页拉取 + 逐条匹配 + ADD/UPSERT 下发）完成。
//
// Standby（active=false）只加载配置、提供 OAM 与回调服务，
// 不向 UPF 注册订阅、不向 UP 写规则（说明书 11.2）。
package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"natcp/internal/callback"
	"natcp/internal/config"
	"natcp/internal/engine"
	"natcp/internal/oam"
	"natcp/internal/snapshot"
	"natcp/internal/store"
	"natcp/internal/up"
	"natcp/internal/upf"
)

// Run 是 CP 主流程：组装全部模块并阻塞到 ctx 取消。
func Run(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	level, err := logrus.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	log := logrus.WithField("module", "app")

	// 1. 加载 SQLite 静态配置；失败则不进入业务 Ready。
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// 2. 构建内存索引（不可变 Snapshot）。
	snap0, err := st.BuildSnapshot(0)
	if err != nil {
		return fmt.Errorf("build initial snapshot: %w", err)
	}
	holder := snapshot.NewHolder(snap0)

	upfCli := upf.New(cfg.UPF.BaseURL, cfg.UPF.User, cfg.UPF.PwdMD5,
		time.Duration(cfg.UPF.TimeoutSec)*time.Second, cfg.UPF.TokenTTLMin, cfg.UPF.TokenRefreshBeforeSec)
	upCli := up.New(cfg.UP.Socket, time.Duration(cfg.UP.TimeoutSec)*time.Second, cfg.UP.MaxRetry)
	eng := engine.New(cfg, st, holder, upfCli, upCli)

	// 3. 先启动 Callback Server 与 OAM Server，再向 UPF 注册订阅。
	//    这样订阅建立前回调已可监听，且 OAM 配置接口始终可用。
	cbSrv := &http.Server{Addr: cfg.Callback.Listen, Handler: callback.New(eng, cfg.Callback.Path).Handler()}
	oamSrv := &http.Server{Addr: cfg.OAM.Listen, Handler: oam.New(st, holder, eng, cfg.OAM.Path).Handler()}
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

	// 8~10. 按 DNN 拉取一次全量 Session 并下发 UP 规则。
	for _, dnn := range effective {
		if _, err := eng.FullSyncDNN(ctx, dnn); err != nil {
			log.WithError(err).WithField("dnn", dnn).Error("initial full sync failed, continue with other dnns")
		}
	}

	// 11. 注册 UE 上下线订阅；失败则进入维护循环继续重试。
	subscribed := false
	if err := upfCli.Subscribe(ctx, cfg.UPF.CallbackURL, "fusion-access-gw cp"); err != nil {
		log.WithError(err).Warn("initial subscribe failed, will retry in maintenance loop")
	} else {
		subscribed = true
	}

	// 12. 进入 RUNNING：心跳维护 + 订阅补注册 + pending DNN 补偿重试。
	log.Info("cp entered RUNNING")
	ticker := time.NewTicker(time.Duration(cfg.UPF.PingIntervalSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 退出时尽力删除订阅，避免 Standby 抢占最新回调地址。
			if subscribed {
				delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = upfCli.Unsubscribe(delCtx, cfg.UPF.CallbackURL)
				cancel()
			}
			return nil
		case <-ticker.C:
			if err := upfCli.Ping(ctx); err != nil {
				log.WithError(err).Warn("upf ping failed, keep existing rules")
				eng.SetLinkDown(true)
				continue
			}
			wasDown := eng.IsLinkDown()
			eng.SetLinkDown(false)
			if wasDown {
				log.Info("upf link recovered")
			}
			if !subscribed {
				if err := upfCli.Subscribe(ctx, cfg.UPF.CallbackURL, "fusion-access-gw cp"); err != nil {
					log.WithError(err).Warn("subscribe retry failed")
				} else {
					subscribed = true
					log.Info("upf subscribe recovered")
				}
			}
			eng.RetryPending()
		}
	}
}

// waitUpfReady 循环执行 Token → Ping，直到成功或 ctx 取消。
// Token 失败退避重试（说明书 5.10.1 步骤 4）。
func waitUpfReady(ctx context.Context, upfCli *upf.Client, eng *engine.Engine) error {
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
