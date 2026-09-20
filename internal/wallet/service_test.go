package wallet

import (
	"context"
	"testing"
	"time"

	"blockchain-exchange/internal/asset"
)

// 测试助记词（固定，保证确定性）
const testMnemonic = "test test test test test test test test test test test junk"

func testAssets() []*Asset {
	return []*Asset{
		{Symbol: "ETH", Chain: ChainETH, Decimals: 18, Confirmations: 2, WithdrawEnabled: true, MinWithdrawUnits: 1_000_000, FeeUnits: 100},
		{Symbol: "USDT_ERC20", Chain: ChainETH, Contract: "0xdac17f958d2ee523a2206206994597c13d831ec7", Decimals: 6, Confirmations: 2, WithdrawEnabled: true, MinWithdrawUnits: 1_000_000, FeeUnits: 0},
	}
}

func newTestService(t *testing.T) (*WalletService, *asset.Service, *MockAdapter) {
	t.Helper()
	mock := NewMockAdapter(ChainETH)
	assets := asset.New()
	svc, err := New(Config{
		Mnemonic:     testMnemonic,
		Assets:       testAssets(),
		PollInterval: 50 * time.Millisecond,
		StartHeight:  map[Chain]uint64{ChainETH: 100},
		OnDeposit: func(userID, assetSymbol string, amountUnits int64, txHash string) error {
			return assets.Deposit(userID, assetSymbol, amountUnits, "dep:"+txHash)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.RegisterAdapter(mock)
	return svc, assets, mock
}

// HD 地址派生确定性
func TestDeriveAddressDeterministic(t *testing.T) {
	svc, _, _ := newTestService(t)
	a1, _, err := svc.hd.DeriveAddress("ETH", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	a2, _, _ := svc.hd.DeriveAddress("ETH", 0, 1)
	if a1 != a2 {
		t.Fatalf("address not deterministic: %s vs %s", a1, a2)
	}
	if len(a1) != 42 || a1[:2] != "0x" {
		t.Fatalf("invalid eth address: %s", a1)
	}
	// 不同 index 不同地址
	a3, _, _ := svc.hd.DeriveAddress("ETH", 0, 2)
	if a1 == a3 {
		t.Fatal("different index should differ")
	}
}

// 充值地址分配 + 地址反查
func TestDepositAddressAndMatch(t *testing.T) {
	svc, _, _ := newTestService(t)
	addr, err := svc.GetDepositAddress("U1", "ETH")
	if err != nil {
		t.Fatal(err)
	}
	// 幂等：再次获取同地址
	addr2, _ := svc.GetDepositAddress("U1", "ETH")
	if addr != addr2 {
		t.Fatalf("address should be stable: %s vs %s", addr, addr2)
	}
	user, assetSym, ok := svc.MatchUser(addr)
	if !ok || user != "U1" || assetSym != "ETH" {
		t.Fatalf("match failed: %s %s %v", user, assetSym, ok)
	}
}

// 充值扫描：确认数达标 → 幂等入账
func TestScannerCreditDeposit(t *testing.T) {
	svc, assets, mock := newTestService(t)
	addr, _ := svc.GetDepositAddress("U1", "ETH")
	_ = addr

	// 模拟两笔充值（同一用户同一地址）：1 ETH 和 0.5 ETH（链上 1e18）
	mock.AddBlock(101, &Tx{Hash: "0xtx1", To: addr, Asset: "NATIVE", AmountUnits: 1_000_000_000_000_000_000, BlockHeight: 101})
	mock.AddBlock(102, &Tx{Hash: "0xtx2", To: addr, Asset: "NATIVE", AmountUnits: 500_000_000_000_000_000, BlockHeight: 102})
	mock.SetConf("0xtx1", 2)
	mock.SetConf("0xtx2", 5)

	scanner := NewScanner(svc)
	scanner.chainFrom[ChainETH] = 100
	scanner.scanOnce(context.Background())

	// 1.5 ETH = 150_000_000 内部单位（1e8 精度）
	bal := assets.GetBalance("U1", "ETH")
	if bal.Available != 150_000_000 {
		t.Fatalf("deposit balance wrong: %+v", bal)
	}

	// 幂等：再次扫描不重复入账
	scanner.scanOnce(context.Background())
	bal = assets.GetBalance("U1", "ETH")
	if bal.Available != 150_000_000 {
		t.Fatalf("duplicate credit! balance: %+v", bal)
	}

	// 确认数不足不入账
	addr2, _ := svc.GetDepositAddress("U2", "ETH")
	mock.AddBlock(103, &Tx{Hash: "0xtx3", To: addr2, Asset: "NATIVE", AmountUnits: 1_000_000_000_000_000_000, BlockHeight: 103})
	mock.SetConf("0xtx3", 0) // 未确认
	scanner.scanOnce(context.Background())
	if b := assets.GetBalance("U2", "ETH"); b.Available != 0 {
		t.Fatalf("unconfirmed deposit should not credit: %+v", b)
	}
}

// 提现流程：申请冻结 → 审批签名广播 → 扣账；驳回解冻
func TestWithdrawFlow(t *testing.T) {
	svc, assets, mock := newTestService(t)
	// U1 充值 5 ETH（5e18 wei = 500_000_000 内部单位）
	addr, _ := svc.GetDepositAddress("U1", "ETH")
	mock.AddBlock(101, &Tx{Hash: "0xdep1", To: addr, Asset: "NATIVE", AmountUnits: 5_000_000_000_000_000_000, BlockHeight: 101})
	mock.SetConf("0xdep1", 3)
	NewScanner(svc).scanOnce(context.Background())

	if b := assets.GetBalance("U1", "ETH"); b.Available != 500_000_000 {
		t.Fatalf("deposit failed: %+v", b)
	}

	// 提现 2 ETH（200_000_000）+ 手续费（fee=100 units 简化）
	w, err := svc.SubmitWithdrawal(assets, "U1", "ETH", "0xRecipientAddress000000000000000000000000000", 200_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != WithdrawPending {
		t.Fatalf("status should be PENDING, got %s", w.Status)
	}
	// 冻结：2 ETH + fee = 200_000_100；可用 = 5 ETH - 2 ETH - fee
	if b := assets.GetBalance("U1", "ETH"); b.Available != 299_999_900 || b.Frozen != 200_000_100 {
		t.Fatalf("freeze wrong: %+v", b)
	}

	// 审批 → 签名广播 → 冻结扣减
	w, err = svc.ApproveWithdrawal(assets, w.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != WithdrawBroadcast || w.TxHash == "" {
		t.Fatalf("after approve: %+v", w)
	}
	if len(mock.Broadcasted()) != 1 {
		t.Fatalf("should broadcast once")
	}
	b := assets.GetBalance("U1", "ETH")
	if b.Frozen != 0 || b.Available != 299_999_900 {
		t.Fatalf("after consume frozen: %+v", b)
	}

	// 确认跟踪 → SUCCESS
	mock.SetConf(w.TxHash, 3)
	w, err = svc.ConfirmWithdrawal(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != WithdrawSuccess {
		t.Fatalf("should be SUCCESS, got %s", w.Status)
	}

	// 驳回流程：U1 再提 1 ETH 然后驳回 → 解冻
	w2, _ := svc.SubmitWithdrawal(assets, "U1", "ETH", "0xAnother000000000000000000000000000000000", 100_000_000)
	if b := assets.GetBalance("U1", "ETH"); b.Frozen != 100_000_100 {
		t.Fatalf("second freeze wrong: %+v", b)
	}
	w2, err = svc.RejectWithdrawal(assets, w2.ID, "risk", "blacklist")
	if err != nil {
		t.Fatal(err)
	}
	if w2.Status != WithdrawRejected {
		t.Fatalf("should be REJECTED, got %s", w2.Status)
	}
	b = assets.GetBalance("U1", "ETH")
	if b.Available != 299_999_900 || b.Frozen != 0 {
		t.Fatalf("after reject unfreeze: %+v", b)
	}
}

// ERC-20 精度换算
func TestUnitsConversion(t *testing.T) {
	// 1 USDT（6 位小数）= 1000000 链上单位 = 100000000 内部单位
	if got := toUnitsInternal(1_000_000, 6); got != 100_000_000 {
		t.Fatalf("usdt 1 unit = %d, want 100000000", got)
	}
	// 1 ETH（18 位）= 1e18 链上 = 1e8 内部
	if got := toUnitsInternal(1_000_000_000_000_000_000, 18); got != 100_000_000 {
		t.Fatalf("eth 1 = %d, want 100000000", got)
	}
	// BTC（8 位）直接相等
	if got := toUnitsInternal(100_000_000, 8); got != 100_000_000 {
		t.Fatalf("btc 1 = %d", got)
	}
}
