// Package controllers 是 HTTP 控制器层（对应 jxtx controllers）。
//
// 本包只做 HTTP 入参解析与响应，业务逻辑在 handler，
// 路由注册与中间件在 server。
package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"natcp/main/handler"
	"natcp/main/webTypes"
)

// CallbackHandler 处理 UPF 的 UE create/delete 推送（说明书 5.6）。
// 响应 200 表示接收成功；400 表示报文无法解析；500 表示内部处理失败
// （UPF 是否重试取决于网元实现，CP 不假设，见说明书 5.10.6）。
func CallbackHandler(e *handler.Engine) gin.HandlerFunc {
	log := logrus.WithField("module", "callback")
	return func(c *gin.Context) {
		var sess webTypes.UeSession
		if err := c.ShouldBindJSON(&sess); err != nil {
			log.WithError(err).Warn("callback bad json")
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "bad json: " + err.Error()})
			return
		}
		sess.SrcIP = sess.NormalizeSrcIP()
		sess.Source = "CALLBACK"
		switch sess.Action {
		case webTypes.SessionCreate:
			if err := e.ApplySession(c.Request.Context(), &sess); err != nil {
				log.WithError(err).Error("callback apply create failed")
				c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
				return
			}
		case webTypes.SessionDelete:
			if err := e.RemoveSession(c.Request.Context(), sess.IMSI, sess.DNN, sess.SrcIP); err != nil {
				log.WithError(err).Error("callback apply delete failed")
				c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
				return
			}
		default:
			log.WithField("action", sess.Action).Warn("callback unknown action")
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "unknown action"})
			return
		}
		// 回 200 前记一行，供测试用例 015 在日志里核对“收到推送并回复 200”。
		log.WithFields(logrus.Fields{"action": sess.Action, "imsi": sess.IMSI, "dnn": sess.DNN}).Info("callback ok")
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "OK"})
	}
}
