package engine

import (
	"testing"
	"time"

	"blockchain-exchange/internal/domain"
)

func newOrder(id, user string, side domain.OrderSide, typ domain.OrderType, price, qty int64) *domain.Order {
	o := &domain.Order{
		ID:             id,
		UserID:         user,
		Symbol:         "BTCUSDT",
		Side:           side,
		Type:           typ,
		TimeInForce:    domain.TIFGTC,
		PriceUnits:     price,
		QtyUnits:       qty,
		RemainingUnits: qty,
		Status:         domain.StatusNew,
		CreatedAt:      time.Now(),
	}
	if typ == domain.TypeMarket && side == domain.SideBuy {
		o.AmountQuoteUnits = price // 复用 price 字段传入 quote 金额（测试约定）
		o.RemainingQuoteUnits = price
		o.QtyUnits = 0
	}
	return o
}

type collector struct {
	trades []*domain.Trade
	depth  int
}

func (c *collector) OnTrade(t *domain.Trade)            { c.trades = append(c.trades, t) }
func (c *collector) OnDepthChanged(s string, seq int64) { c.depth++ }

func newTestEngine(t *testing.T) (*Engine, *collector) {
	t.Helper()
	col := &collector{}
	return New("BTCUSDT", col), col
}

// 同价对敲：买单 100@10 吃卖单 100@10
func TestLimitMatch(t *testing.T) {
	e, col := newTestEngine(t)
	sell := newOrder("S1", "alice", domain.SideSell, domain.TypeLimit, 10, 100)
	e.Submit(sell)
	buy := newOrder("B1", "bob", domain.SideBuy, domain.TypeLimit, 10, 100)
	res := e.Submit(buy)

	if res.Order.Status != domain.StatusFilled {
		t.Fatalf("buy order should be FILLED, got %s", res.Order.Status)
	}
	if sell.Status != domain.StatusFilled {
		t.Fatalf("sell order should be FILLED, got %s", sell.Status)
	}
	if len(res.Trades) != 1 {
		t.Fatalf("expect 1 trade, got %d", len(res.Trades))
	}
	trade := res.Trades[0]
	if trade.PriceUnits != 10 || trade.QtyUnits != 100 {
		t.Fatalf("trade price/qty wrong: %d/%d", trade.PriceUnits, trade.QtyUnits)
	}
	if trade.TakerSide != domain.SideBuy || trade.MakerSide != domain.SideSell {
		t.Fatalf("trade sides wrong: %s/%s", trade.TakerSide, trade.MakerSide)
	}
	if len(col.trades) != 1 {
		t.Fatalf("listener should get 1 trade, got %d", len(col.trades))
	}
}

// 价格优先：买单 120@10 先吃最优卖价 90@10，再吃 100@10
func TestPricePriority(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Submit(newOrder("S1", "a", domain.SideSell, domain.TypeLimit, 100, 10))
	e.Submit(newOrder("S2", "b", domain.SideSell, domain.TypeLimit, 90, 10))
	e.Submit(newOrder("S3", "c", domain.SideSell, domain.TypeLimit, 110, 10))

	buy := newOrder("B1", "d", domain.SideBuy, domain.TypeLimit, 120, 20)
	res := e.Submit(buy)
	if len(res.Trades) != 2 {
		t.Fatalf("expect 2 trades, got %d", len(res.Trades))
	}
	if res.Trades[0].PriceUnits != 90 || res.Trades[1].PriceUnits != 100 {
		t.Fatalf("trades should hit 90 then 100, got %d,%d", res.Trades[0].PriceUnits, res.Trades[1].PriceUnits)
	}
	// 卖盘 110 不应成交
	if e.Depth(10).Asks[0].PriceUnits != 110 {
		t.Fatalf("ask 110 should remain, got %+v", e.Depth(10).Asks)
	}
}

// 时间优先：同一价格，先挂单先成交
func TestTimePriority(t *testing.T) {
	e, _ := newTestEngine(t)
	s1 := newOrder("S1", "a", domain.SideSell, domain.TypeLimit, 100, 5)
	s2 := newOrder("S2", "b", domain.SideSell, domain.TypeLimit, 100, 5)
	e.Submit(s1)
	e.Submit(s2)

	buy := newOrder("B1", "c", domain.SideBuy, domain.TypeLimit, 100, 8)
	res := e.Submit(buy)

	if s1.Status != domain.StatusFilled {
		t.Fatalf("s1 should be FILLED (time priority), got %s", s1.Status)
	}
	if s2.Status != domain.StatusPartiallyFilled {
		t.Fatalf("s2 should be PARTIALLY_FILLED, got %s", s2.Status)
	}
	if len(res.Trades) != 2 {
		t.Fatalf("expect 2 trades, got %d", len(res.Trades))
	}
	if res.Trades[0].MakerOrderID != "S1" || res.Trades[1].MakerOrderID != "S2" {
		t.Fatalf("maker order priority wrong: %s,%s", res.Trades[0].MakerOrderID, res.Trades[1].MakerOrderID)
	}
}

