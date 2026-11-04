// CP 主程序：融合接入网关控制面。
//
// 职责：维护 OAM 静态配置与 UPF 动态 UE 会话，完成 ToB/ToH 业务识别
// 与规则生成，并驱动 UP 安装转发规则。启动顺序与接口定义见
// 《Fusion Access GW CP 接口与数据结构设计说明书》V0.5。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"

	"natcp/internal/app"
)

func main() {
	cfgPath := flag.String("config", "configs/cp.yaml", "path to cp.yaml")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, *cfgPath); err != nil {
		logrus.WithError(err).Fatal("cp exited with error")
		os.Exit(1)
	}
	logrus.Info("cp stopped")
}
