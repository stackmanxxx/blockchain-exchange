// Package market 行情聚合服务：消费撮合成交事件，聚合 K线/Ticker/最近成交，
// 并通过 WebSocket Hub 推送。深度快照从撮合引擎实时拉取。
//
// MVP 内存聚合（服务启动起累计）；生产环境：
// - K线/Ticker 基于 ClickHouse 历史 + 滚动窗口聚合
// - 成交事件从 Kafka 消费，WebSocket 网关水平扩展（Redis Pub/Sub 或消息代理做跨实例广播）
package market

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"blockchain-exchange/internal/domain"
	"blockchain-exchange/internal/ws"
)

// intervalMs 支持的 K线周期（毫秒）
var intervalMs = map[string]int64{
	"1m": 60_000, "5m": 300_000, "15m": 900_000,
	"1h": 3_600_000, "4h": 14_400_000, "1d": 86_400_000,
}

// Service 行情服务
type Service struct {
	hub     *ws.Hub
	symbols map[string]*domain.Symbol

	mu          sync.Mutex
	klines      map[string][]*domain.Kline // key: symbol:interval
	lastTrades  map[string][]*domain.Trade // symbol -> 最近成交（新→旧）
	tickers     map[string]*domain.Ticker
	depthPushAt map[string]time.Time // 深度推送节流
	depthFn     func(symbol string, limit int) *domain.Depth
}

func NewService(hub *ws.Hub, symbols []*domain.Symbol) *Service {
	symMap := make(map[string]*domain.Symbol, len(symbols))
	for _, s := range symbols {
		symMap[s.Name] = s
	}
	return &Service{
		hub:         hub,
		symbols:     symMap,
		klines:      make(map[string][]*domain.Kline),
		lastTrades:  make(map[string][]*domain.Trade),
		tickers:     make(map[string]*domain.Ticker),
		depthPushAt: make(map[string]time.Time),
	}
}

// ---- 撮合事件监听 ----

// OnTrade 撮合成交事件：聚合 K线/Ticker/最近成交并推送
func (s *Service) OnTrade(t *domain.Trade) {
	s.mu.Lock()
	s.aggregate(t)
	s.mu.Unlock()

	s.hub.Publish("trade:"+t.Symbol, t)
}

// OnDepthChanged 订单簿变更：节流推送深度快照（200ms）。
// 注意：本回调在撮合引擎事件循环内执行，绝不能同步调用引擎方法（会自死锁），
// 因此深度拉取放入独立 goroutine（异步），生产环境由 Kafka 输出后由行情网关消费。
func (s *Service) OnDepthChanged(symbol string, seq int64) {
	s.mu.Lock()
	last, ok := s.depthPushAt[symbol]
	now := time.Now()
	if ok && now.Sub(last) < 200*time.Millisecond {
		s.mu.Unlock()
		return // 节流：丢弃本次
	}
	s.depthPushAt[symbol] = now
	s.mu.Unlock()
	go s.pushDepth(symbol, seq)
}

// pushDepth 异步拉取深度并推送（脱离撮合事件循环，避免自死锁）
func (s *Service) pushDepth(symbol string, seq int64) {
	d := s.Depth(symbol, 50)
	if d != nil {
		s.hub.Publish("depth:"+symbol, d)
	}
}

