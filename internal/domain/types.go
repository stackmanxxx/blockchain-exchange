// Package domain 定义交易所核心领域模型与错误。
package domain

import (
	"errors"
	"time"

	"blockchain-exchange/pkg/decimal"
)

// ---- 订单 ----

// OrderSide 买卖方向
type OrderSide string

const (
	SideBuy  OrderSide = "BUY"
	SideSell OrderSide = "SELL"
)

// OrderType 订单类型
type OrderType string

const (
	TypeLimit  OrderType = "LIMIT"  // 限价单
	TypeMarket OrderType = "MARKET" // 市价单
)

// TimeInForce 生效策略
type TimeInForce string

const (
	TIFGTC TimeInForce = "GTC" // 一直有效直至成交或撤单
	TIFIOC TimeInForce = "IOC" // 立即成交剩余撤销
	TIFPOK TimeInForce = "FOK" // 全部成交否则撤销
)

// OrderStatus 订单状态机：
// NEW → PARTIALLY_FILLED → FILLED
//
//	↘ CANCELED
//
// NEW/REJECTED：下单即失败；EXPIRED：IOC/FOK 部分或全部未成交
type OrderStatus string

const (
	StatusNew             OrderStatus = "NEW"
	StatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	StatusFilled          OrderStatus = "FILLED"
	StatusCanceled        OrderStatus = "CANCELED"
	StatusRejected        OrderStatus = "REJECTED"
	StatusExpired         OrderStatus = "EXPIRED"
)

// Order 订单（领域对象，撮合引擎内部使用最小单位 int64 的价格/数量）
type Order struct {
	ID          string          `json:"id"`
	UserID      string          `json:"userId"`
	Symbol      string          `json:"symbol"`
	Side        OrderSide       `json:"side"`
	Type        OrderType       `json:"type"`
	TimeInForce TimeInForce     `json:"timeInForce"`
	Price       decimal.Decimal `json:"-"`
	PriceUnits  int64           `json:"-"`
	Quantity    decimal.Decimal `json:"-"`
	QtyUnits    int64           `json:"-"`
	// AmountQuoteUnits 市价买单的 quote 金额（最小单位）；限价单/市价卖单为 0
	AmountQuoteUnits int64 `json:"-"`
	// 撮合过程中由引擎维护
	RemainingUnits      int64       `json:"-"`
	RemainingQuoteUnits int64       `json:"-"` // 市价买单剩余可花 quote
	Status              OrderStatus `json:"status"`
	CreatedAt           time.Time   `json:"createdAt"`
	UpdatedAt           time.Time   `json:"updatedAt"`
	// 执行统计
	FilledQtyUnits int64 `json:"-"`
	AvgPriceUnits  int64 `json:"-"` // 累计均价（加权）
	// 资金结算跟踪（order service 维护）
	FrozenUnits int64 `json:"-"` // 初始冻结额：买单=quote，卖单=base
	SpentUnits  int64 `json:"-"` // 已结算消耗：买单=quote，卖单=base
}

// IsBuy 是否买单
func (o *Order) IsBuy() bool { return o.Side == SideBuy }

// ---- 成交 ----

// Trade 一笔撮合成交（maker/taker 视角）
type Trade struct {
	ID           string    `json:"id"`
	Symbol       string    `json:"symbol"`
	PriceUnits   int64     `json:"price"`
	QtyUnits     int64     `json:"quantity"`
	MakerOrderID string    `json:"makerOrderId"`
	TakerOrderID string    `json:"takerOrderId"`
	MakerUserID  string    `json:"makerUserId"`
	TakerUserID  string    `json:"takerUserId"`
	TakerSide    OrderSide `json:"takerSide"` // 吃单方向（清算用）
	MakerSide    OrderSide `json:"makerSide"`
	Seq          int64     `json:"seq"` // 交易对内单调递增序号，下游按序消费
	CreatedAt    time.Time `json:"createdAt"`
}

// ---- 交易对 ----

