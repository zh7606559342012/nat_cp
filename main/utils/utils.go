// Package utils 提供 CP 各层共用的小工具函数（无内部依赖）。
package utils

import (
	"fmt"
	"sync/atomic"
	"time"
)

// NowMilli 返回当前 Unix 毫秒时间戳。SQLite 时间字段统一用 int64 Unix ms。
func NowMilli() int64 { return time.Now().UnixMilli() }

var requestCounter uint64

// NewRequestID 生成 CP-UP 请求唯一 ID，用于 ACK 匹配和幂等。
func NewRequestID() string {
	n := atomic.AddUint64(&requestCounter, 1)
	return fmt.Sprintf("cp-%d-%d", NowMilli(), n)
}
