package engine

import (
	"container/heap"
	"math/big"
	"time"

	"blockchain-exchange/internal/domain"
	"blockchain-exchange/pkg/decimal"
)

// TradeListener 撮合事件监听（MVP 进程内回调；生产环境改为 Kafka 输出 + 下游消费）
type TradeListener interface {
	// OnTrade 每笔成交
	OnTrade(t *domain.Trade)
	// OnDepthChanged 订单簿变更（供行情服务聚合/推送）
	OnDepthChanged(symbol string, seq int64)
}

// SubmitResult 下单结果（同步返回）
type SubmitResult struct {
	Order  *domain.Order
	Trades []*domain.Trade
	Reject error
}

// cancelReq 撤单请求
type cancelReq struct {
	orderID string
	resp    chan *domain.Order
	err     chan error
}

// Engine 交易对撮合引擎：单 goroutine 事件循环串行处理，
// 内部无锁；对外 Submit/Cancel 通过 channel 提交并同步等待结果。
type Engine struct {
	symbol string
	bids   *orderBook
	asks   *orderBook
	// active 簿内活跃订单（含部分成交未满单），用于 O(1) 撤单
	active    map[string]*domain.Order
	seq       int64
	listeners []TradeListener

	ch chan func()
}

// New 创建交易对撮合引擎并启动事件循环 goroutine
func New(symbol string, listeners ...TradeListener) *Engine {
	e := &Engine{
		symbol:    symbol,
		bids:      newOrderBook(true),
		asks:      newOrderBook(false),
		active:    make(map[string]*domain.Order),
		seq:       0,
		listeners: listeners,
		ch:        make(chan func(), 1024),
	}
	go e.run()
	return e
}

func (e *Engine) run() {
	for fn := range e.ch {
		fn()
	}
}

// Symbol 交易对
func (e *Engine) Symbol() string { return e.symbol }

func (e *Engine) opposite(side domain.OrderSide) *orderBook {
	if side == domain.SideBuy {
		return e.asks
	}
	return e.bids
}

func (e *Engine) same(side domain.OrderSide) *orderBook {
	if side == domain.SideBuy {
		return e.bids
	}
	return e.asks
}

// Submit 提交订单（阻塞直到撮合完成），返回成交列表与最终状态。
// 订单对象会被引擎修改（RemainingUnits/FilledQtyUnits/Status/AvgPriceUnits）。
func (e *Engine) Submit(o *domain.Order) *SubmitResult {
	resp := make(chan *SubmitResult, 1)
	e.ch <- func() { resp <- e.submit(o) }
	return <-resp
}

func (e *Engine) submit(o *domain.Order) *SubmitResult {
	res := &SubmitResult{Order: o, Trades: make([]*domain.Trade, 0, 4)}
	if _, dup := e.active[o.ID]; dup {
		res.Reject = domain.ErrDuplicateOrder
		o.Status = domain.StatusRejected
		return res
	}

	switch o.Type {
	case domain.TypeMarket:
		e.match(o, res)
	case domain.TypeLimit:
		if o.TimeInForce == domain.TIFPOK && !e.canFullyFill(o) {
			o.Status = domain.StatusRejected
			res.Reject = domain.ErrInsufficientQty // FOK 不能全成交
			return res
		}
		e.match(o, res)
	default:
		o.Status = domain.StatusRejected
		res.Reject = domain.ErrInvalidOrder
		return res
	}

	// 状态终态化
	switch {
	case o.Type == domain.TypeMarket:
		// 市价单不挂单：剩余部分自动撤销
		o.Status = domain.StatusFilled
		if o.FilledQtyUnits == 0 {
			o.Status = domain.StatusRejected
			res.Reject = domain.ErrInsufficientQty // 无对手流动性
		}
	case o.RemainingUnits == 0:
		o.Status = domain.StatusFilled
	case o.TimeInForce == domain.TIFIOC:
		o.Status = domain.StatusExpired
	case o.FilledQtyUnits > 0:
		o.Status = domain.StatusPartiallyFilled
		e.same(o.Side).add(o)
		e.active[o.ID] = o
	default:
		o.Status = domain.StatusNew
		e.same(o.Side).add(o)
		e.active[o.ID] = o
	}

	o.UpdatedAt = time.Now()
	if len(res.Trades) > 0 || o.Status == domain.StatusFilled || o.Status == domain.StatusPartiallyFilled {
		e.emitDepthChange()
	}
	return res
}

