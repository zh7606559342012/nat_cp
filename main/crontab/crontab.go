// Package crontab 是定时任务（对应 jxtx crontab）。
//
// CP 的定时任务目前只有一项：定期重试处于 pending 的 DNN 定向全量补偿
// （链路/Token/DNN 不可用时补偿会被挂起，恢复后由这里再次触发）。
package crontab

import (
	"context"
	"time"

	"natcp/main/handler"
)

// Jobs 是定时任务集合。
type Jobs struct {
	engine   *handler.Engine
	interval time.Duration
}

// New 创建定时任务。
func New(e *handler.Engine, interval time.Duration) *Jobs {
	return &Jobs{engine: e, interval: interval}
}

// Start 阻塞运行定时循环，直到 ctx 取消。
func (s *Jobs) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.engine.RetryPending()
		}
	}
}
