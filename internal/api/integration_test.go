package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"blockchain-exchange/internal/asset"
	"blockchain-exchange/internal/auth"
	"blockchain-exchange/internal/domain"
	"blockchain-exchange/internal/engine"
	"blockchain-exchange/internal/market"
	"blockchain-exchange/internal/order"
	"blockchain-exchange/internal/ws"
)

// setup 组装全部服务（与 cmd/exchange/main.go 一致）
func setup() (*gin.Engine, *asset.Service, *auth.Service, *order.Service) {
	gin.SetMode(gin.ReleaseMode)
	symbols := domain.DefaultSymbols()
	hub := ws.NewHub()
	mkt := market.NewService(hub, symbols)
	engines := make(map[string]*engine.Engine)
	for _, s := range symbols {
		if s.Enabled {
			engines[s.Name] = engine.New(s.Name, mkt)
		}
	}
	mkt.SetDepthProvider(func(symbol string, limit int) *domain.Depth {
		if eng, ok := engines[symbol]; ok {
			return eng.Depth(limit)
		}
		return nil
	})
	assets := asset.New()
	orders := order.NewService(assets, engines, symbols)
	authSvc := auth.New("test-secret")
	h := NewHandlers(authSvc, assets, orders, mkt, nil)
	return NewRouter(h, hub), assets, authSvc, orders
}

func doJSON(t *testing.T, r *gin.Engine, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func mustLogin(t *testing.T, r *gin.Engine, email, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	code, out := doJSON(t, r, http.MethodPost, "/api/v1/auth/login", "", string(body))
	if code != http.StatusOK {
		t.Fatalf("login %s failed: %d %v", email, code, out)
	}
	return out["data"].(map[string]any)["token"].(string)
}

func mustRegister(t *testing.T, r *gin.Engine, email, password string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	code, out := doJSON(t, r, http.MethodPost, "/api/v1/auth/register", "", string(body))
	if code != http.StatusOK {
		t.Fatalf("register %s failed: %d %v", email, code, out)
	}
}

// 完整链路：注册 → 充值 → 挂卖单 → 吃买单 → 校验撮合结果与清算
func TestEndToEndTradeFlow(t *testing.T) {
	r, assets, _, _ := setup()

	// 注册 alice（卖家）/ bob（买家）
	mustRegister(t, r, "alice@test.com", "password123")
	mustRegister(t, r, "bob@test.com", "password123")
	aliceToken := mustLogin(t, r, "alice@test.com", "password123")
	bobToken := mustLogin(t, r, "bob@test.com", "password123")

	// 从 auth 服务取 userID（直接走注册对象；测试用简单方式）
	// 这里通过 balances 接口返回空校验 token 有效
	if code, _ := doJSON(t, r, http.MethodGet, "/api/v1/accounts/balances", aliceToken, ""); code != http.StatusOK {
		t.Fatalf("balances should be accessible with token, got %d", code)
	}

	// 充值：alice 1 BTC（卖家），bob 100000 USDT（买家）
	// userID 通过注册顺序推断：U00000001 / U00000002（见 auth.Service.Register 实现）
	aliceID := "U00000001"
	bobID := "U00000002"
	if err := assets.Deposit(aliceID, "BTC", 100_000_000, "dep-test-alice"); err != nil { // 1 BTC
		t.Fatal(err)
	}
	if err := assets.Deposit(bobID, "USDT", 10_000_000_000_000, "dep-test-bob"); err != nil { // 100000 USDT
		t.Fatal(err)
	}

	// alice 挂卖单：1 BTC @ 60000 USDT
	code, out := doJSON(t, r, http.MethodPost, "/api/v1/orders", aliceToken,
		`{"symbol":"BTCUSDT","side":"SELL","type":"LIMIT","price":"60000","quantity":"1"}`)
	if code != http.StatusOK {
		t.Fatalf("place sell order failed: %d %v", code, out)
	}

	// bob 市价买单：50000 USDT（应吃满 1 BTC @60000 花 60000？不足 → 买 0.8333 BTC）
	// 改为限价买 1 BTC @ 60000 以全成交
	code, out = doJSON(t, r, http.MethodPost, "/api/v1/orders", bobToken,
		`{"symbol":"BTCUSDT","side":"BUY","type":"LIMIT","price":"60000","quantity":"1"}`)
	if code != http.StatusOK {
		t.Fatalf("place buy order failed: %d %v", code, out)
	}
	trades := out["data"].(map[string]any)["trades"].([]any)
	if len(trades) != 1 {
		t.Fatalf("expect 1 trade, got %d", len(trades))
	}

	// 余额校验：
	// alice：BTC 冻结 0、可用 0（卖出 1 BTC）；USDT 收到 60000
	// bob：BTC 收到 1；USDT 冻结 0、可用 40000（100000 - 60000）
	code, out = doJSON(t, r, http.MethodGet, "/api/v1/accounts/balances", aliceToken, "")
	if code != http.StatusOK {
		t.Fatal("alice balances failed")
	}
	aliceBal := out["data"].(map[string]any)
	if aliceBal["BTC"].(map[string]any)["available"].(float64) != 0 {
		t.Fatalf("alice BTC available should be 0, got %v", aliceBal["BTC"])
	}
	if aliceBal["USDT"].(map[string]any)["available"].(float64) != 6000000000000 {
		t.Fatalf("alice USDT available should be 60000, got %v", aliceBal["USDT"])
	}

	code, out = doJSON(t, r, http.MethodGet, "/api/v1/accounts/balances", bobToken, "")
	bobBal := out["data"].(map[string]any)
	if bobBal["BTC"].(map[string]any)["available"].(float64) != 100_000_000 {
		t.Fatalf("bob BTC available should be 1, got %v", bobBal["BTC"])
	}
	if bobBal["USDT"].(map[string]any)["available"].(float64) != 4_000_000_000_000 {
		t.Fatalf("bob USDT available should be 40000, got %v", bobBal["USDT"])
	}

	// 行情校验：ticker 应有最新价 60000，K线/深度可用
	code, out = doJSON(t, r, http.MethodGet, "/api/v1/market/ticker/BTCUSDT", "", "")
	if code != http.StatusOK {
		t.Fatalf("ticker failed: %d %v", code, out)
	}
	tk := out["data"].(map[string]any)
	if tk["lastPrice"] != "60000" {
		t.Fatalf("ticker lastPrice = %v, want 60000", tk["lastPrice"])
	}

	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/market/klines/BTCUSDT?interval=1m", "", "")
	if code != http.StatusOK {
		t.Fatalf("klines failed: %d", code)
	}
	code, _ = doJSON(t, r, http.MethodGet, "/api/v1/market/depth/BTCUSDT?limit=5", "", "")
	if code != http.StatusOK {
		t.Fatalf("depth failed: %d", code)
	}
}

// 余额不足拒单
func TestInsufficientBalanceRejected(t *testing.T) {
	r, _, _, _ := setup()
	mustRegister(t, r, "carol@test.com", "password123")
	token := mustLogin(t, r, "carol@test.com", "password123")

	code, out := doJSON(t, r, http.MethodPost, "/api/v1/orders", token,
		`{"symbol":"BTCUSDT","side":"BUY","type":"LIMIT","price":"60000","quantity":"1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("expect 400 insufficient funds, got %d %v", code, out)
	}
}
