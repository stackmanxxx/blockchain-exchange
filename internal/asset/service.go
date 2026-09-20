// Package asset 实现资产账本服务：
// - 复式记账：每笔资金变动生成不可变流水（append-only），可审计、可对账
// - 余额模型：Available（可用）+ Frozen（冻结，下单时锁定）
// - 全部使用最小单位 int64，杜绝浮点误差
//
// MVP 使用内存存储 + mutex；生产环境替换为 MySQL 事务（余额表 + 流水表），
// 且流水必须幂等（同一 RefID 只入账一次）。
package asset

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"blockchain-exchange/internal/domain"
	"blockchain-exchange/pkg/decimal"
)

// LedgerType 流水类型
type LedgerType string

const (
	LedgerDeposit  LedgerType = "DEPOSIT"        // 充值入账
	LedgerWithdraw LedgerType = "WITHDRAW"       // 提现扣减
	LedgerFreeze   LedgerType = "ORDER_FREEZE"   // 下单冻结
	LedgerUnfreeze LedgerType = "ORDER_UNFREEZE" // 撤单解冻
	LedgerTradeOut LedgerType = "TRADE_OUT"      // 成交付出（买方付 quote / 卖方付 base）
	LedgerTradeIn  LedgerType = "TRADE_IN"       // 成交收入
	LedgerFee      LedgerType = "FEE"            // 手续费
)

// Balance 账户余额
type Balance struct {
	Available int64 `json:"available"`
	Frozen    int64 `json:"frozen"`
}

// Entry 账本流水（不可变）
type Entry struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	Asset     string     `json:"asset"`
	Amount    int64      `json:"amount"`  // 正增负减
	Balance   int64      `json:"balance"` // 该资产变动后总余额（可用+冻结），便于对账
	Type      LedgerType `json:"type"`
	RefID     string     `json:"refId"` // 关联单号：订单ID/交易ID/提现ID
	CreatedAt time.Time  `json:"createdAt"`
}

// Service 资产账本服务
type Service struct {
	mu       sync.Mutex
	accounts map[string]map[string]*Balance // userID -> asset -> balance
	ledger   []*Entry
	seq      int64
}

func New() *Service {
	return &Service{
		accounts: make(map[string]map[string]*Balance),
		ledger:   make([]*Entry, 0, 1024),
	}
}

// ---- 查询 ----

// GetBalance 查询单币种余额
func (s *Service) GetBalance(userID, asset string) Balance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(userID, asset)
}

// Balances 查询用户全部余额
func (s *Service) Balances(userID string) map[string]Balance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Balance)
	for asset, b := range s.accounts[userID] {
		out[asset] = *b
	}
	return out
}