// 部分成交 + 挂单保留
func TestPartialFill(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Submit(newOrder("S1", "a", domain.SideSell, domain.TypeLimit, 100, 10))
	buy := newOrder("B1", "b", domain.SideBuy, domain.TypeLimit, 100, 5)
	e.Submit(buy)
	if buy.Status != domain.StatusFilled || buy.FilledQtyUnits != 5 {
		t.Fatalf("buy fill wrong: status=%s filled=%d", buy.Status, buy.FilledQtyUnits)
	}
	// 卖单剩余 5 仍在簿上
	d := e.Depth(10)
	if len(d.Asks) != 1 || d.Asks[0].QtyUnits != 5 {
		t.Fatalf("ask should have 5 remaining, got %+v", d.Asks)
	}
}

// 撤单
func TestCancel(t *testing.T) {
	e, _ := newTestEngine(t)
	o := newOrder("B1", "bob", domain.SideBuy, domain.TypeLimit, 90, 10)
	e.Submit(o)
	canceled, err := e.Cancel("B1")
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if canceled.Status != domain.StatusCanceled {
		t.Fatalf("status should be CANCELED, got %s", canceled.Status)
	}
	if len(e.Depth(10).Bids) != 0 {
		t.Fatalf("book should be empty after cancel")
	}
	if _, err := e.Cancel("B1"); err != domain.ErrOrderNotFound {
		t.Fatalf("second cancel should fail with ErrOrderNotFound, got %v", err)
	}
}

// 市价买单按 quote 金额成交
// 价格 100 USDT = 10_000_000_000 units；110 USDT = 11_000_000_000 units
// 数量 2 BTC = 200_000_000 units；1 BTC = 100_000_000 units
// 金额 250 USDT = 25_000_000_000 units
func TestMarketBuy(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Submit(newOrder("S1", "a", domain.SideSell, domain.TypeLimit, 10_000_000_000, 200_000_000)) // 2 BTC @ 100
	e.Submit(newOrder("S2", "b", domain.SideSell, domain.TypeLimit, 11_000_000_000, 100_000_000)) // 1 BTC @ 110

	buy := newOrder("B1", "c", domain.SideBuy, domain.TypeMarket, 25_000_000_000, 0)
	res := e.Submit(buy)
	if buy.Status != domain.StatusFilled {
		t.Fatalf("market buy should be FILLED, got %s", buy.Status)
	}
	// 2 BTC @100 花 200 USDT + 剩余 50 USDT 买 0.45454545 BTC @110
	if len(res.Trades) != 2 {
		t.Fatalf("expect 2 trades, got %d", len(res.Trades))
	}
	if res.Trades[0].PriceUnits != 10_000_000_000 || res.Trades[0].QtyUnits != 200_000_000 {
		t.Fatalf("first trade wrong: %+v", res.Trades[0])
	}
	if res.Trades[1].PriceUnits != 11_000_000_000 || res.Trades[1].QtyUnits != 45_454_545 {
		t.Fatalf("second trade wrong: %+v", res.Trades[1])
	}
	if buy.FilledQtyUnits != 245_454_545 {
		t.Fatalf("filled qty wrong: %d", buy.FilledQtyUnits)
	}
	// 剩余 quote：25e9 - 20e9 - 11e9*45454545/1e8 = 5e9 - 4999999950 = 50 units ≈ 0
	if buy.RemainingQuoteUnits != 50 {
		t.Fatalf("remaining quote wrong: %d", buy.RemainingQuoteUnits)
	}
}

// IOC：不能全成交则剩余撤销
func TestIOC(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Submit(newOrder("S1", "a", domain.SideSell, domain.TypeLimit, 100, 5))
	buy := newOrder("B1", "b", domain.SideBuy, domain.TypeLimit, 100, 10)
	buy.TimeInForce = domain.TIFIOC
	e.Submit(buy)
	if buy.Status != domain.StatusExpired {
		t.Fatalf("IOC should be EXPIRED, got %s", buy.Status)
	}
	if buy.FilledQtyUnits != 5 {
		t.Fatalf("IOC filled should be 5, got %d", buy.FilledQtyUnits)
	}
	if len(e.Depth(10).Bids) != 0 {
		t.Fatalf("IOC should not rest on book")
	}
}

// FOK：不能全成交直接拒绝
func TestFOK(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Submit(newOrder("S1", "a", domain.SideSell, domain.TypeLimit, 100, 5))
	buy := newOrder("B1", "b", domain.SideBuy, domain.TypeLimit, 100, 10)
	buy.TimeInForce = domain.TIFPOK
	res := e.Submit(buy)
	if res.Reject == nil {
		t.Fatalf("FOK should be rejected")
	}
	if buy.Status != domain.StatusRejected {
		t.Fatalf("FOK status should be REJECTED, got %s", buy.Status)
	}
}

// 快照 + 恢复
func TestSnapshotRestore(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Submit(newOrder("S1", "a", domain.SideSell, domain.TypeLimit, 100, 5))
	e.Submit(newOrder("B1", "b", domain.SideBuy, domain.TypeLimit, 95, 3))
	snap := e.Snapshot()
	if len(snap.Bids) != 1 || len(snap.Asks) != 1 {
		t.Fatalf("snapshot books wrong: bids=%d asks=%d", len(snap.Bids), len(snap.Asks))
	}

	e2 := New("BTCUSDT")
	e2.Restore(snap)
	d := e2.Depth(10)
	if d.Asks[0].QtyUnits != 5 || d.Bids[0].QtyUnits != 3 {
		t.Fatalf("restored book wrong: %+v", d)
	}
}
