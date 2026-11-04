package conf

import (
	"github.com/sirupsen/logrus"
)

// InitLogger 初始化全局日志级别（对应 jxtx conf 的日志初始化）。
func InitLogger(level string) {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		l = logrus.InfoLevel
	}
	logrus.SetLevel(l)
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
}