// 撮合主循环：以最优对手价逐档吃单。
// 市价买单按 quote 金额撮合（剩余可花金额递减），其余按数量撮合。
func (e *Engine) match(o *domain.Order, res *SubmitResult) {
	book := e.opposite(o.Side)
	for {
		// 剩余量检查：吃单者已耗尽则停止（否则 qty=0 提前 break 会形成忙循环）
		if o.IsBuy() && o.Type == domain.TypeMarket {
			if o.RemainingQuoteUnits <= 0 {
				break
			}
		} else if o.RemainingUnits <= 0 {
			break
		}
		level := book.best()
		if level == nil {
			break
		}
		if o.Type == domain.TypeLimit {
			if o.IsBuy() && level.price > o.PriceUnits {
				break // 卖盘最低价仍高于买单价格，无法成交
			}
			if !o.IsBuy() && level.price < o.PriceUnits {
				break
			}
		}
		// 该档位无法成交（如剩余金额不足 1 个最小单位）则停止，避免忙循环
		if !e.consumeLevel(o, level, book, res) {
			break
		}
	}
}

// consumeLevel 消费一个价格档位（FIFO 逐单成交）。
// 返回是否发生了成交；false 表示该档位无法成交（金额/数量不足），调用方应终止撮合。
func (e *Engine) consumeLevel(taker *domain.Order, level *PriceLevel, book *orderBook, res *SubmitResult) bool {
	consumed := false
	for level.totalQty > 0 {
		maker := level.orders[0]
		// 计算本笔成交数量
		var qty int64
		if taker.IsBuy() && taker.Type == domain.TypeMarket {
			// 市价买单：以 quote 金额折算 base 数量（向下取整）
			// qtyUnits = quoteUnits * 1e8 / priceUnits（big.Int 防溢出）
			if level.price <= 0 {
				break
			}
			qty = quoteToQty(taker.RemainingQuoteUnits, level.price)
			if qty > maker.RemainingUnits {
				qty = maker.RemainingUnits
			}
			if qty <= 0 {
				break
			}
		} else {
			qty = min64(taker.RemainingUnits, maker.RemainingUnits)
			if qty <= 0 {
				break
			}
		}
		price := level.price
		consumed = true

		// 更新撮合统计（big.Int 防溢出）
		taker.AvgPriceUnits = weightedAvg(taker.AvgPriceUnits, taker.FilledQtyUnits, price, qty)
		maker.AvgPriceUnits = weightedAvg(maker.AvgPriceUnits, maker.FilledQtyUnits, price, qty)

		taker.RemainingUnits -= qty
		taker.FilledQtyUnits += qty
		maker.RemainingUnits -= qty
		maker.FilledQtyUnits += qty
		level.totalQty -= qty
		// 市价买单：扣减剩余可花金额（price*qty/1e8，big.Int 防溢出）
		if taker.IsBuy() && taker.Type == domain.TypeMarket {
			spent, err := decimal.PriceQtyToQuote(price, qty)
			if err != nil || spent > taker.RemainingQuoteUnits {
				break // 防御：理论上不会发生
			}
			taker.RemainingQuoteUnits -= spent
		}

		// maker 状态
		if maker.RemainingUnits == 0 {
			maker.Status = domain.StatusFilled
			level.popFront()
			delete(e.active, maker.ID)
		} else {
			maker.Status = domain.StatusPartiallyFilled
		}
		maker.UpdatedAt = time.Now()

		e.emitTrade(taker, maker, price, qty, res)
	}
	return consumed
}

// emitTrade 生成成交事件并通知监听器
func (e *Engine) emitTrade(taker, maker *domain.Order, price, qty int64, res *SubmitResult) {
	e.seq++
	t := &domain.Trade{
		ID:           taker.Symbol + "-" + itoa(e.seq),
		Symbol:       e.symbol,
		PriceUnits:   price,
		QtyUnits:     qty,
		MakerOrderID: maker.ID,
		TakerOrderID: taker.ID,
		MakerUserID:  maker.UserID,
		TakerUserID:  taker.UserID,
		TakerSide:    taker.Side,
		MakerSide:    maker.Side,
		Seq:          e.seq,
		CreatedAt:    time.Now(),
	}
	res.Trades = append(res.Trades, t)
	for _, l := range e.listeners {
		l.OnTrade(t)
	}
}

func (e *Engine) emitDepthChange() {
	for _, l := range e.listeners {
		l.OnDepthChanged(e.symbol, e.seq)
	}
}

