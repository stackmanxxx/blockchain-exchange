package wallet

import (
	"context"
	"log"
	"time"
)

// Sweeper 归集器：将散落在用户充值地址的资产定期归集到热钱包。
// 触发条件：地址余额 >= 归集阈值（MVP 配置固定 100 最小单位，生产按币种配置）。
// 安全：用户地址私钥与热钱包同源（HD 派生），归集即热钱包地址向热钱包转账。
type Sweeper struct {
	svc       *WalletService
	threshold int64
}

func NewSweeper(svc *WalletService, threshold int64) *Sweeper {
	if threshold <= 0 {
		threshold = 100_000_000 // 1 单位
	}
	return &Sweeper{svc: svc, threshold: threshold}
}

// Run 定时归集（阻塞直到 ctx 取消）
func (sp *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sp.sweepOnce(ctx)
		}
	}
}

// sweepOnce 遍历所有用户充值地址，余额超阈值则归集到热钱包
func (sp *Sweeper) sweepOnce(ctx context.Context) {
	// 收集地址快照（避免遍历时加锁）
	type addrEntry struct {
		address string
		userID  string
		asset   string
	}
	entries := make([]addrEntry, 0)
	sp.svc.mu.RLock()
	for key, addr := range sp.svc.userAddr {
		userID, assetSymbol := splitAddrKey(key)
		if userID == "" {
			continue
		}
		entries = append(entries, addrEntry{address: addr, userID: userID, asset: assetSymbol})
	}
	sp.svc.mu.RUnlock()

	for _, e := range entries {
		assetCfg, ok := sp.svc.Asset(e.asset)
		if !ok {
			continue
		}
		adapter, ok := sp.svc.Adapter(assetCfg.Chain)
		if !ok {
			continue
		}
		balance, err := adapter.Balance(ctx, e.address, assetCfg)
		if err != nil {
			log.Printf("wallet: sweep balance %s: %v", e.address, err)
			continue
		}
		if balance < sp.threshold {
			continue
		}
		// 热钱包地址（account 0 index 0）作为归集目标
		hotAddr, _, err := sp.svc.hd.DeriveAddress(e.asset, 0, 0)
		if err != nil {
			log.Printf("wallet: sweep derive hot addr: %v", err)
			continue
		}
		_, hotKey, err := sp.svc.hd.DeriveAddress(e.asset, 0, 0)
		if err != nil {
			continue
		}
		rawHex, err := adapter.BuildSignedTx(ctx, hotKey, hotAddr, assetCfg, balance, 0)
		if err != nil {
			log.Printf("wallet: sweep sign %s: %v", e.address, err)
			continue
		}
		hash, err := adapter.Broadcast(ctx, rawHex)
		if err != nil {
			log.Printf("wallet: sweep broadcast %s: %v", e.address, err)
			continue
		}
		log.Printf("wallet: swept %d units %s from %s -> %s (tx %s)", balance, e.asset, e.address, hotAddr, hash)
	}
}

func splitAddrKey(key string) (userID, assetSymbol string) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
}
