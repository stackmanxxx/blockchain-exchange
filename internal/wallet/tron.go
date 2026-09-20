package wallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// TRONAdapter TRON 链适配器（TRX 原生币 + TRC-20），自研 HTTP wallet API 实现：
//   - 扫块：/wallet/getnowblock、/wallet/getblockbynum
//   - 确认数：/wallet/gettransactioninfobyid
//   - 交易构造：/wallet/createtransaction（TRX）、/wallet/triggersmartcontract（TRC-20）
//   - 签名：本地 sha256(raw_data) + secp256k1（与 ETH 同一套密钥）
//   - 广播：/wallet/broadcasttransaction（JSON 交易）
//
// 无需任何第三方 TRON SDK，直接对接 TronGrid（https://api.trongrid.io）或自建全节点。
type TRONAdapter struct {
	url   string // HTTP 端点，如 https://api.trongrid.io
	httpc *http.Client
}

func NewTRONAdapter(cfg ChainConfig) (*TRONAdapter, error) {
	return &TRONAdapter{
		url:   cfg.RPCURL,
		httpc: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (a *TRONAdapter) Chain() Chain { return ChainTRON }

// ---- wallet HTTP API ----

func (a *TRONAdapter) post(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("wallet: tron %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (a *TRONAdapter) postRaw(ctx context.Context, path string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wallet: tron %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ---- ChainAdapter ----

func (a *TRONAdapter) LatestHeight(ctx context.Context) (uint64, error) {
	var out struct {
		BlockHeader struct {
			RawData struct {
				Number int64 `json:"number"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := a.post(ctx, "/wallet/getnowblock", map[string]any{}, &out); err != nil {
		return 0, err
	}
	return uint64(out.BlockHeader.RawData.Number), nil
}

// Block 获取区块并解析 TRX 转账与 TRC-20 Transfer
func (a *TRONAdapter) Block(ctx context.Context, height uint64) (*Block, error) {
	raw, err := a.postRaw(ctx, "/wallet/getblockbynum", map[string]any{"num": height})
	if err != nil {
		return nil, err
	}
	var block struct {
		BlockHeader struct {
			RawData struct {
				Number    int64 `json:"number"`
				Timestamp int64 `json:"timestamp"`
			} `json:"raw_data"`
		} `json:"block_header"`
		Transactions []struct {
			TxID    string `json:"txID"`
			RawData struct {
				Contract []struct {
					Type      string `json:"type"`
					Parameter struct {
						Value struct {
							OwnerAddress string `json:"owner_address"`
							ToAddress    string `json:"to_address"`
							Amount       int64  `json:"amount"`
							Data         string `json:"data"`
						} `json:"value"`
					} `json:"parameter"`
				} `json:"contract"`
			} `json:"raw_data"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, err
	}
	out := &Block{Height: height, Txs: make([]*Tx, 0)}
	t := time.UnixMilli(block.BlockHeader.RawData.Timestamp)
	for _, tx := range block.Transactions {
		if len(tx.RawData.Contract) == 0 {
			continue
		}
		c := tx.RawData.Contract[0]
		switch c.Type {
		case "TransferContract":
			out.Txs = append(out.Txs, &Tx{
				Hash:        tx.TxID,
				From:        tronHexToBase58(c.Parameter.Value.OwnerAddress),
				To:          tronHexToBase58(c.Parameter.Value.ToAddress),
				Asset:       "TRX",
				AmountUnits: c.Parameter.Value.Amount,
				BlockHeight: height,
				Time:        t,
			})
		case "TriggerSmartContract":
			to20, amount := parseTrc20Data(c.Parameter.Value.Data)
			if to20 == nil {
				continue
			}
			out.Txs = append(out.Txs, &Tx{
				Hash:        tx.TxID,
				From:        tronHexToBase58(c.Parameter.Value.OwnerAddress),
				To:          tronHexToBase58(hex.EncodeToString(append([]byte{0x41}, to20...))),
				Asset:       "TRC20",
				AmountUnits: amount.Int64(),
				BlockHeight: height,
				Time:        t,
			})
		}
	}
	return out, nil
}

// parseTrc20Data 解析 triggerSmartContract 的 data：
//
//	selector(4) + to(32，左填充20字节) + amount(32)
func parseTrc20Data(data string) (to20 []byte, amount *big.Int) {
	raw, err := hex.DecodeString(data)
	if err != nil || len(raw) < 4+32+32 {
		return nil, nil
	}
	// 校验 selector：transfer(address,uint256)
	if !bytes.Equal(raw[:4], []byte{0xa9, 0x05, 0x9c, 0xbb}) {
		return nil, nil
	}
	to20 = raw[16:36]
	amount = new(big.Int).SetBytes(raw[36:68])
	return to20, amount
}

func (a *TRONAdapter) TxConfirmations(ctx context.Context, hash string) (uint64, error) {
	var out struct {
		BlockNumber int64 `json:"blockNumber"`
	}
	if err := a.post(ctx, "/wallet/gettransactioninfobyid", map[string]any{"value": hash}, &out); err != nil {
		return 0, nil
	}
	if out.BlockNumber <= 0 {
		return 0, nil
	}
	latest, err := a.LatestHeight(ctx)
	if err != nil {
		return 0, err
	}
	return latest - uint64(out.BlockNumber) + 1, nil
}

// Broadcast 广播签名后的交易（rawHex = 签名交易 JSON 的 hex 编码）
func (a *TRONAdapter) Broadcast(ctx context.Context, rawHex string) (string, error) {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", err
	}
	var tx map[string]any
	if err := json.Unmarshal(raw, &tx); err != nil {
		return "", fmt.Errorf("wallet: tron tx json: %w", err)
	}
	var out struct {
		Result  bool   `json:"result"`
		Txid    string `json:"txid"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := a.post(ctx, "/wallet/broadcasttransaction", tx, &out); err != nil {
		return "", err
	}
	if !out.Result {
		return "", fmt.Errorf("wallet: tron broadcast rejected: %s %s", out.Code, out.Message)
	}
	return out.Txid, nil
}

// Balance 余额：TRX 用 getaccount；TRC-20 用 triggerconstantcontract 调 balanceOf
func (a *TRONAdapter) Balance(ctx context.Context, addressStr string, asset *Asset) (int64, error) {
	addrHex := tronBase58ToHex(addressStr)
	if asset.Contract == "" {
		var out struct {
			Balance int64 `json:"balance"`
		}
		if err := a.post(ctx, "/wallet/getaccount", map[string]any{"address": addrHex}, &out); err != nil {
			return 0, err
		}
		return out.Balance, nil
	}
	// balanceOf(address)：parameter = 32 字节左填充地址
	param := hex.EncodeToString(leftPad20(addrHex))
	var out struct {
		ConstantResult []string `json:"constant_result"`
	}
	if err := a.post(ctx, "/wallet/triggerconstantcontract", map[string]any{
		"owner_address":     addrHex,
		"contract_address":  tronBase58ToHex(asset.Contract),
		"function_selector": "balanceOf(address)",
		"parameter":         param,
		"visible":           false,
	}, &out); err != nil {
		return 0, err
	}
	if len(out.ConstantResult) == 0 {
		return 0, fmt.Errorf("wallet: tron balanceOf empty result")
	}
	balBytes, err := hex.DecodeString(out.ConstantResult[0])
	if err != nil {
		return 0, err
	}
	return toUnits(new(big.Int).SetBytes(balBytes), asset.Decimals), nil
}

// BuildSignedTx 构造并签名交易：
//   - TRX：/wallet/createtransaction
//   - TRC-20：/wallet/triggersmartcontract
//
// 本地签名（sha256 + secp256k1，与 ETH 同一私钥），返回签名交易 JSON 的 hex
func (a *TRONAdapter) BuildSignedTx(ctx context.Context, fromKey []byte, to string, asset *Asset, amountUnits int64, nonce uint64) (string, error) {
	priv, err := SignEVM(fromKey)
	if err != nil {
		return "", err
	}
	fromHex := crypto.PubkeyToAddress(priv.PublicKey).Hex()[2:] // 20 字节
	fromAddrHex := "41" + fromHex
	toAddrHex := tronBase58ToHex(to)

	var tx map[string]any
	if asset.Contract == "" {
		// TRX：内部 1e8 → sun（decimals=6 → ×10^(6-8) = ÷100）
		sun := toUnitsInternal(amountUnits, asset.Decimals)
		var out map[string]any
		if err := a.post(ctx, "/wallet/createtransaction", map[string]any{
			"to_address":    toAddrHex,
			"owner_address": fromAddrHex,
			"amount":        sun,
			"visible":       false,
		}, &out); err != nil {
			return "", fmt.Errorf("wallet: tron createtransaction: %w", err)
		}
		tx = out
	} else {
		// TRC-20 transfer(address,uint256)
		tokenAmount := fromUnits(amountUnits, asset.Decimals)
		param := hex.EncodeToString(append(leftPad20(toAddrHex), leftPad32(tokenAmount.Bytes())...))
		var out map[string]any
		if err := a.post(ctx, "/wallet/triggersmartcontract", map[string]any{
			"owner_address":     fromAddrHex,
			"contract_address":  tronBase58ToHex(asset.Contract),
			"function_selector": "transfer(address,uint256)",
			"parameter":         param,
			"fee_limit":         50_000_000, // 500 TRX 能量上限（MVP 固定；生产按估算）
			"visible":           false,
		}, &out); err != nil {
			return "", fmt.Errorf("wallet: tron triggersmartcontract: %w", err)
		}
		tx = out
	}

	// 签名：sha256(raw_data_hex) + secp256k1 → 65 字节 r+s+v
	rawDataHex, ok := tx["raw_data_hex"].(string)
	if !ok {
		return "", fmt.Errorf("wallet: tron tx missing raw_data_hex")
	}
	rawData, err := hex.DecodeString(strip0x(rawDataHex))
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(rawData)
	sig, err := crypto.Sign(hash[:], priv)
	if err != nil {
		return "", fmt.Errorf("wallet: tron sign: %w", err)
	}
	tx["signature"] = []string{"0x" + hex.EncodeToString(sig)}

	// 返回签名交易 JSON 的 hex（Broadcast 反序列化后走 broadcasttransaction）
	raw, err := json.Marshal(tx)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// ---- TRON 地址工具 ----

// tronBase58ToHex base58 地址（T...）→ hex 地址（41...）
func tronBase58ToHex(addr string) string {
	payload, err := b58CheckDecode(addr)
	if err != nil || len(payload) != 21 {
		return ""
	}
	return hex.EncodeToString(payload)
}

// tronHexToBase58 hex 地址（41...）→ base58 地址（T...）
func tronHexToBase58(addrHex string) string {
	payload, err := hex.DecodeString(strip0x(addrHex))
	if err != nil || len(payload) != 21 {
		return ""
	}
	return b58CheckEncode(payload)
}

// leftPad20 hex 字符串（41 + 20字节）→ 32 字节左填充（合约地址参数用）
func leftPad20(addrHex string) []byte {
	b, err := hex.DecodeString(strip0x(addrHex))
	if err != nil || len(b) < 21 {
		return make([]byte, 32)
	}
	out := make([]byte, 32)
	copy(out[12:], b[1:21]) // 去掉 0x41 前缀，取 20 字节
	return out
}

func leftPad32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

func strip0x(s string) string {
	if len(s) >= 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}

var _ = strconv.Itoa
