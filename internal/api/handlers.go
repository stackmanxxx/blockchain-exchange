// Package api REST API 层：鉴权、参数校验、响应封装。
// 金额统一以十进制字符串对外展示，内部全部为最小单位 int64。
package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"blockchain-exchange/internal/asset"
	"blockchain-exchange/internal/auth"
	"blockchain-exchange/internal/domain"
	"blockchain-exchange/internal/market"
	"blockchain-exchange/internal/order"
	"blockchain-exchange/internal/wallet"
	"blockchain-exchange/pkg/decimal"
)

// Handlers HTTP 处理器（组装各服务）
type Handlers struct {
	auth   *auth.Service
	assets *asset.Service
	orders *order.Service
	market *market.Service
	wallet *wallet.WalletService
}

func NewHandlers(a *auth.Service, assets *asset.Service, orders *order.Service, mkt *market.Service, w *wallet.WalletService) *Handlers {
	return &Handlers{auth: a, assets: assets, orders: orders, market: mkt, wallet: w}
}

// ---- 响应封装 ----

func ok(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok", "data": data})
}

func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"code": status, "message": msg, "data": nil})
}

func errStatus(err error) (int, string) {
	switch {
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized, err.Error()
	case errors.Is(err, domain.ErrOrderNotFound), errors.Is(err, domain.ErrUserNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, domain.ErrInsufficientFunds),
		errors.Is(err, domain.ErrInvalidOrder),
		errors.Is(err, domain.ErrInvalidPrice),
		errors.Is(err, domain.ErrInsufficientQty),
		errors.Is(err, domain.ErrInsufficientNotional),
		errors.Is(err, domain.ErrSymbolNotFound),
		errors.Is(err, domain.ErrSymbolDisabled),
		errors.Is(err, domain.ErrOrderAlreadyFilled),
		errors.Is(err, domain.ErrDuplicateOrder),
		errors.Is(err, auth.ErrEmailTaken),
		errors.Is(err, auth.ErrBadEmail),
		errors.Is(err, auth.ErrBadPassword),
		errors.Is(err, auth.ErrLoginFailed):
		return http.StatusBadRequest, err.Error()
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

func unitsStr(u int64) string {
	return decimal.FromUnits(u).String()
}

// ---- 认证 ----

type registerReq struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (h *Handlers) Register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	u, err := h.auth.Register(req.Email, req.Password)
	if err != nil {
		status, _ := errStatus(err)
		fail(c, status, err.Error())
		return
	}
	ok(c, gin.H{"id": u.ID, "email": u.Email})
}

type loginReq struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (h *Handlers) Login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	token, u, err := h.auth.Login(req.Email, req.Password)
	if err != nil {
		status, _ := errStatus(err)
		fail(c, status, err.Error())
		return
	}
	ok(c, gin.H{"token": token, "user": gin.H{"id": u.ID, "email": u.Email}})
}

// ---- 账户 ----

func (h *Handlers) Balances(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	ok(c, h.assets.Balances(userID))
}

func (h *Handlers) Ledger(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	limit := intQuery(c, "limit", 20)
	entries := h.assets.Ledger(userID, limit)
	views := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		views = append(views, gin.H{
			"id": e.ID, "type": e.Type, "asset": e.Asset,
			"amount": unitsStr(e.Amount), "balance": unitsStr(e.Balance),
			"refId": e.RefID, "createdAt": e.CreatedAt,
		})
	}
	ok(c, views)
}

// ---- 订单 ----

func (h *Handlers) PlaceOrder(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	var req order.PlaceOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := h.orders.PlaceOrder(userID, req)
	if err != nil {
		status, _ := errStatus(err)
		fail(c, status, err.Error())
		return
	}
	ok(c, gin.H{
		"order":  orderView(res.Order),
		"trades": tradeViews(res.Trades),
	})
}

func (h *Handlers) CancelOrder(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	o, err := h.orders.CancelOrder(userID, c.Param("orderID"))
	if err != nil {
		status, _ := errStatus(err)
		fail(c, status, err.Error())
		return
	}
	ok(c, orderView(o))
}

func (h *Handlers) GetOrder(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	o, err := h.orders.GetOrder(userID, c.Param("orderID"))
	if err != nil {
		status, _ := errStatus(err)
		fail(c, status, err.Error())
		return
	}
	ok(c, orderView(o))
}

func (h *Handlers) ListOrders(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	symbol := c.Query("symbol")
	limit := intQuery(c, "limit", 50)
	orders := h.orders.ListOrders(userID, symbol, limit)
	views := make([]gin.H, 0, len(orders))
	for _, o := range orders {
		views = append(views, orderView(o))
	}
	ok(c, views)
}

// ---- 行情 ----

func (h *Handlers) Symbols(c *gin.Context) {
	ok(c, h.market.Symbols())
}

func (h *Handlers) Ticker(c *gin.Context) {
	tk := h.market.Ticker(c.Param("symbol"))
	if tk == nil {
		fail(c, http.StatusNotFound, "no ticker data")
		return
	}
	ok(c, gin.H{
		"symbol":        tk.Symbol,
		"lastPrice":     unitsStr(tk.LastPriceUnits),
		"openPrice":     unitsStr(tk.OpenPriceUnits),
		"highPrice":     unitsStr(tk.HighPriceUnits),
		"lowPrice":      unitsStr(tk.LowPriceUnits),
		"volume":        unitsStr(tk.VolumeUnits),
		"quoteVolume":   unitsStr(tk.QuoteVolumeUnits),
		"changePercent": tk.ChangePercent,
		"time":          tk.Time,
	})
}

