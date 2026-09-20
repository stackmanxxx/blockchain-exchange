// Package engine 实现内存撮合引擎：价格优先、时间优先的订单簿撮合。
// 每个交易对一个 Engine 实例，单 goroutine 事件循环串行处理所有请求（无锁、确定性）。
// 生产环境中：撮合事件经 Kafka 输出，订单簿定期快照 + 回放 Kafka 实现秒级恢复。
package engine

import (
	"container/heap"
	"sort"

	"blockchain-exchange/internal/domain"
)

// PriceLevel 同一价格档位：FIFO 队列（时间优先）
type PriceLevel struct {
	price    int64
	totalQty int64 // 该价位剩余可成交量（最小单位）
	orders   []*domain.Order
}

func newPriceLevel(price int64) *PriceLevel {
	return &PriceLevel{price: price, orders: make([]*domain.Order, 0, 8)}
}

func (l *PriceLevel) push(o *domain.Order) {
	l.orders = append(l.orders, o)
	l.totalQty += o.RemainingUnits
}

// 从队头弹出已完成的订单，返回是否全部弹出
func (l *PriceLevel) popFront() {
	l.orders = l.orders[1:]
}

// orderBook 单向订单簿。
// 使用 container/heap 维护价格序（买单降序/卖单升序），map 提供 O(1) 价格定位。
// heap 中可能残留已清空的 level，通过 totalQty==0 惰性跳过。
type orderBook struct {
	levels map[int64]*PriceLevel
	hp     levelHeap
}

func newOrderBook(isBid bool) *orderBook {
	ob := &orderBook{
		levels: make(map[int64]*PriceLevel),
		hp:     levelHeap{isBid: isBid},
	}
	heap.Init(&ob.hp)
	return ob
}

// best 最优价档位；不存在返回 nil
func (ob *orderBook) best() *PriceLevel {
	for ob.hp.Len() > 0 {
		top := ob.hp.levels[0]
		if top.totalQty == 0 || !ob.valid(top) {
			heap.Pop(&ob.hp)
			continue
		}
		return top
	}
	return nil
}

func (ob *orderBook) valid(l *PriceLevel) bool {
	cur, ok := ob.levels[l.price]
	return ok && cur == l
}

// add 插入订单到对应价格档位
func (ob *orderBook) add(o *domain.Order) {
	l, ok := ob.levels[o.PriceUnits]
	if !ok {
		l = newPriceLevel(o.PriceUnits)
		ob.levels[o.PriceUnits] = l
		heap.Push(&ob.hp, l)
	}
	l.push(o)
}

// remove 从订单簿移除指定订单（撤单）。返回是否找到。
func (ob *orderBook) remove(o *domain.Order) bool {
	l, ok := ob.levels[o.PriceUnits]
	if !ok {
		return false
	}
	for i, cur := range l.orders {
		if cur == o {
			l.orders = append(l.orders[:i], l.orders[i+1:]...)
			l.totalQty -= o.RemainingUnits
			if l.totalQty <= 0 || len(l.orders) == 0 {
				delete(ob.levels, o.PriceUnits)
			}
			return true
		}
	}
	return false
}

// depth 导出深度快照（按价格聚合），limit 限制档位数
func (ob *orderBook) depth(limit int) []domain.DepthLevel {
	levels := make([]*PriceLevel, 0, len(ob.levels))
	for _, l := range ob.levels {
		if l.totalQty > 0 {
			levels = append(levels, l)
		}
	}
	sort.Slice(levels, func(i, j int) bool {
		if ob.hp.isBid {
			return levels[i].price > levels[j].price // 买盘降序
		}
		return levels[i].price < levels[j].price // 卖盘升序
	})
	if limit > 0 && len(levels) > limit {
		levels = levels[:limit]
	}
	out := make([]domain.DepthLevel, 0, len(levels))
	for _, l := range levels {
		out = append(out, domain.DepthLevel{PriceUnits: l.price, QtyUnits: l.totalQty})
	}
	return out
}

// levelHeap 价格优先堆：买单大顶堆（价高优先），卖单小顶堆（价低优先）
type levelHeap struct {
	levels []*PriceLevel
	isBid  bool
}

func (h levelHeap) Len() int { return len(h.levels) }

func (h levelHeap) Less(i, j int) bool {
	if h.isBid {
		return h.levels[i].price > h.levels[j].price
	}
	return h.levels[i].price < h.levels[j].price
}

func (h levelHeap) Swap(i, j int) { h.levels[i], h.levels[j] = h.levels[j], h.levels[i] }

func (h *levelHeap) Push(x any) { h.levels = append(h.levels, x.(*PriceLevel)) }

func (h *levelHeap) Pop() any {
	old := h.levels
	n := len(old)
	x := old[n-1]
	h.levels = old[:n-1]
	return x
}
