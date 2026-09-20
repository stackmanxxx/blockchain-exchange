// 交易所后端入口（MVP 单进程版）
//
// 组装：认证 → 账本 → 撮合引擎（每交易对一实例）→ 订单/清算 → 行情聚合 → HTTP/WS。
// 生产环境按 docs/技术方案.md 拆分为微服务 + Kafka 事件总线。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"blockchain-exchange/internal/api"
	"blockchain-exchange/internal/asset"
	"blockchain-exchange/internal/auth"
	"blockchain-exchange/internal/domain"
	"blockchain-exchange/internal/engine"
	"blockchain-exchange/internal/market"
	"blockchain-exchange/internal/order"
	"blockchain-exchange/internal/wallet"
	"blockchain-exchange/internal/ws"
)

func main() {
	gin.SetMode(gin.ReleaseMode)

	// 1. 交易对配置（MVP 内置；生产由 admin 后台管理）
	symbols := domain.DefaultSymbols()

	// 2. WebSocket Hub + 行情聚合服务
	hub := ws.NewHub()
	mkt := market.NewService(hub, symbols)

	// 3. 撮合引擎：每交易对一个实例，行情服务作为成交/深度事件监听者
	engines := make(map[string]*engine.Engine)
	for _, s := range symbols {
		if !s.Enabled {
			continue
		}
		engines[s.Name] = engine.New(s.Name, mkt)
	}
	mkt.SetDepthProvider(func(symbol string, limit int) *domain.Depth {
		eng, ok := engines[symbol]
		if !ok {
			return nil
		}
		return eng.Depth(limit)
	})

	// 4. 资产账本 + 订单服务（含清算）
	assets := asset.New()
	orders := order.NewService(assets, engines, symbols)

	// 4.5 钱包服务（链上充提，Phase 2）
	walletSvc, scannerCancel := setupWallet(assets)

	// 5. 认证服务
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "dev-secret-change-me"
	}
	authSvc := auth.New(secret)

	// 6. HTTP + WS 路由
	handlers := api.NewHandlers(authSvc, assets, orders, mkt, walletSvc)
	router := api.NewRouter(handlers, hub)

	addr := os.Getenv("EXCHANGE_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: router}

	go func() {
		log.Printf("blockchain-exchange listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// 优雅退出
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")
	scannerCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("bye")
}

// setupWallet 组装钱包服务并启动充值扫描器。
// 演示模式（未配置 RPC 节点）自动注册内存 mock 链；生产必须配置节点 URL。
func setupWallet(assets *asset.Service) (*wallet.WalletService, context.CancelFunc) {
	mnemonic := os.Getenv("WALLET_MNEMONIC")
	if mnemonic == "" {
		var err error
		mnemonic, err = wallet.GenerateMnemonic()
		if err != nil {
			log.Fatalf("wallet: generate mnemonic: %v", err)
		}
		log.Printf("wallet: no WALLET_MNEMONIC set, generated demo mnemonic (SAVE IT FOR RESTART): %s", mnemonic)
	}

	assetsCfg := []*wallet.Asset{
		{Symbol: "ETH", Chain: wallet.ChainETH, Decimals: 18, Confirmations: 2,
			WithdrawEnabled: true, MinWithdrawUnits: 1_000_000, FeeUnits: 100_000_000_000_000 /*0.0001 ETH*/},
		{Symbol: "USDT_ERC20", Chain: wallet.ChainETH,
			Contract: "0xdac17f958d2ee523a2206206994597c13d831ec7", Decimals: 6, Confirmations: 2,
			WithdrawEnabled: true, MinWithdrawUnits: 1_000_000, FeeUnits: 0},
		{Symbol: "TRX", Chain: wallet.ChainTRON, Decimals: 6, Confirmations: 19,
			WithdrawEnabled: true, MinWithdrawUnits: 1_000_000, FeeUnits: 100},
		{Symbol: "USDT_TRC20", Chain: wallet.ChainTRON,
			Contract: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", Decimals: 6, Confirmations: 19,
			WithdrawEnabled: true, MinWithdrawUnits: 1_000_000, FeeUnits: 0},
		{Symbol: "BTC", Chain: wallet.ChainBTC, Decimals: 8, Confirmations: 2,
			WithdrawEnabled: true, MinWithdrawUnits: 1_000_000, FeeUnits: 100},
	}

	svc, err := wallet.New(wallet.Config{
		Mnemonic:     mnemonic,
		Assets:       assetsCfg,
		PollInterval: 5 * time.Second,
		StartHeight:  map[wallet.Chain]uint64{},
		OnDeposit: func(userID, assetSymbol string, amountUnits int64, txHash string) error {
			return assets.Deposit(userID, assetSymbol, amountUnits, "dep:"+txHash)
		},
	})
	if err != nil {
		log.Fatalf("wallet: init: %v", err)
	}

	// 注册链适配器：配置了 RPC 则连真实节点，否则用 mock（演示）
	registerChain := func(cfg wallet.ChainConfig) {
		switch cfg.Chain {
		case wallet.ChainETH, wallet.ChainBSC:
			adapter, err := wallet.NewEVMAdapter(cfg)
			if err != nil {
				log.Fatalf("wallet: evm adapter %s: %v", cfg.Chain, err)
			}
			svc.RegisterAdapter(adapter)
		case wallet.ChainBTC:
			adapter, err := wallet.NewBTCAdapter(cfg)
			if err != nil {
				log.Fatalf("wallet: btc adapter: %v", err)
			}
			svc.RegisterAdapter(adapter)
		case wallet.ChainTRON:
			adapter, err := wallet.NewTRONAdapter(cfg)
			if err != nil {
				log.Fatalf("wallet: tron adapter: %v", err)
			}
			svc.RegisterAdapter(adapter)
		}
	}

	ethURL := os.Getenv("ETH_RPC_URL")
	if ethURL != "" {
		chainID := int64(1)
		if v := os.Getenv("ETH_CHAIN_ID"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				chainID = n
			}
		}
		registerChain(wallet.ChainConfig{Chain: wallet.ChainETH, RPCURL: ethURL, ChainID: &chainID})
	} else {
		log.Printf("wallet: ETH_RPC_URL not set, using MOCK chain (demo mode)")
		svc.RegisterAdapter(wallet.NewMockAdapter(wallet.ChainETH))
	}
	if btcURL := os.Getenv("BTC_RPC_URL"); btcURL != "" {
		registerChain(wallet.ChainConfig{Chain: wallet.ChainBTC, RPCURL: btcURL,
			User: os.Getenv("BTC_RPC_USER"), Password: os.Getenv("BTC_RPC_PASS")})
	} else {
		log.Printf("wallet: BTC_RPC_URL not set, using MOCK chain (demo mode)")
		svc.RegisterAdapter(wallet.NewMockAdapter(wallet.ChainBTC))
	}
	if tronURL := os.Getenv("TRON_GRPC_URL"); tronURL != "" {
		registerChain(wallet.ChainConfig{Chain: wallet.ChainTRON, RPCURL: tronURL})
	} else {
		log.Printf("wallet: TRON_GRPC_URL not set, using MOCK chain (demo mode)")
		svc.RegisterAdapter(wallet.NewMockAdapter(wallet.ChainTRON))
	}

	// 启动充值扫描器
	ctx, cancel := context.WithCancel(context.Background())
	scanner := wallet.NewScanner(svc)
	go scanner.Run(ctx)
	// 提现确认跟踪（BROADCAST → SUCCESS）
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// MVP：扫描器内部已覆盖（提现确认在 API 查询时懒更新；此处留空）
			}
		}
	}()
	return svc, cancel
}
