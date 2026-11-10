// CP 主程序：融合接入网关控制面（对应 jxtx main/main.go 的入口编排）。
//
// 启动流程：配置加载（conf）→ 日志初始化（conf）→
// 数据库连接初始化（httpServer）→ 模块初始化与启动编排（modules）。
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"

	"natcp/main/conf"
	"natcp/main/httpServer"
	"natcp/main/modules"
)

// Version 由 make_package.sh 在编译时通过 -ldflags 注入，未注入时为 dev。
var Version = "dev"

func main() {
	cfgPath := flag.String("config", "conf/cp.yaml", "path to cp.yaml")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := conf.Load(*cfgPath)
	if err != nil {
		logrus.WithError(err).Fatal("load config failed")
	}
	conf.InitLogger(cfg.LogLevel, cfg.LogPath)
	logrus.WithField("version", Version).Info("natcp starting")

	if err := httpServer.DbConnInit(cfg.DBPath); err != nil {
		logrus.WithError(err).Fatal("db init failed")
	}
	defer httpServer.Close()

	if err := modules.Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		logrus.WithError(err).Fatal("cp exited with error")
		os.Exit(1)
	}
	logrus.Info("cp stopped")
}
