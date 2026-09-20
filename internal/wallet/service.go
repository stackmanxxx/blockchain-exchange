package wallet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"blockchain-exchange/internal/asset"
	"blockchain-exchange/internal/domain"
)

// OnDepositFn 充值入账回调（由组装层注入，对接 asset.Service.Deposit）
type OnDepositFn func(userID, assetSymbol string, amountUnits int64, txHash string) error

// Config 钱包服务配置
type Config struct {
	Mnemonic     string // BIP39 助记词（生产从 KMS 获取）
	Assets       []*Asset
	Chains       []ChainConfig
	PollInterval time.Duration // 扫块间隔
	StartHeight  map[Chain]uint64
	OnDeposit    OnDepositFn
}

// WalletService 钱包服务
type WalletService struct {
	hd        *HDWallet
	assets    map[string]*Asset // key: symbol（如 "BTC"、"ETH"、"USDT_ERC20"）
	adapters  map[Chain]ChainAdapter
	poll      time.Duration
	startH    map[Chain]uint64
	onDeposit OnDepositFn

	mu sync.RWMutex
	// userAddr: "userID:assetSymbol" -> address
	userAddr map[string]string
	// addrUser: address -> "userID:assetSymbol"
	addrUser map[string]string
	// withdrawals: id -> request
	withdrawals map[string]*WithdrawalRequest
	// userWithdrawals: userID -> []id
	userWithdrawals map[string][]string
	// credited: 已入账 txKey（txHash:assetSymbol），幂等
	credited map[string]struct{}
	// hotIndex: 热钱包派生 index（固定 0；生产支持多热钱包轮换）
	seq atomic.Int64
}

// New 构造钱包服务（MVP 进程内；生产拆分为 wallet-svc 微服务）
func New(cfg Config) (*WalletService, error) {
	if cfg.Mnemonic == "" {
		return nil, errors.New("wallet: mnemonic required")
	}
	hd, err := NewHDWallet(cfg.Mnemonic)
	if err != nil {
		return nil, err
	}
	svc := &WalletService{
		hd:              hd,
		assets:          make(map[string]*Asset),
		adapters:        make(map[Chain]ChainAdapter),
		poll:            cfg.PollInterval,
		startH:          cfg.StartHeight,
		onDeposit:       cfg.OnDeposit,
		userAddr:        make(map[string]string),
		addrUser:        make(map[string]string),
		withdrawals:     make(map[string]*WithdrawalRequest),
		userWithdrawals: make(map[string][]string),
		credited:        make(map[string]struct{}),
	}
	if svc.poll <= 0 {
		svc.poll = 5 * time.Second
	}
	for _, a := range cfg.Assets {
		svc.assets[a.Symbol] = a
	}
	return svc, nil
}

// RegisterAdapter 注册链适配器（组装层调用）
func (s *WalletService) RegisterAdapter(a ChainAdapter) {
	s.adapters[a.Chain()] = a
}

// Asset 查询资产配置
func (s *WalletService) Asset(symbol string) (*Asset, bool) {
	a, ok := s.assets[symbol]
	return a, ok
}

// Adapter 查询链适配器
func (s *WalletService) Adapter(chain Chain) (ChainAdapter, bool) {
	a, ok := s.adapters[chain]
	return a, ok
}

// ---- 地址管理 ----

// GetDepositAddress 为用户分配充值地址（HD 派生，同一用户同资产恒定地址）
func (s *WalletService) GetDepositAddress(userID, assetSymbol string) (string, error) {
	key := userID + ":" + assetSymbol
	s.mu.RLock()
	if addr, ok := s.userAddr[key]; ok {
		s.mu.RUnlock()
		return addr, nil
	}
	s.mu.RUnlock()

	assetCfg, ok := s.assets[assetSymbol]
	if !ok {
		return "", domain.ErrSymbolNotFound
	}
	// 用户地址派生：account=0，index=自增（首个 index=1，index 0 保留给热钱包）
	index := uint32(s.nextIndex())
	addr, _, err := s.hd.DeriveAddress(assetSymbol, 0, index)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.userAddr[key]; ok { // 并发双分配保护
		return existing, nil
	}
	s.userAddr[key] = addr
	s.addrUser[addr] = key
	_ = assetCfg
	return addr, nil
}

func (s *WalletService) nextIndex() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq.Add(1)
	return s.seq.Load()
}

// MatchUser 通过地址反查用户与资产（充值扫描用）
func (s *WalletService) MatchUser(address string) (userID, assetSymbol string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, found := s.addrUser[address]
	if !found {
		return "", "", false
	}
	// key 格式 "userID:assetSymbol"
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

// ---- 充值 ----

// DepositAddresses 用户充值地址列表（API）
func (s *WalletService) DepositAddresses(userID string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string)
	for key, addr := range s.userAddr {
		for i := 0; i < len(key); i++ {
			if key[i] == ':' && key[:i] == userID {
				out[key[i+1:]] = addr
			}
		}
	}
	return out
}

// CreditDeposit 充值入账（幂等：同一 txHash+asset 只入账一次）。
// 由扫描器在确认数达标后调用。
func (s *WalletService) CreditDeposit(userID, assetSymbol string, amountUnits int64, txHash string) error {
	key := txHash + ":" + assetSymbol
	s.mu.Lock()
	if _, done := s.credited[key]; done {
		s.mu.Unlock()
		return nil // 幂等
	}
	s.credited[key] = struct{}{}
	s.mu.Unlock()

	if s.onDeposit == nil {
		return errors.New("wallet: onDeposit callback not configured")
	}
	return s.onDeposit(userID, assetSymbol, amountUnits, txHash)
}

