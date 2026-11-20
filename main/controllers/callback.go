// Package controllers 是 HTTP 控制器层（对应 jxtx controllers）。
//
// 本包只做 HTTP 入参解析与响应，业务逻辑在 handler，
// 路由注册与中间件在 server。
package controllers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"natcp/main/handler"
	"natcp/main/webTypes"
)

// CallbackHandler 处理 UPF 的 UE create/delete 推送（说明书 5.6）。
// 响应语义（2026-11-20 领导要求：对 UPF 和对 UP 的内外分开）：
//   - 报文解析正确就回 200 OK，表示 CP 已接收；
//   - 跟 UP 的交互（装规则/删规则）是网关内部的事情，异步进行，不等 UP
//     回结果再回 200；装不装得上 UPF 不用知道、也不用重推。
//   - 400 只表示报文无法解析或 action 未知。
//
// 异步处理用 context.Background()，与 HTTP 请求解耦：UPF（经中继）断开
// 推送连接不再取消正在进行的 UP 下发。处理失败只记日志，可通过
// /debug/state（desired 与 installed 对比）与 rule installed/deleted
// 日志核对。
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
			// 异步处理：先回 200，装规则在后台做。
			go func() {
				if err := e.ApplySession(context.Background(), &sess); err != nil {
					log.WithError(err).WithFields(logrus.Fields{"imsi": sess.IMSI, "dnn": sess.DNN}).Error("callback apply create failed")
				}
			}()
		case webTypes.SessionDelete:
			// 异步处理：先回 200，删规则在后台做。
			// RemoveSession 本身恒返回 nil，失败由内部按会话记日志。
			go func() {
				_ = e.RemoveSession(context.Background(), sess.IMSI, sess.DNN, sess.SrcIP)
			}()
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
