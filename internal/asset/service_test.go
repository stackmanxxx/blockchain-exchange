package asset

import (
	"testing"

	"blockchain-exchange/internal/domain"
)

// 1 BTC = 100_000_000 units；100 USDT = 10_000_000_000 units

func TestDepositAndBalance(t *testing.T) {
	s := New()
	if err := s.Deposit("u1", "BTC", 100_000_000, "dep1"); err != nil {
		t.Fatal(err)
	}
	b := s.GetBalance("u1", "BTC")
	if b.Available != 100_000_000 || b.Frozen != 0 {
		t.Fatalf("balance wrong: %+v", b)
	}
}

func TestFreezeInsufficient(t *testing.T) {
	s := New()
	if err := s.Deposit("u1", "USDT", 1_000, "dep1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Freeze("u1", "USDT", 2_000, "o1"); err != domain.ErrInsufficientFunds {
		t.Fatalf("expect ErrInsufficientFunds, got %v", err)
	}
}

func TestFreezeUnfreeze(t *testing.T) {
	s := New()
	_ = s.Deposit("u1", "USDT", 10_000_000_000, "dep1")
	if err := s.Freeze("u1", "USDT", 4_000_000_000, "o1"); err != nil {
		t.Fatal(err)
	}
	b := s.GetBalance("u1", "USDT")
	if b.Available != 6_000_000_000 || b.Frozen != 4_000_000_000 {
		t.Fatalf("after freeze wrong: %+v", b)
	}
	if err := s.Unfreeze("u1", "USDT", 4_000_000_000, "o1"); err != nil {
		t.Fatal(err)
	}
	b = s.GetBalance("u1", "USDT")
	if b.Available != 10_000_000_000 || b.Frozen != 0 {
		t.Fatalf("after unfreeze wrong: %+v", b)
	}
}

// 完整成交清算：买家冻结 quote → 成交 → 买卖双方资产交换
func TestSettleTrade(t *testing.T) {
	s := New()
	sym := &domain.Symbol{BaseAsset: "BTC", QuoteAsset: "USDT"}

	// 买家 bob 有 1000 USDT，卖单 @100 冻结 1 BTC 值 100 USDT
	_ = s.Deposit("bob", "USDT", 100_000_000_000, "dep-b")
	_ = s.Freeze("bob", "USDT", 10_000_000_000, "O1") // 买 1 BTC @ 100
	// 卖家 alice 有 1 BTC，冻结 1 BTC
	_ = s.Deposit("alice", "BTC", 100_000_000, "dep-a")
	_ = s.Freeze("alice", "BTC", 100_000_000, "S1")

	trade := &domain.Trade{
		ID: "T1", Symbol: "BTCUSDT",
		PriceUnits: 10_000_000_000, QtyUnits: 100_000_000,
		MakerUserID: "alice", TakerUserID: "bob",
		TakerSide: domain.SideBuy, MakerSide: domain.SideSell,
	}
	if err := s.SettleTrade(trade, sym); err != nil {
		t.Fatal(err)
	}

	// bob：付 100 USDT 冻结，得到 1 BTC 可用
	bobBTC := s.GetBalance("bob", "BTC")
	bobUSDT := s.GetBalance("bob", "USDT")
	if bobBTC.Available != 100_000_000 {
		t.Fatalf("bob btc wrong: %+v", bobBTC)
	}
	if bobUSDT.Frozen != 0 || bobUSDT.Available != 90_000_000_000 {
		t.Fatalf("bob usdt wrong: %+v", bobUSDT)
	}
	// alice：付 1 BTC 冻结，得到 100 USDT 可用
	aliceBTC := s.GetBalance("alice", "BTC")
	aliceUSDT := s.GetBalance("alice", "USDT")
	if aliceBTC.Frozen != 0 || aliceBTC.Available != 0 {
		t.Fatalf("alice btc wrong: %+v", aliceBTC)
	}
	if aliceUSDT.Available != 10_000_000_000 {
		t.Fatalf("alice usdt wrong: %+v", aliceUSDT)
	}

	// 流水：双方各 4 条（充值 + 冻结 + 成交付出 + 成交收入），可审计
	entries := s.Ledger("bob", 0)
	if len(entries) != 4 {
		t.Fatalf("bob ledger entries: %d", len(entries))
	}
	entries = s.Ledger("alice", 0)
	if len(entries) != 4 {
		t.Fatalf("alice ledger entries: %d", len(entries))
	}
}
