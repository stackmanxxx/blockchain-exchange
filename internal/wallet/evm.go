package wallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// ERC-20 标准方法签名
var (
	// keccak256("Transfer(address,address,uint256)") 的 topic0
	transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	// transfer(address,uint256) selector
	transferSelector = common.Hex2Bytes("a9059cbb")
	// balanceOf(address) selector
	balanceOfSelector = common.Hex2Bytes("70a08231")
	// 默认 gas 限制（MVP 固定；生产应估算）
	defaultGasLimit = uint64(21000)
	erc20GasLimit   = uint64(100000)
)

// EVMAdapter EVM 系链适配器（ETH / BSC / Polygon 等，支持原生币与 ERC-20）
type EVMAdapter struct {
	chain   Chain
	client  *ethclient.Client
	rpc     *rpc.Client
	chainID *big.Int
}

// NewEVMAdapter 连接节点
func NewEVMAdapter(cfg ChainConfig) (*EVMAdapter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("wallet: dial %s rpc: %w", cfg.Chain, err)
	}
	chainID := big.NewInt(*cfg.ChainID)
	if cfg.ChainID == nil || *cfg.ChainID == 0 {
		chainID, err = client.ChainID(ctx)
		if err != nil {
			return nil, fmt.Errorf("wallet: fetch chain id: %w", err)
		}
	}
	return &EVMAdapter{
		chain:   cfg.Chain,
		client:  client,
		rpc:     client.Client(),
		chainID: chainID,
	}, nil
}

func (a *EVMAdapter) Chain() Chain { return a.chain }

func (a *EVMAdapter) LatestHeight(ctx context.Context) (uint64, error) {
	return a.client.BlockNumber(ctx)
}

// Block 获取区块：解析原生币转账与 ERC-20 Transfer 日志
func (a *EVMAdapter) Block(ctx context.Context, height uint64) (*Block, error) {
	block, err := a.client.BlockByNumber(ctx, new(big.Int).SetUint64(height))
	if err != nil {
		return nil, err
	}
	out := &Block{Height: height, Txs: make([]*Tx, 0)}
	txs := block.Transactions()
	if len(txs) == 0 {
		return out, nil
	}
	receipts := make(map[common.Hash]*types.Receipt)
	for _, tx := range txs {
		receipt, err := a.client.TransactionReceipt(ctx, tx.Hash())
		if err != nil {
			continue // 未找到回执（罕见）跳过
		}
		receipts[tx.Hash()] = receipt
		// 原生币转账
		if tx.To() != nil && tx.Value().Sign() > 0 {
			out.Txs = append(out.Txs, &Tx{
				Hash: tx.Hash().Hex(), From: fromAddr(tx), To: tx.To().Hex(),
				Asset: "NATIVE", AmountUnits: tx.Value().Int64(),
				Confirmations: 0, BlockHeight: height,
				Time: time.Unix(int64(block.Time()), 0),
			})
		}
		// ERC-20 转账日志
		if receipt != nil {
			for _, log := range receipt.Logs {
				if len(log.Topics) != 3 || log.Topics[0] != transferTopic {
					continue
				}
				from := common.BytesToAddress(log.Topics[1].Bytes())
				to := common.BytesToAddress(log.Topics[2].Bytes())
				amount := new(big.Int).SetBytes(log.Data)
				out.Txs = append(out.Txs, &Tx{
					Hash: log.TxHash.Hex(), From: from.Hex(), To: to.Hex(),
					Asset: "ERC20", AmountUnits: amount.Int64(),
					Confirmations: 0, BlockHeight: height, LogIndex: uint(log.Index),
					Time: time.Unix(int64(block.Time()), 0),
				})
			}
		}
	}
	return out, nil
}

func fromAddr(tx *types.Transaction) string {
	// from 不在 tx 内，需通过签名恢复（MVP 从回执拿不到 from，简化用 sender 恢复）
	signer := types.LatestSignerForChainID(tx.ChainId())
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return ""
	}
	return sender.Hex()
}

func (a *EVMAdapter) TxConfirmations(ctx context.Context, hash string) (uint64, error) {
	receipt, err := a.client.TransactionReceipt(ctx, common.HexToHash(hash))
	if err != nil {
		return 0, nil // 未确认
	}
	latest, err := a.client.BlockNumber(ctx)
	if err != nil {
		return 0, err
	}
	if receipt.BlockNumber == nil || latest < receipt.BlockNumber.Uint64() {
		return 0, nil
	}
	return latest - receipt.BlockNumber.Uint64() + 1, nil
}