func (h *Handlers) Depth(c *gin.Context) {
	limit := intQuery(c, "limit", 20)
	d := h.market.Depth(c.Param("symbol"), limit)
	if d == nil {
		fail(c, http.StatusNotFound, "symbol not found")
		return
	}
	ok(c, gin.H{
		"symbol": d.Symbol,
		"bids":   depthViews(d.Bids),
		"asks":   depthViews(d.Asks),
		"seq":    d.Seq,
		"time":   d.Time,
	})
}

func (h *Handlers) Klines(c *gin.Context) {
	interval := c.DefaultQuery("interval", "1m")
	limit := intQuery(c, "limit", 100)
	klines := h.market.Klines(c.Param("symbol"), interval, limit)
	if klines == nil {
		fail(c, http.StatusBadRequest, "invalid symbol or interval")
		return
	}
	views := make([]gin.H, 0, len(klines))
	for _, k := range klines {
		views = append(views, gin.H{
			"symbol": k.Symbol, "interval": k.Interval,
			"openTime": k.OpenTime,
			"open":     unitsStr(k.OpenUnits), "high": unitsStr(k.HighUnits),
			"low": unitsStr(k.LowUnits), "close": unitsStr(k.CloseUnits),
			"volume": unitsStr(k.VolumeUnits), "closed": k.Closed,
		})
	}
	ok(c, views)
}

func (h *Handlers) RecentTrades(c *gin.Context) {
	limit := intQuery(c, "limit", 50)
	trades := h.market.RecentTrades(c.Param("symbol"), limit)
	ok(c, tradeViews(trades))
}

// ---- 钱包（链上充提，Phase 2） ----

// DepositAddress 获取充值地址（HD 派生，恒定）
func (h *Handlers) DepositAddress(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	assetSym := c.Param("asset")
	addr, err := h.wallet.GetDepositAddress(userID, assetSym)
	if err != nil {
		status, _ := errStatus(err)
		fail(c, status, err.Error())
		return
	}
	ok(c, gin.H{"asset": assetSym, "address": addr})
}

type withdrawReq struct {
	Asset     string `json:"asset" binding:"required"`
	ToAddress string `json:"toAddress" binding:"required"`
	Amount    string `json:"amount" binding:"required"`
}

// Withdraw 提现申请（MVP 提交即自动审批+广播；生产走双人复核+风控）
func (h *Handlers) Withdraw(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	var req withdrawReq
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	amount, err := decimal.FromString(req.Amount)
	if err != nil || !amount.IsPositive() {
		fail(c, http.StatusBadRequest, "invalid amount")
		return
	}
	w, err := h.wallet.SubmitWithdrawal(h.assets, userID, req.Asset, req.ToAddress, amount.Units.Int64())
	if err != nil {
		status, _ := errStatus(err)
		fail(c, status, err.Error())
		return
	}
	// MVP：自动审批（演示模式，生产由 admin 风控审批）
	w, err = h.wallet.ApproveWithdrawal(h.assets, w.ID, "system")
	if err != nil {
		// 签名/广播失败：保留申请单，返回错误
		fail(c, http.StatusInternalServerError, "withdraw processing failed: "+err.Error())
		return
	}
	ok(c, withdrawalView(w))
}

// Withdrawals 用户提现记录
func (h *Handlers) Withdrawals(c *gin.Context) {
	userID := c.GetString(ctxUserID)
	list := h.wallet.ListWithdrawals(userID)
	views := make([]gin.H, 0, len(list))
	for _, w := range list {
		views = append(views, withdrawalView(w))
	}
	ok(c, views)
}

func withdrawalView(w *wallet.WithdrawalRequest) gin.H {
	return gin.H{
		"id":        w.ID,
		"asset":     w.Asset,
		"toAddress": w.ToAddress,
		"amount":    unitsStr(w.AmountUnits),
		"fee":       unitsStr(w.FeeUnits),
		"status":    w.Status,
		"txHash":    w.TxHash,
		"createdAt": w.CreatedAt,
	}
}

// ---- 视图转换 ----

func orderView(o *domain.Order) gin.H {
	return gin.H{
		"id":           o.ID,
		"symbol":       o.Symbol,
		"side":         o.Side,
		"type":         o.Type,
		"timeInForce":  o.TimeInForce,
		"price":        unitsStr(o.PriceUnits),
		"quantity":     unitsStr(o.QtyUnits),
		"filledQty":    unitsStr(o.FilledQtyUnits),
		"remainingQty": unitsStr(o.RemainingUnits),
		"avgPrice":     unitsStr(o.AvgPriceUnits),
		"status":       o.Status,
		"createdAt":    o.CreatedAt,
		"updatedAt":    o.UpdatedAt,
	}
}

func tradeViews(trades []*domain.Trade) []gin.H {
	views := make([]gin.H, 0, len(trades))
	for _, t := range trades {
		views = append(views, gin.H{
			"id": t.ID, "symbol": t.Symbol,
			"price": unitsStr(t.PriceUnits), "quantity": unitsStr(t.QtyUnits),
			"makerOrderId": t.MakerOrderID, "takerOrderId": t.TakerOrderID,
			"seq": t.Seq, "time": t.CreatedAt,
		})
	}
	return views
}

func depthViews(levels []domain.DepthLevel) []gin.H {
	views := make([]gin.H, 0, len(levels))
	for _, l := range levels {
		views = append(views, gin.H{
			"price": unitsStr(l.PriceUnits), "quantity": unitsStr(l.QtyUnits),
		})
	}
	return views
}

func intQuery(c *gin.Context, key string, def int) int {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
