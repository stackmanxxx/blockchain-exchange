package api

import (
	"github.com/gin-gonic/gin"

	"blockchain-exchange/internal/ws"
)

// NewRouter 组装全部路由
func NewRouter(h *Handlers, hub *ws.Hub) *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	v1 := r.Group("/api/v1")
	{
		// 公开接口：认证
		v1.POST("/auth/register", h.Register)
		v1.POST("/auth/login", h.Login)

		// 公开接口：行情
		v1.GET("/market/symbols", h.Symbols)
		v1.GET("/market/ticker/:symbol", h.Ticker)
		v1.GET("/market/depth/:symbol", h.Depth)
		v1.GET("/market/klines/:symbol", h.Klines)
		v1.GET("/market/trades/:symbol", h.RecentTrades)

		// 需登录
		authed := v1.Group("", AuthRequired(h.auth))
		{
			authed.GET("/accounts/balances", h.Balances)
			authed.GET("/accounts/ledger", h.Ledger)
			authed.POST("/orders", h.PlaceOrder)
			authed.GET("/orders", h.ListOrders)
			authed.GET("/orders/:orderID", h.GetOrder)
			authed.DELETE("/orders/:orderID", h.CancelOrder)

			// 钱包（链上充提）
			authed.GET("/wallet/deposit-address/:asset", h.DepositAddress)
			authed.POST("/wallet/withdraw", h.Withdraw)
			authed.GET("/wallet/withdrawals", h.Withdrawals)
		}
	}

	// WebSocket 行情推送
	r.GET("/ws", func(c *gin.Context) {
		hub.ServeWS(c.Writer, c.Request)
	})

	return r
}