// aggregate 聚合单笔成交到 K线/Ticker/最近成交
func (s *Service) aggregate(t *domain.Trade) {
	sym := s.symbols[t.Symbol]

	// 最近成交（保留 200 条）
	trades := s.lastTrades[t.Symbol]
	trades = append([]*domain.Trade{t}, trades...)
	if len(trades) > 200 {
		trades = trades[:200]
	}
	s.lastTrades[t.Symbol] = trades

	// Ticker（MVP：启动累计；生产用 24h 滚动窗口）
	tk := s.tickers[t.Symbol]
	if tk == nil {
		tk = &domain.Ticker{Symbol: t.Symbol, OpenPriceUnits: t.PriceUnits}
		s.tickers[t.Symbol] = tk
	}
	tk.LastPriceUnits = t.PriceUnits
	if t.PriceUnits > tk.HighPriceUnits {
		tk.HighPriceUnits = t.PriceUnits
	}
	if tk.LowPriceUnits == 0 || t.PriceUnits < tk.LowPriceUnits {
		tk.LowPriceUnits = t.PriceUnits
	}
	tk.VolumeUnits += t.QtyUnits
	if qv, ok := safeAddQuote(tk.QuoteVolumeUnits, t.PriceUnits, t.QtyUnits); ok {
		tk.QuoteVolumeUnits = qv
	} // 溢出时跳过本次累加（现实场景不会发生）
	tk.Time = time.Now().UnixMilli()
	if tk.OpenPriceUnits > 0 {
		tk.ChangePercent = fmt.Sprintf("%.2f%%", float64(tk.LastPriceUnits-tk.OpenPriceUnits)/float64(tk.OpenPriceUnits)*100)
	}
	s.hub.Publish("ticker:"+t.Symbol, tk)

	// K线（各周期）
	now := time.Now().UnixMilli()
	for interval, ms := range intervalMs {
		key := t.Symbol + ":" + interval
		arr := s.klines[key]
		openTime := now - now%ms
		var cur *domain.Kline
		if len(arr) > 0 && !arr[len(arr)-1].Closed {
			cur = arr[len(arr)-1]
		}
		if cur == nil || cur.OpenTime != openTime {
			// 上一根已收盘
			if cur != nil {
				cur.Closed = true
			}
			cur = &domain.Kline{
				Symbol: t.Symbol, Interval: interval,
				OpenTime: openTime, OpenUnits: t.PriceUnits,
				HighUnits: t.PriceUnits, LowUnits: t.PriceUnits,
				CloseUnits: t.PriceUnits,
				CloseTime:  openTime + ms,
			}
			arr = append(arr, cur)
			if len(arr) > 1000 {
				arr = arr[len(arr)-1000:]
			}
			s.klines[key] = arr
		}
		if t.PriceUnits > cur.HighUnits {
			cur.HighUnits = t.PriceUnits
		}
		if t.PriceUnits < cur.LowUnits {
			cur.LowUnits = t.PriceUnits
		}
		cur.CloseUnits = t.PriceUnits
		cur.VolumeUnits += t.QtyUnits
		cur.CloseTime = openTime + ms

		_ = sym
		s.hub.Publish("kline:"+key, cur)
	}
}

// ---- 查询接口（REST/WS 快照） ----

// Symbols 交易对列表
func (s *Service) Symbols() []*domain.Symbol {
	out := make([]*domain.Symbol, 0, len(s.symbols))
	for _, sym := range s.symbols {
		out = append(out, sym)
	}
	return out
}

// Klines 最近 limit 根 K线（含当前未收盘）
func (s *Service) Klines(symbol, interval string, limit int) []*domain.Kline {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := intervalMs[interval]; !ok {
		return nil
	}
	arr := s.klines[symbol+":"+interval]
	if len(arr) == 0 {
		return nil
	}
	if limit <= 0 || limit > len(arr) {
		limit = len(arr)
	}
	out := make([]*domain.Kline, limit)
	copy(out, arr[len(arr)-limit:])
	return out
}

// RecentTrades 最近成交（新→旧）
func (s *Service) RecentTrades(symbol string, limit int) []*domain.Trade {
	s.mu.Lock()
	defer s.mu.Unlock()
	arr := s.lastTrades[symbol]
	if len(arr) == 0 {
		return nil
	}
	if limit <= 0 || limit > len(arr) {
		limit = len(arr)
	}
	out := make([]*domain.Trade, limit)
	copy(out, arr[:limit])
	return out
}

// Ticker 24h 概要（MVP 为累计值）
func (s *Service) Ticker(symbol string) *domain.Ticker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tickers[symbol]
}

// Depth 订单簿深度（实时拉取撮合引擎）
func (s *Service) Depth(symbol string, limit int) *domain.Depth {
	// 由组装层注入 depthProvider 避免循环依赖
	if s.depthFn != nil {
		return s.depthFn(symbol, limit)
	}
	return nil
}

// SetDepthProvider 注入深度获取函数（组装时调用）
func (s *Service) SetDepthProvider(fn func(symbol string, limit int) *domain.Depth) {
	s.mu.Lock()
	s.depthFn = fn
	s.mu.Unlock()
}

var _ = trimPercent

func trimPercent(f float64) string {
	return fmt.Sprintf("%.2f%%", f)
}

// safeAddQuote quote 成交额累加（big.Int 防溢出），溢出返回 false
func safeAddQuote(cur, price, qty int64) (int64, bool) {
	res := new(big.Int).Mul(big.NewInt(price), big.NewInt(qty))
	if !res.IsInt64() {
		return cur, false
	}
	sum := new(big.Int).Add(big.NewInt(cur), res)
	if !sum.IsInt64() {
		return cur, false
	}
	return sum.Int64(), true
}