// ---- 提现 ----

// SubmitWithdrawal 提现申请：冻结资产（含手续费）→ PENDING
func (s *WalletService) SubmitWithdrawal(assets *asset.Service, userID, assetSymbol, toAddress string, amountUnits int64) (*WithdrawalRequest, error) {
	assetCfg, ok := s.assets[assetSymbol]
	if !ok {
		return nil, domain.ErrSymbolNotFound
	}
	if !assetCfg.WithdrawEnabled {
		return nil, domain.ErrInvalidOrder
	}
	if amountUnits < assetCfg.MinWithdrawUnits {
		return nil, domain.ErrInsufficientNotional
	}
	if toAddress == "" {
		return nil, domain.ErrInvalidOrder
	}
	// 冻结本金 + 手续费
	if err := assets.Freeze(userID, assetSymbol, amountUnits, "w:"+assetSymbol); err != nil {
		return nil, err
	}
	if assetCfg.FeeUnits > 0 {
		if err := assets.Freeze(userID, assetSymbol, assetCfg.FeeUnits, "w-fee:"+assetSymbol); err != nil {
			_ = assets.Unfreeze(userID, assetSymbol, amountUnits, "w-rollback")
			return nil, err
		}
	}

	w := &WithdrawalRequest{
		ID:          fmt.Sprintf("W%010d", s.seq.Add(1)),
		UserID:      userID,
		Asset:       assetSymbol,
		ToAddress:   toAddress,
		AmountUnits: amountUnits,
		FeeUnits:    assetCfg.FeeUnits,
		Status:      WithdrawPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	s.mu.Lock()
	s.withdrawals[w.ID] = w
	s.userWithdrawals[userID] = append(s.userWithdrawals[userID], w.ID)
	s.mu.Unlock()
	return w, nil
}

// ApproveWithdrawal 审批通过：签名并广播（MVP 自动审批；生产双人复核 + 风控）
func (s *WalletService) ApproveWithdrawal(assets *asset.Service, id, reviewer string) (*WithdrawalRequest, error) {
	w, err := s.getWithdrawal(id)
	if err != nil {
		return nil, err
	}
	if w.Status != WithdrawPending {
		return nil, errors.New("wallet: withdrawal not pending")
	}
	assetCfg := s.assets[w.Asset]
	adapter, ok := s.adapters[assetCfg.Chain]
	if !ok {
		return nil, errors.New("wallet: chain adapter not registered")
	}
	// 热钱包私钥（HD account 0 index 0；生产由 KMS 托管）
	_, hotKey, err := s.hd.DeriveAddress(w.Asset, 0, 0)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rawHex, err := adapter.BuildSignedTx(ctx, hotKey, w.ToAddress, assetCfg, w.AmountUnits, 0)
	if err != nil {
		return nil, fmt.Errorf("wallet: sign: %w", err)
	}
	txHash, err := adapter.Broadcast(ctx, rawHex)
	if err != nil {
		return nil, fmt.Errorf("wallet: broadcast: %w", err)
	}

	w.Status = WithdrawBroadcast
	w.TxHash = txHash
	w.Reviewer = reviewer
	w.UpdatedAt = time.Now()
	// 广播成功后：冻结本金+手续费永久扣减
	if err := assets.ConsumeFrozen(w.UserID, w.Asset, w.AmountUnits, w.ID); err != nil {
		return nil, err
	}
	if w.FeeUnits > 0 {
		_ = assets.ConsumeFrozen(w.UserID, w.Asset, w.FeeUnits, w.ID+"-fee")
	}
	return w, nil
}

// RejectWithdrawal 驳回：解冻本金与手续费
func (s *WalletService) RejectWithdrawal(assets *asset.Service, id, reviewer, reason string) (*WithdrawalRequest, error) {
	w, err := s.getWithdrawal(id)
	if err != nil {
		return nil, err
	}
	if w.Status != WithdrawPending {
		return nil, errors.New("wallet: withdrawal not pending")
	}
	_ = assets.Unfreeze(w.UserID, w.Asset, w.AmountUnits, w.ID)
	if w.FeeUnits > 0 {
		_ = assets.Unfreeze(w.UserID, w.Asset, w.FeeUnits, w.ID+"-fee")
	}
	w.Status = WithdrawRejected
	w.Reviewer = reviewer
	w.RejectReason = reason
	w.UpdatedAt = time.Now()
	return w, nil
}

// ConfirmWithdrawal 跟踪提现确认（由扫描器周期性调用，确认后置 SUCCESS）
func (s *WalletService) ConfirmWithdrawal(id string) (*WithdrawalRequest, error) {
	w, err := s.getWithdrawal(id)
	if err != nil {
		return nil, err
	}
	if w.Status != WithdrawBroadcast || w.TxHash == "" {
		return w, nil
	}
	adapter := s.adapters[s.assets[w.Asset].Chain]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conf, err := adapter.TxConfirmations(ctx, w.TxHash)
	if err != nil {
		return w, err
	}
	if conf > 0 {
		w.Status = WithdrawSuccess
		w.UpdatedAt = time.Now()
	}
	return w, nil
}

func (s *WalletService) getWithdrawal(id string) (*WithdrawalRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.withdrawals[id]
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	return w, nil
}

// ListWithdrawals 用户提现记录
func (s *WalletService) ListWithdrawals(userID string) []*WithdrawalRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.userWithdrawals[userID]
	out := make([]*WithdrawalRequest, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		out = append(out, s.withdrawals[ids[i]])
	}
	return out
}
