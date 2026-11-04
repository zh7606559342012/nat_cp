// Package server 负责 Gin 路由注册与中间件（对应 jxtx server）。
//
// 两个 HTTP 服务：
//   - 回调服务：接收 UPF 的 UE create/delete 推送，另挂健康检查与调试接口；
//   - OAM 服务：接收 OAM 的静态配置调用。
// 控制器实现在 controllers，本包只做路由装配。
package server

import (
	"github.com/gin-gonic/gin"

	"natcp/main/controllers"
	"natcp/main/handler"
)

// NewCallbackEngine 创建回调服务的 Gin 引擎。
func NewCallbackEngine(e *handler.Engine, callbackPath string) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(callbackPath, controllers.CallbackHandler(e))
	r.GET("/healthz", controllers.HealthHandler)
	r.GET("/debug/state", controllers.StateHandler(e))
	r.POST("/debug/snapshot-sync", controllers.SnapshotSyncHandler(e))
	return r
}

// NewOAMEngine 创建 OAM 配置服务的 Gin 引擎。
func NewOAMEngine(oam *controllers.OAMServer, oamPath string) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(oamPath, oam.HandleCall)
	return r
}
