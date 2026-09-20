package wallet

import (
	"context"
	"log"
	"math/big"
	"time"
)

// Scanner 充值扫描器：按区块高度轮询各链，解析充值交易，
// 确认数达标后幂等入账。崩溃恢复：从 StartHeight（生产持久化到 Redis/DB）续扫。
type Scanner struct {
	svc       *WalletService
	chainFrom map[Chain]uint64
	// pending: 已发现未达确认数的交易（txKey -> 下次再查确认数）
	pending map[string]uint64 // txKey -> 已见确认数（MVP 简化：不持久化）
}

func NewScanner(svc *WalletService) *Scanner {
	chainFrom := make(map[Chain]uint64)
	for chain, h := range svc.startH {
		chainFrom[chain] = h
	}
	return &Scanner{
		svc:       svc,
		chainFrom: chainFrom,
		pending:   make(map[string]uint64),
	}
}

// Run 启动扫块循环（阻塞，直到 ctx 取消）
func (s *Scanner) Run(ctx context.Context) {
	ticker := time.NewTicker(s.svc.poll)
	defer ticker.Stop()
	log.Printf("wallet: scanner started, poll=%s", s.svc.poll)
	for {
		select {
		case <-ctx.Done():
			log.Printf("wallet: scanner stopped")
			return
		case <-ticker.C:
			s.scanOnce(ctx)
		}
	}
}

// scanOnce 扫描所有已注册链的增量区块
func (s *Scanner) scanOnce(ctx context.Context) {
	for chain, adapter := range s.svc.adapters {
		latest, err := adapter.LatestHeight(ctx)
		if err != nil {
			log.Printf("wallet: [%s] latest height: %v", chain, err)
			continue
		}
		from := s.chainFrom[chain]
		if from == 0 || from > latest {
			// 首次启动或链回滚：从最新区块回退安全深度（2 块）开始
			if latest > 2 {
				from = latest - 2
			} else {
				from = 0
			}
		}
		if from > latest {
			continue
		}
		for h := from + 1; h <= latest; h++ {
			if err := s.scanBlock(ctx, chain, adapter, h); err != nil {
				log.Printf("wallet: [%s] scan block %d: %v", chain, h, err)
				break
			}
		}
		s.chainFrom[chain] = latest
	}
}

// scanBlock 解析单块内所有交易，匹配用户充值地址
func (s *Scanner) scanBlock(ctx context.Context, chain Chain, adapter ChainAdapter, height uint64) error {
	block, err := adapter.Block(ctx, height)
	if err != nil {
		return err
	}
	for _, tx := range block.Txs {
		userID, assetSymbol, ok := s.svc.MatchUser(tx.To)
		if !ok {
			continue // 非用户充值地址（可能是热钱包归集/提现找零）
		}
		assetCfg, ok := s.svc.Asset(assetSymbol)
		if !ok {
			continue
		}
		// 金额精度换算：链上精度 → 内部 1e8
		amount := toUnitsInternal(tx.AmountUnits, assetCfg.Decimals)
		if amount <= 0 {
			log.Printf("wallet: [%s] tx %s zero amount (addr %s), skip", chain, tx.Hash, tx.To)
			continue
		}
		// 确认数达标才入账；未达标记 pending（后续轮询 TxConfirmations）
		conf := tx.Confirmations
		if assetCfg.Confirmations > 0 {
			if conf < assetCfg.Confirmations {
				conf = s.pollConfirmations(ctx, adapter, tx.Hash, assetCfg.Confirmations)
				if conf < assetCfg.Confirmations {
					continue
				}
			}
		}
		if err := s.svc.CreditDeposit(userID, assetSymbol, amount, tx.Hash); err != nil {
			log.Printf("wallet: credit deposit %s failed: %v", tx.Hash, err)
		} else {
			log.Printf("wallet: credited %s %d units to %s (tx %s)", assetSymbol, amount, userID, tx.Hash)
		}
	}
	return nil
}

// pollConfirmations 对未确认交易查询确认数（MVP 立即查一次；生产按块推进状态机）
func (s *Scanner) pollConfirmations(ctx context.Context, adapter ChainAdapter, hash string, need uint64) uint64 {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conf, err := adapter.TxConfirmations(cctx, hash)
	if err != nil {
		return 0
	}
	return conf
}

// toUnitsInternal 链上最小单位 → 内部 1e8 最小单位
//
//	decimals == 8：相等；< 8：放大；> 8：缩小（与 evm.toUnits 一致）
func toUnitsInternal(chainUnits int64, decimals int) int64 {
	if decimals == 8 {
		return chainUnits
	}
	if decimals < 8 {
		// 放大 ×10^(8-decimals)
		exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(8-decimals)), nil)
		res := new(big.Int).Mul(big.NewInt(chainUnits), exp)
		if !res.IsInt64() {
			return 0
		}
		return res.Int64()
	}
	// 缩小 ÷10^(decimals-8)
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-8)), nil)
	div := new(big.Int).Div(big.NewInt(chainUnits), exp)
	if !div.IsInt64() {
		return 0
	}
	return div.Int64()
}
