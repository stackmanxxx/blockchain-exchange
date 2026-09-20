// Package order 订单服务：下单校验 → 冻结资金 → 提交撮合 → 清算成交 → 解冻剩余。
// 生产环境拆分为 order-svc（REST/校验/入队）+ settlement-svc（Kafka 消费成交入账），
// MVP 进程内同步完成，但资金流转逻辑与生产一致。
package order

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"blockchain-exchange/internal/asset"
	"blockchain-exchange/internal/domain"
	"blockchain-exchange/internal/engine"
	"blockchain-exchange/pkg/decimal"
)

// PlaceOrderRequest 下单请求
type PlaceOrderRequest struct {
	Symbol        string             `json:"symbol"`
	Side          domain.OrderSide   `json:"side"`
	Type          domain.OrderType   `json:"type"`
	TimeInForce   domain.TimeInForce `json:"timeInForce,omitempty"`
	Price         string             `json:"price,omitempty"`         // 限价单价格
	Quantity      string             `json:"quantity,omitempty"`      // base 数量
	AmountQuote   string             `json:"amountQuote,omitempty"`   // 市价买单金额（quote）
	ClientOrderID string             `json:"clientOrderId,omitempty"` // 客户端订单号（可选）
}

// PlaceOrderResult 下单结果
type PlaceOrderResult struct {
	Order  *domain.Order   `json:"order"`
	Trades []*domain.Trade `json:"trades"`
}

// Service 订单服务
type Service struct {
	assets     *asset.Service
	engines    map[string]*engine.Engine
	symbols    map[string]*domain.Symbol
	orders     map[string]*domain.Order
	userOrders map[string][]string
	seq        atomic.Int64
	mu         sync.RWMutex
}

func NewService(assets *asset.Service, engines map[string]*engine.Engine, symbols []*domain.Symbol) *Service {
	symMap := make(map[string]*domain.Symbol, len(symbols))
	for _, s := range symbols {
		symMap[s.Name] = s
	}
	return &Service{
		assets:     assets,
		engines:    engines,
		symbols:    symMap,
		orders:     make(map[string]*domain.Order),
		userOrders: make(map[string][]string),
	}
}

// PlaceOrder 下单：校验 → 冻结 → 撮合 → 清算 → 处理剩余
func (s *Service) PlaceOrder(userID string, req PlaceOrderRequest) (*PlaceOrderResult, error) {
	sym, ok := s.symbols[req.Symbol]
	if !ok {
		return nil, domain.ErrSymbolNotFound
	}
	if !sym.Enabled {
		return nil, domain.ErrSymbolDisabled
	}

	o, err := s.buildOrder(userID, sym, req)
	if err != nil {
		return nil, err
	}

	// 1. 冻结资金（失败则拒单，不进入撮合）
	if err := s.freeze(o, sym); err != nil {
		o.Status = domain.StatusRejected
		s.storeOrder(o)
		return &PlaceOrderResult{Order: o}, err
	}

	// 2. 提交撮合（同步，事件循环串行）
	eng := s.engines[o.Symbol]
	res := eng.Submit(o)
	if res.Reject != nil {
		// 拒单：解冻已冻结资金
		s.unfreezeRemaining(o, sym)
		o.Status = domain.StatusRejected
		s.storeOrder(o)
		return &PlaceOrderResult{Order: o}, res.Reject
	}

	// 3. 清算成交（同一交易 ID 只结算一次；MVP 同步，生产由 settlement 消费 Kafka 事件）
	for _, t := range res.Trades {
		if err := s.assets.SettleTrade(t, sym); err != nil {
			// 理论上不应发生（冻结已覆盖）；出现即严重错误，向上抛出
			s.storeOrder(o)
			return &PlaceOrderResult{Order: o, Trades: res.Trades}, fmt.Errorf("settle trade %s: %w", t.ID, err)
		}
		if o.IsBuy() {
			spent, err := decimal.PriceQtyToQuote(t.PriceUnits, t.QtyUnits)
			if err != nil {
				s.storeOrder(o)
				return &PlaceOrderResult{Order: o, Trades: res.Trades}, fmt.Errorf("trade amount overflow: %w", err)
			}
			o.SpentUnits += spent // 买单跟踪 quote 消耗
		} else {
			o.SpentUnits = o.FilledQtyUnits // 卖单消耗 base
		}
	}

	// 4. 未成交部分处理：
	//    - 挂单在簿（PARTIALLY_FILLED/NEW）：冻结保留
	//    - 终态（FILLED 但剩余 quote / EXPIRED / REJECTED / 市价未花完）：解冻剩余
	switch o.Status {
	case domain.StatusNew, domain.StatusPartiallyFilled:
		// 冻结保留在簿
	case domain.StatusExpired, domain.StatusRejected, domain.StatusFilled:
		s.unfreezeRemaining(o, sym)
	}

	s.storeOrder(o)
	return &PlaceOrderResult{Order: o, Trades: res.Trades}, nil
}

