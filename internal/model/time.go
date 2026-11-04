package model

import "time"

// nowMilli 返回当前 Unix 毫秒时间戳。SQLite 时间字段统一用 int64 Unix ms。
func nowMilli() int64 { return time.Now().UnixMilli() }

// NowMilli 供其他包使用的时间戳工具。
func NowMilli() int64 { return nowMilli() }