// Broadcast 广播 raw hex（eth_sendRawTransaction）
func (a *EVMAdapter) Broadcast(ctx context.Context, rawHex string) (string, error) {
	var hash common.Hash
	err := a.rpc.CallContext(ctx, &hash, "eth_sendRawTransaction", rawHex)
	if err != nil {
		return "", err
	}
	return hash.Hex(), nil
}

// Balance 余额：原生币用 BalanceAt，ERC-20 用 eth_call balanceOf
func (a *EVMAdapter) Balance(ctx context.Context, address string, asset *Asset) (int64, error) {
	addr := common.HexToAddress(address)
	if asset.Contract == "" {
		bal, err := a.client.BalanceAt(ctx, addr, nil)
		if err != nil {
			return 0, err
		}
		return toUnits(bal, asset.Decimals), nil
	}
	contract := common.HexToAddress(asset.Contract)
	data := append(balanceOfSelector, common.LeftPadBytes(addr.Bytes(), 32)...)
	out, err := a.client.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
	if err != nil {
		return 0, err
	}
	bal := new(big.Int).SetBytes(out)
	return toUnits(bal, asset.Decimals), nil
}

// BuildSignedTx 构建并签名（原生币转账 / ERC-20 transfer）
func (a *EVMAdapter) BuildSignedTx(ctx context.Context, fromKey []byte, to string, asset *Asset, amountUnits int64, nonce uint64) (string, error) {
	priv, err := SignEVM(fromKey)
	if err != nil {
		return "", err
	}
	from := crypto.PubkeyToAddress(priv.PublicKey)
	toAddr := common.HexToAddress(to)

	// gas 估算与 nonce
	if nonce == 0 {
		nonce, err = a.client.PendingNonceAt(ctx, from)
		if err != nil {
			return "", err
		}
	}
	gasPrice, err := a.client.SuggestGasPrice(ctx)
	if err != nil {
		return "", err
	}

	var tx *types.Transaction
	if asset.Contract == "" {
		// 原生币
		gasLimit := defaultGasLimit
		amount := fromUnits(amountUnits, asset.Decimals)
		tx = types.NewTransaction(nonce, toAddr, amount, gasLimit, gasPrice, nil)
	} else {
		// ERC-20 transfer(address,uint256)
		amount := fromUnits(amountUnits, asset.Decimals)
		data := make([]byte, 0, 68)
		data = append(data, transferSelector...)
		data = append(data, common.LeftPadBytes(toAddr.Bytes(), 32)...)
		data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
		contract := common.HexToAddress(asset.Contract)
		tx = types.NewTransaction(nonce, contract, big.NewInt(0), erc20GasLimit, gasPrice, data)
	}

	signed, err := types.SignTx(tx, types.NewEIP155Signer(a.chainID), priv)
	if err != nil {
		return "", err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// ---- 单位换算（链上精度 ↔ 内部 1e8）----

// toUnits 链上 big.Int（10^decimals）→ 内部最小单位 int64（1e8）
//
//	decimals == 8：相等；decimals < 8：放大 ×10^(8-d)；decimals > 8：缩小 ÷10^(d-8)
func toUnits(amount *big.Int, decimals int) int64 {
	if decimals == 8 {
		return amount.Int64()
	}
	if decimals < 8 {
		exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(8-decimals)), nil)
		res := new(big.Int).Mul(amount, exp)
		if !res.IsInt64() {
			return 0
		}
		return res.Int64()
	}
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-8)), nil)
	div := new(big.Int).Div(amount, exp)
	if !div.IsInt64() {
		return 0
	}
	return div.Int64()
}

// fromUnits 内部最小单位 int64（1e8）→ 链上 big.Int（10^decimals）
func fromUnits(units int64, decimals int) *big.Int {
	if decimals == 8 {
		return big.NewInt(units)
	}
	if decimals < 8 {
		// 内部 1e8 → 链上 1e6：缩小 ×10^(d-8)
		exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(8-decimals)), nil)
		return new(big.Int).Div(big.NewInt(units), exp)
	}
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-8)), nil)
	return new(big.Int).Mul(big.NewInt(units), exp)
}

var _ = strings.TrimSpace