// buildOrder 构造领域订单并做基础校验
func (s *Service) buildOrder(userID string, sym *domain.Symbol, req PlaceOrderRequest) (*domain.Order, error) {
	if req.Side != domain.SideBuy && req.Side != domain.SideSell {
		return nil, domain.ErrInvalidOrder
	}
	if req.Type != domain.TypeLimit && req.Type != domain.TypeMarket {
		return nil, domain.ErrInvalidOrder
	}
	tif := req.TimeInForce
	if tif == "" {
		tif = domain.TIFGTC
	}
	if req.Type == domain.TypeLimit && tif == "" {
		tif = domain.TIFGTC
	}
	if req.Type == domain.TypeMarket && tif != domain.TIFGTC && tif != "" {
		// 市价单不支持 IOC/FOK（无挂单语义），强制 GTC
		tif = domain.TIFGTC
	}

	o := &domain.Order{
		ID:          s.nextOrderID(),
		UserID:      userID,
		Symbol:      sym.Name,
		Side:        req.Side,
		Type:        req.Type,
		TimeInForce: tif,
		Status:      domain.StatusNew,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	switch req.Type {
	case domain.TypeLimit:
		price, err := decimal.FromString(req.Price)
		if err != nil || !price.IsPositive() {
			return nil, domain.ErrInvalidPrice
		}
		qty, err := decimal.FromString(req.Quantity)
		if err != nil || !qty.IsPositive() {
			return nil, domain.ErrInvalidOrder
		}
		if err := validateQty(sym, qty.Units.Int64()); err != nil {
			return nil, err
		}
		o.Price = price
		o.PriceUnits = price.Units.Int64()
		o.Quantity = qty
		o.QtyUnits = qty.Units.Int64()
		o.RemainingUnits = qty.Units.Int64()
	case domain.TypeMarket:
		if req.Side == domain.SideBuy {
			amt, err := decimal.FromString(req.AmountQuote)
			if err != nil || !amt.IsPositive() {
				return nil, domain.ErrInvalidOrder
			}
			if amt.Units.Int64() < sym.MinNotionalUnits {
				return nil, domain.ErrInsufficientNotional
			}
			o.AmountQuoteUnits = amt.Units.Int64()
			o.RemainingQuoteUnits = amt.Units.Int64()
		} else {
			qty, err := decimal.FromString(req.Quantity)
			if err != nil || !qty.IsPositive() {
				return nil, domain.ErrInvalidOrder
			}
			if err := validateQty(sym, qty.Units.Int64()); err != nil {
				return nil, err
			}
			o.QtyUnits = qty.Units.Int64()
			o.RemainingUnits = qty.Units.Int64()
		}
	}
	return o, nil
}

// freeze 冻结下单资金；记录冻结额到订单（big.Int 校验乘积不溢出 int64）
func (s *Service) freeze(o *domain.Order, sym *domain.Symbol) error {
	switch {
	case o.IsBuy():
		// 买单冻结 quote
		if o.Type == domain.TypeMarket {
			o.FrozenUnits = o.AmountQuoteUnits
			return s.assets.Freeze(o.UserID, sym.QuoteAsset, o.AmountQuoteUnits, o.ID)
		}
		quote, err := decimal.PriceQtyToQuote(o.PriceUnits, o.QtyUnits)
		if err != nil {
			return domain.ErrInvalidOrder // 金额超出可表示范围
		}
		o.FrozenUnits = quote
		return s.assets.Freeze(o.UserID, sym.QuoteAsset, quote, o.ID)
	default:
		// 卖单冻结 base
		o.FrozenUnits = o.QtyUnits
		return s.assets.Freeze(o.UserID, sym.BaseAsset, o.QtyUnits, o.ID)
	}
}

// unfreezeRemaining 解冻未消耗的冻结资金（撤单/终态剩余）
func (s *Service) unfreezeRemaining(o *domain.Order, sym *domain.Symbol) {
	remaining := o.FrozenUnits - o.SpentUnits
	if remaining <= 0 {
		return
	}
	if o.IsBuy() {
		_ = s.assets.Unfreeze(o.UserID, sym.QuoteAsset, remaining, o.ID)
	} else {
		_ = s.assets.Unfreeze(o.UserID, sym.BaseAsset, remaining, o.ID)
	}
	o.FrozenUnits -= remaining
}

// CancelOrder 撤单：撤出订单簿并解冻剩余
func (s *Service) CancelOrder(userID, orderID string) (*domain.Order, error) {
	s.mu.RLock()
	o, ok := s.orders[orderID]
	s.mu.RUnlock()
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	if o.UserID != userID {
		return nil, domain.ErrOrderNotFound
	}
	if o.Status == domain.StatusFilled || o.Status == domain.StatusCanceled {
		return nil, domain.ErrOrderAlreadyFilled
	}

	eng := s.engines[o.Symbol]
	canceled, err := eng.Cancel(orderID)
	if err != nil {
		return nil, err
	}
	sym := s.symbols[o.Symbol]
	s.unfreezeRemaining(canceled, sym)
	return canceled, nil
}

// GetOrder 查询订单
func (s *Service) GetOrder(userID, orderID string) (*domain.Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[orderID]
	if !ok || o.UserID != userID {
		return nil, domain.ErrOrderNotFound
	}
	return o, nil
}

// ListOrders 用户订单列表（可按交易对过滤）
func (s *Service) ListOrders(userID, symbol string, limit int) []*domain.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.userOrders[userID]
	out := make([]*domain.Order, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		o := s.orders[ids[i]]
		if symbol != "" && o.Symbol != symbol {
			continue
		}
		out = append(out, o)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (s *Service) storeOrder(o *domain.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.orders[o.ID]; !exists {
		s.userOrders[o.UserID] = append(s.userOrders[o.UserID], o.ID)
	}
	s.orders[o.ID] = o
}

func (s *Service) nextOrderID() string {
	return fmt.Sprintf("O%012d", s.seq.Add(1))
}

func validateQty(sym *domain.Symbol, qtyUnits int64) error {
	if qtyUnits < sym.MinQtyUnits {
		return domain.ErrInsufficientQty
	}
	return nil
}
