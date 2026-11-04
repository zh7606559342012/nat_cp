// Package monitor 是监控模块（对应 jxtx monitor）。
//
// 负责 UPF 链路监控：周期性 Ping UPF，维护链路状态；
// 心跳失败只标记链路异常，不删除已有规则（说明书 5.10.6）；
// 链路恢复或订阅丢失时补注册 UE 上下线订阅。
package monitor

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"natcp/main/handler"
	"natcp/main/upf"
)

// Monitor 是 UPF 链路监控器。
type Monitor struct {
	upf         *upf.Client
	engine      *handler.Engine
	callbackURL string
	interval    time.Duration
	subscribed  atomic.Bool
	log         *logrus.Entry
}

// New 创建链路监控器。subscribed 为启动阶段首次订阅的结果。
func New(upfCli *upf.Client, e *handler.Engine, callbackURL string, interval time.Duration, subscribed bool) *Monitor {
	m := &Monitor{
		upf:         upfCli,
		engine:      e,
		callbackURL: callbackURL,
		interval:    interval,
		log:         logrus.WithField("module", "monitor"),
	}
	m.subscribed.Store(subscribed)
	return m
}

// SetSubscribed 更新订阅状态。
func (m *Monitor) SetSubscribed(v bool) { m.subscribed.Store(v) }

// Start 阻塞运行心跳循环，直到 ctx 取消。
func (m *Monitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

func (m *Monitor) tick(ctx context.Context) {
	if err := m.upf.Ping(ctx); err != nil {
		m.log.WithError(err).Warn("upf ping failed, keep existing rules")
		m.engine.SetLinkDown(true)
		return
	}
	wasDown := m.engine.IsLinkDown()
	m.engine.SetLinkDown(false)
	if wasDown {
		m.log.Info("upf link recovered")
	}
	if !m.subscribed.Load() {
		if err := m.upf.Subscribe(ctx, m.callbackURL, "fusion-access-gw cp"); err != nil {
			m.log.WithError(err).Warn("subscribe retry failed")
			return
		}
		m.subscribed.Store(true)
		m.log.Info("upf subscribe recovered")
	}
}