// Symbol 交易对配置，如 BTC/USDT
type Symbol struct {
	Name             string `json:"name"`           // BTCUSDT
	BaseAsset        string `json:"baseAsset"`      // BTC
	QuoteAsset       string `json:"quoteAsset"`     // USDT
	PricePrecision   int    `json:"pricePrecision"` // 价格小数位
	QtyPrecision     int    `json:"qtyPrecision"`   // 数量小数位
	MinQtyUnits      int64  `json:"-"`              // 最小下单数量（最小单位）
	MinNotionalUnits int64  `json:"-"`              // 最小成交额（quote 最小单位）
	Enabled          bool   `json:"enabled"`
}

// ---- 行情 ----

// DepthLevel 深度档位
type DepthLevel struct {
	PriceUnits int64 `json:"price"`
	QtyUnits   int64 `json:"quantity"`
}

// Depth 订单簿深度快照
type Depth struct {
	Symbol string       `json:"symbol"`
	Bids   []DepthLevel `json:"bids"` // 买盘，从最高价降序
	Asks   []DepthLevel `json:"asks"` // 卖盘，从最低价升序
	Seq    int64        `json:"seq"`
	Time   time.Time    `json:"time"`
}

// Kline K线
type Kline struct {
	Symbol      string `json:"symbol"`
	Interval    string `json:"interval"`
	OpenTime    int64  `json:"openTime"` // 毫秒
	OpenUnits   int64  `json:"open"`
	HighUnits   int64  `json:"high"`
	LowUnits    int64  `json:"low"`
	CloseUnits  int64  `json:"close"`
	VolumeUnits int64  `json:"volume"` // base 成交量
	CloseTime   int64  `json:"closeTime"`
	Closed      bool   `json:"closed"`
}

// Ticker 24h 行情概要
type Ticker struct {
	Symbol           string `json:"symbol"`
	LastPriceUnits   int64  `json:"lastPrice"`
	OpenPriceUnits   int64  `json:"openPrice"`
	HighPriceUnits   int64  `json:"highPrice"`
	LowPriceUnits    int64  `json:"lowPrice"`
	VolumeUnits      int64  `json:"volume"`
	QuoteVolumeUnits int64  `json:"quoteVolume"`
	ChangePercent    string `json:"changePercent"`
	Time             int64  `json:"time"`
}

// ---- 错误 ----

var (
	ErrOrderNotFound        = errors.New("order not found")
	ErrSymbolNotFound       = errors.New("symbol not found")
	ErrInsufficientFunds    = errors.New("insufficient funds")
	ErrInvalidOrder         = errors.New("invalid order")
	ErrOrderAlreadyFilled   = errors.New("order already filled or canceled")
	ErrUnauthorized         = errors.New("unauthorized")
	ErrUserNotFound         = errors.New("user not found")
	ErrInvalidSymbol        = errors.New("invalid symbol")
	ErrInsufficientQty      = errors.New("quantity below minimum")
	ErrInsufficientNotional = errors.New("notional below minimum")
	ErrInvalidPrice         = errors.New("invalid price")
	ErrMarketPriceOnly      = errors.New("market order does not require price")
	ErrDuplicateOrder       = errors.New("duplicate order id")
	ErrSymbolDisabled       = errors.New("symbol disabled")
)

// 默认交易对配置（MVP 内置，生产环境由 admin 后台管理）
func DefaultSymbols() []*Symbol {
	return []*Symbol{
		{
			Name: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT",
			PricePrecision: 2, QtyPrecision: 6,
			MinQtyUnits: 1_000_000 /*0.01 BTC*/, MinNotionalUnits: 5_000_000_000, /*5 USDT*/
			Enabled: true,
		},
		{
			Name: "ETHUSDT", BaseAsset: "ETH", QuoteAsset: "USDT",
			PricePrecision: 2, QtyPrecision: 5,
			MinQtyUnits: 1_000_000 /*0.01 ETH*/, MinNotionalUnits: 5_000_000_000,
			Enabled: true,
		},
		{
			Name: "ETHBTC", BaseAsset: "ETH", QuoteAsset: "BTC",
			PricePrecision: 6, QtyPrecision: 5,
			MinQtyUnits: 1_000_000, MinNotionalUnits: 1_000_000, /*0.01 BTC*/
			Enabled: true,
		},
	}
}
