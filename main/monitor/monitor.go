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

// verifySubscription 用 GET /ue/subscribe 核对 UPF 侧是否仍有本机回调订阅；
// 丢失则补注册。只在链路恢复时调用，避免每次心跳都多一次查询。
func (m *Monitor) verifySubscription(ctx context.Context) {
	subs, err := m.upf.ListSubscribes(ctx)
	if err != nil {
		m.log.WithError(err).Warn("list subscribes failed, keep current state")
		return
	}
	for _, s := range subs {
		if s.URL == m.callbackURL {
			m.subscribed.Store(true)
			return
		}
	}
	m.log.Warn("upf subscribe missing after link recovery, re-subscribing")
	if err := m.upf.Subscribe(ctx, m.callbackURL, "fusion-access-gw cp"); err != nil {
		m.log.WithError(err).Warn("subscribe retry failed")
		m.subscribed.Store(false)
		return
	}
	m.subscribed.Store(true)
	m.log.Info("upf subscribe recovered")
}

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
		// 链路恢复时核对 UPF 侧订阅是否还在：UPF 重启可能丢订阅，
		// 而本地 subscribed 仍为 true，不核对就再也收不到 UE 事件。
		if m.subscribed.Load() {
			m.verifySubscription(ctx)
		}
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
