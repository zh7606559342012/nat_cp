package conf

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// InitLogger 初始化全局日志（对照 jxtx main/conf/loginit.go 的做法）：
// 程序自己打开日志文件追加写，级别与路径都来自配置；文件打不开时
// 回退到 stderr 并记一条警告，不影响启动。所有组件日志都派生自
// 全局 logrus（logrus.WithField），所以改全局输出即覆盖全部模块。
func InitLogger(level, path string) {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		l = logrus.InfoLevel
	}
	logrus.SetLevel(l)

	formatter := &logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
		CallerPrettyfier: func(frame *runtime.Frame) (function string, file string) {
			return "", filepath.Base(frame.File)+":"+strconv.Itoa(frame.Line)
		},
	}
	logrus.SetFormatter(formatter)
	logrus.SetReportCaller(true)

	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		logrus.WithError(err).WithField("logPath", path).Warn("create log dir failed, fallback to stderr")
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logrus.WithError(err).WithField("logPath", path).Warn("open log file failed, fallback to stderr")
		return
	}
	logrus.SetOutput(f)
	// gin 自己的输出（启动时打印的路由表等）也进同一个文件；
	// 代码里没有其他日志出口：生产代码不用标准库 log、不直接写
	// stdout/stderr，全部走全局 logrus。
	gin.DefaultWriter = f
}
