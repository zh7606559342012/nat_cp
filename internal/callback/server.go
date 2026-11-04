// Package callback 实现 CP 接收 UPF POST 推送的回调 HTTP 服务。
//
// UPF 在订阅时拿到 CP 提供的 callback URL；UE 上线/下线时用 POST 把
// 会话 JSON 推到该 URL（说明书 5.6）。CP 收到后只做增量维护，
// 全量基线由启动/补偿流程负责。
package callback

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"natcp/internal/engine"
	"natcp/internal/model"
)

// Server 是回调服务：回调路径 + 健康检查 + 调试状态。
type Server struct {
	engine *engine.Engine
	path   string
	log    *logrus.Entry
}

// New 创建回调服务。
func New(e *engine.Engine, path string) *Server {
	return &Server{engine: e, path: path, log: logrus.WithField("module", "callback")}
}

// Handler 返回 Gin 引擎，供主程序挂到监听地址上。
func (s *Server) Handler() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(s.path, s.handleCallback)
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/debug/state", func(c *gin.Context) { c.JSON(http.StatusOK, s.engine.Stats()) })
	// 手动触发 RULE_SYNC 全量同步：用于 UP 重启后的规则恢复（说明书 10.23）。
	r.POST("/debug/snapshot-sync", func(c *gin.Context) {
		if err := s.engine.SnapshotSync(c.Request.Context()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "OK"})
	})
	return r
}

// handleCallback 处理 UPF 的 UE create/delete 推送。
// 响应 200 表示接收成功；400 表示报文无法解析；500 表示内部处理失败
// （UPF 是否重试取决于网元实现，CP 不假设，见说明书 5.10.6）。
func (s *Server) handleCallback(c *gin.Context) {
	var sess model.UeSession
	if err := c.ShouldBindJSON(&sess); err != nil {
		s.log.WithError(err).Warn("callback bad json")
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "bad json: " + err.Error()})
		return
	}
	sess.SrcIP = sess.NormalizeSrcIP()
	sess.Source = "CALLBACK"
	switch sess.Action {
	case model.SessionCreate:
		if err := s.engine.ApplySession(c.Request.Context(), &sess); err != nil {
			s.log.WithError(err).Error("callback apply create failed")
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
			return
		}
	case model.SessionDelete:
		if err := s.engine.RemoveSession(c.Request.Context(), sess.IMSI, sess.DNN, sess.SrcIP); err != nil {
			s.log.WithError(err).Error("callback apply delete failed")
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
			return
		}
	default:
		s.log.WithField("action", sess.Action).Warn("callback unknown action")
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "unknown action"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "OK"})
}
