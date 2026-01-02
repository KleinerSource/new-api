package router

import (
	"github.com/QuantumNous/new-api/custom/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

// SetCustomRouter 设置自定义扩展路由
// 此文件用于二次开发功能，与上游代码分离，便于合并更新
func SetCustomRouter(router *gin.Engine) {
	// ==================== 余额查询接口 ====================
	// /usage/api - Token 相关接口
	usageBalanceRoute := router.Group("/usage/api")
	usageBalanceRoute.Use(middleware.CriticalRateLimit())
	usageBalanceRoute.Use(middleware.TokenAuth())
	{
		usageBalanceRoute.GET("/balance", controller.GetTokenBalance)
		usageBalanceRoute.GET("/get-models", controller.GetModels)
	}

	// ==================== 透传模式路由 ====================
	// /chat-stream - 透传模式路由（根路径）
	chatStreamRouter := router.Group("/chat-stream")
	chatStreamRouter.Use(middleware.TokenAuth())
	chatStreamRouter.Use(middleware.ModelRequestRateLimit())
	{
		chatStreamRouter.POST("", controller.RelayPassthrough)
	}
}

