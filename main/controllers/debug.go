package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"natcp/main/handler"
)

// HealthHandler 供探活与负载均衡检查。
func HealthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// StateHandler 返回 CP 运行态（会话数、期望规则、安装结果、链路状态）。
func StateHandler(e *handler.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, e.Stats())
	}
}

// SnapshotSyncHandler 手动触发 RULE_SYNC 全量同步，
// 用于 UP 重启后的规则恢复（说明书 10.23）。
func SnapshotSyncHandler(e *handler.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := e.SnapshotSync(c.Request.Context()); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "OK"})
	}
}
