package wallet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// btcRPC Bitcoin Core JSON-RPC 客户端（轻量自研，依赖 Bitcoin Core 节点）
type btcRPC struct {
	url    string
	user   string
	pass   string
	httpc  *http.Client
	nextID atomic.Int64
}

func newBTCRPC(url, user, pass string) *btcRPC {
	return &btcRPC{
		url: url, user: user, pass: pass,
		httpc: &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *btcRPC) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "1.0", "id": r.nextID.Add(1),
		"method": method, "params": params,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(r.user+":"+r.pass)))
	resp, err := r.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("btc rpc %s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

// BTCAdapter BTC 链适配器。
// 依赖 Bitcoin Core（含 wallet）节点：扫块解析 vout 地址、确认数、余额、广播。
// 签名流程（UTXO 选择 + 私钥签名）依赖节点 wallet 的 fund/sign 命令，
// 生产环境应替换为自维护 UTXO + HSM 签名（MVP 演示架构）。
type BTCAdapter struct {
	rpc *btcRPC
}

func NewBTCAdapter(cfg ChainConfig) (*BTCAdapter, error) {
	return &BTCAdapter{rpc: newBTCRPC(cfg.RPCURL, cfg.User, cfg.Password)}, nil
}

func (a *BTCAdapter) Chain() Chain { return ChainBTC }

func (a *BTCAdapter) LatestHeight(ctx context.Context) (uint64, error) {
	raw, err := a.rpc.call(ctx, "getblockcount")
	if err != nil {
		return 0, err
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return uint64(n), nil
}

// Block 获取区块并解析 vout 目标地址（充值/归集匹配）
func (a *BTCAdapter) Block(ctx context.Context, height uint64) (*Block, error) {
	rawHash, err := a.rpc.call(ctx, "getblockhash", height)
	if err != nil {
		return nil, err
	}
	var hash string
	if err := json.Unmarshal(rawHash, &hash); err != nil {
		return nil, err
	}
	rawBlock, err := a.rpc.call(ctx, "getblock", hash, 2)
	if err != nil {
		return nil, err
	}
	var block struct {
		Height uint64 `json:"height"`
		Time   int64  `json:"time"`
		Tx     []struct {
			Txid string `json:"txid"`
			Vout []struct {
				ScriptPubKey struct {
					Addresses []string `json:"addresses"`
				} `json:"scriptPubKey"`
			} `json:"vout"`
		} `json:"tx"`
	}
	if err := json.Unmarshal(rawBlock, &block); err != nil {
		return nil, err
	}
	out := &Block{Height: height, Txs: make([]*Tx, 0)}
	for _, tx := range block.Tx {
		// MVP：只解析 coinbase 之后交易的 vout 地址（简化，不区分找零）
		for _, vout := range tx.Vout {
			for _, addr := range vout.ScriptPubKey.Addresses {
				if addr == "" {
					continue
				}
				out.Txs = append(out.Txs, &Tx{
					Hash: tx.Txid, To: addr, Asset: "BTC",
					BlockHeight: height,
					Time:        time.Unix(block.Time, 0),
					// 金额需 getrawtransaction 获取；MVP 置 0，扫描器不依赖本字段入账（见 scanner）
				})
			}
		}
	}
	return out, nil
}

func (a *BTCAdapter) TxConfirmations(ctx context.Context, hash string) (uint64, error) {
	raw, err := a.rpc.call(ctx, "gettransaction", hash)
	if err != nil {
		return 0, nil // 未确认
	}
	var t struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return 0, nil
	}
	if t.Confirmations < 0 {
		return 0, nil
	}
	return uint64(t.Confirmations), nil
}

func (a *BTCAdapter) Broadcast(ctx context.Context, rawHex string) (string, error) {
	raw, err := a.rpc.call(ctx, "sendrawtransaction", rawHex)
	if err != nil {
		return "", err
	}
	var hash string
	if err := json.Unmarshal(raw, &hash); err != nil {
		return "", err
	}
	return hash, nil
}

func (a *BTCAdapter) Balance(ctx context.Context, address string, asset *Asset) (int64, error) {
	raw, err := a.rpc.call(ctx, "getreceivedbyaddress", address, 0)
	if err != nil {
		return 0, err
	}
	var btc float64
	if err := json.Unmarshal(raw, &btc); err != nil {
		return 0, err
	}
	// BTC → 最小单位（1e8）
	units := int64(btc * 1e8)
	if units < 0 {
		units = 0
	}
	return units, nil
}

// BuildSignedTx BTC 签名：依赖节点钱包 fundrawtransaction + signrawtransactionwithwallet。
// 返回 raw hex 直接调用 sendrawtransaction 即可广播。
func (a *BTCAdapter) BuildSignedTx(ctx context.Context, fromKey []byte, to string, asset *Asset, amountUnits int64, nonce uint64) (string, error) {
	// MVP 演示：构造仅含输出的裸交易，交由节点钱包补足输入并签名
	amountBTC := float64(amountUnits) / 1e8
	_ = amountBTC
	raw, err := a.rpc.call(ctx, "createrawtransaction", []any{}, []map[string]any{{to: amountBTC}})
	if err != nil {
		return "", err
	}
	var hexStr string
	if err := json.Unmarshal(raw, &hexStr); err != nil {
		return "", err
	}
	// 节点钱包签名
	funded, err := a.rpc.call(ctx, "fundrawtransaction", hexStr)
	if err != nil {
		return "", fmt.Errorf("btc fundrawtransaction: %w", err)
	}
	var f struct {
		Hex string `json:"hex"`
	}
	if err := json.Unmarshal(funded, &f); err != nil {
		return "", err
	}
	signed, err := a.rpc.call(ctx, "signrawtransactionwithwallet", f.Hex)
	if err != nil {
		return "", fmt.Errorf("btc sign: %w", err)
	}
	var s struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal(signed, &s); err != nil {
		return "", err
	}
	if !s.Complete {
		return "", fmt.Errorf("btc sign incomplete")
	}
	return s.Hex, nil
}

var _ = strconv.Itoa