// canFullyFill 模拟撮合判断能否全部成交（FOK 用）
func (e *Engine) canFullyFill(o *domain.Order) bool {
	book := e.opposite(o.Side)
	need := o.RemainingUnits
	for _, l := range book.levels {
		if l.totalQty <= 0 {
			continue
		}
		if o.Type == domain.TypeLimit {
			if o.IsBuy() && l.price > o.PriceUnits {
				continue
			}
			if !o.IsBuy() && l.price < o.PriceUnits {
				continue
			}
		}
		need -= l.totalQty
		if need <= 0 {
			return true
		}
	}
	return false
}

// Cancel 撤单（阻塞直到完成）。仅对簿内挂单有效。
func (e *Engine) Cancel(orderID string) (*domain.Order, error) {
	resp := make(chan *domain.Order, 1)
	errCh := make(chan error, 1)
	e.ch <- func() {
		o, ok := e.active[orderID]
		if !ok {
			errCh <- domain.ErrOrderNotFound
			return
		}
		book := e.same(o.Side)
		if !book.remove(o) {
			delete(e.active, orderID)
			errCh <- domain.ErrOrderNotFound
			return
		}
		delete(e.active, orderID)
		o.Status = domain.StatusCanceled
		o.UpdatedAt = time.Now()
		resp <- o
		e.emitDepthChange()
	}
	select {
	case o := <-resp:
		return o, nil
	case err := <-errCh:
		return nil, err
	}
}

// Depth 获取深度快照（同步）
func (e *Engine) Depth(limit int) *domain.Depth {
	resp := make(chan *domain.Depth, 1)
	e.ch <- func() {
		d := &domain.Depth{
			Symbol: e.symbol,
			Bids:   e.bids.depth(limit),
			Asks:   e.asks.depth(limit),
			Seq:    e.seq,
			Time:   time.Now(),
		}
		resp <- d
	}
	return <-resp
}

// Snapshot 订单簿快照（生产环境定期落盘/写 Redis，崩溃后与 Kafka 回放配合恢复）
type Snapshot struct {
	Symbol string                   `json:"symbol"`
	Seq    int64                    `json:"seq"`
	Bids   []domain.DepthLevel      `json:"bids"`
	Asks   []domain.DepthLevel      `json:"asks"`
	Active map[string]*domain.Order `json:"active"`
}

func (e *Engine) Snapshot() *Snapshot {
	resp := make(chan *Snapshot, 1)
	e.ch <- func() {
		snap := &Snapshot{
			Symbol: e.symbol,
			Seq:    e.seq,
			Bids:   e.bids.depth(0),
			Asks:   e.asks.depth(0),
			Active: make(map[string]*domain.Order, len(e.active)),
		}
		for id, o := range e.active {
			cp := *o
			snap.Active[id] = &cp
		}
		resp <- snap
	}
	return <-resp
}

// Restore 从快照恢复（仅限事件循环启动前调用）
func (e *Engine) Restore(snap *Snapshot) {
	if snap == nil {
		return
	}
	e.seq = snap.Seq
	for _, lv := range snap.Bids {
		e.restoreLevel(e.bids, lv)
	}
	for _, lv := range snap.Asks {
		e.restoreLevel(e.asks, lv)
	}
}

func (e *Engine) restoreLevel(book *orderBook, lv domain.DepthLevel) {
	// 快照只保留聚合深度，活跃订单明细在内存版中随进程存活；
	// 生产环境恢复时需回放 Kafka 中的未快照订单
	if lv.QtyUnits <= 0 {
		return
	}
	l := newPriceLevel(lv.PriceUnits)
	l.totalQty = lv.QtyUnits
	book.levels[lv.PriceUnits] = l
	heap.Push(&book.hp, l)
}

// ---- 工具 ----

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// weightedAvg 加权均价：(curAvg*curQty + price*addQty) / (curQty+addQty)，big.Int 防溢出
func weightedAvg(curAvg, curQty, price, addQty int64) int64 {
	if curQty+addQty <= 0 {
		return 0
	}
	total := new(big.Int).Mul(big.NewInt(curAvg), big.NewInt(curQty))
	total.Add(total, new(big.Int).Mul(big.NewInt(price), big.NewInt(addQty)))
	return total.Div(total, big.NewInt(curQty+addQty)).Int64()
}

// quoteToQty quote 金额（最小单位）换算为 base 数量（最小单位），向下取整
func quoteToQty(quoteUnits, priceUnits int64) int64 {
	if priceUnits <= 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(quoteUnits), big.NewInt(decimal.Base))
	num.Div(num, big.NewInt(priceUnits))
	if !num.IsInt64() {
		return 0
	}
	return num.Int64()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