// Ledger 查询用户流水（倒序）
func (s *Service) Ledger(userID string, limit int) []*Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Entry, 0, len(s.ledger))
	for i := len(s.ledger) - 1; i >= 0; i-- {
		if s.ledger[i].UserID == userID {
			out = append(out, s.ledger[i])
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

func (s *Service) get(userID, asset string) Balance {
	assets, ok := s.accounts[userID]
	if !ok {
		return Balance{}
	}
	b, ok := assets[asset]
	if !ok {
		return Balance{}
	}
	return *b
}

func (s *Service) ensure(userID, asset string) *Balance {
	assets, ok := s.accounts[userID]
	if !ok {
		assets = make(map[string]*Balance)
		s.accounts[userID] = assets
	}
	b, ok := assets[asset]
	if !ok {
		b = &Balance{}
		assets[asset] = b
	}
	return b
}

func (s *Service) entry(userID, asset string, amount int64, typ LedgerType, refID string) *Entry {
	s.seq++
	e := &Entry{
		ID:        fmt.Sprintf("L%06d", s.seq),
		UserID:    userID,
		Asset:     asset,
		Amount:    amount,
		Type:      typ,
		RefID:     refID,
		CreatedAt: time.Now(),
	}
	// 该币种变动后总余额
	b := s.get(userID, asset)
	e.Balance = b.Available + b.Frozen
	s.ledger = append(s.ledger, e)
	return e
}

// ---- 资金操作 ----

// Deposit 充值/人工入账（可用余额增加）
func (s *Service) Deposit(userID, asset string, qty int64, refID string) error {
	if qty <= 0 {
		return domain.ErrInvalidOrder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.ensure(userID, asset)
	b.Available += qty
	s.entry(userID, asset, qty, LedgerDeposit, refID)
	return nil
}

// Freeze 冻结可用余额（下单校验，余额不足返回 ErrInsufficientFunds）
func (s *Service) Freeze(userID, asset string, qty int64, refID string) error {
	if qty <= 0 {
		return domain.ErrInvalidOrder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.ensure(userID, asset)
	if b.Available < qty {
		return domain.ErrInsufficientFunds
	}
	b.Available -= qty
	b.Frozen += qty
	s.entry(userID, asset, -qty, LedgerFreeze, refID)
	return nil
}

// Unfreeze 解冻（撤单/部分成交剩余释放）
func (s *Service) Unfreeze(userID, asset string, qty int64, refID string) error {
	if qty <= 0 {
		return domain.ErrInvalidOrder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.ensure(userID, asset)
	if b.Frozen < qty {
		return domain.ErrInsufficientFunds
	}
	b.Frozen -= qty
	b.Available += qty
	s.entry(userID, asset, qty, LedgerUnfreeze, refID)
	return nil
}

// ConsumeFrozen 冻结资金永久扣减（提现/手续费支出：冻结不再回滚）
func (s *Service) ConsumeFrozen(userID, asset string, qty int64, refID string) error {
	if qty <= 0 {
		return domain.ErrInvalidOrder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.ensure(userID, asset)
	if b.Frozen < qty {
		return domain.ErrInsufficientFunds
	}
	b.Frozen -= qty
	s.entry(userID, asset, -qty, LedgerWithdraw, refID)
	return nil
}

// SettleTrade 成交清算：买卖双方资金划转（幂等由调用方保证：同一交易ID只结算一次）
//
// 买单方（买 base）：付出 quote = price*qty（从冻结中扣减），收入 base qty（入可用）
// 卖单方（卖 base）：付出 base qty（从冻结中扣减），收入 quote（入可用）
func (s *Service) SettleTrade(t *domain.Trade, sym *domain.Symbol) error {
	// quote = price * qty / 1e8，big.Int 防溢出
	quoteUnits, err := decimal.PriceQtyToQuote(t.PriceUnits, t.QtyUnits)
	if err != nil {
		return err
	}

	var buyerID, sellerID string
	if t.TakerSide == domain.SideBuy {
		buyerID, sellerID = t.TakerUserID, t.MakerUserID
	} else {
		buyerID, sellerID = t.MakerUserID, t.TakerUserID
	}
	if buyerID == sellerID {
		return domain.ErrInvalidOrder
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. 买方：冻结中扣除 quote（付出）
	if b := s.ensure(buyerID, sym.QuoteAsset); b.Frozen < quoteUnits {
		return fmt.Errorf("asset: buyer %s frozen %s insufficient, need %d", buyerID, sym.QuoteAsset, quoteUnits)
	} else {
		b.Frozen -= quoteUnits
		s.entry(buyerID, sym.QuoteAsset, -quoteUnits, LedgerTradeOut, t.ID)
	}
	// 2. 买方：base 入可用（收入）
	if b := s.ensure(buyerID, sym.BaseAsset); true {
		b.Available += t.QtyUnits
		s.entry(buyerID, sym.BaseAsset, t.QtyUnits, LedgerTradeIn, t.ID)
	}
	// 3. 卖方：冻结中扣除 base（付出）
	if b := s.ensure(sellerID, sym.BaseAsset); b.Frozen < t.QtyUnits {
		return fmt.Errorf("asset: seller %s frozen %s insufficient, need %d", sellerID, sym.BaseAsset, t.QtyUnits)
	} else {
		b.Frozen -= t.QtyUnits
		s.entry(sellerID, sym.BaseAsset, -t.QtyUnits, LedgerTradeOut, t.ID)
	}
	// 4. 卖方：quote 入可用（收入）
	if b := s.ensure(sellerID, sym.QuoteAsset); true {
		b.Available += quoteUnits
		s.entry(sellerID, sym.QuoteAsset, quoteUnits, LedgerTradeIn, t.ID)
	}
	return nil
}

// safeMul 大数乘法，溢出 int64 返回错误
func safeMul(a, b int64) (int64, error) {
	res := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !res.IsInt64() {
		return 0, domain.ErrInvalidOrder
	}
	return res.Int64(), nil
}

var _ = safeMul // 预留；金额换算统一走 decimal.PriceQtyToQuote
