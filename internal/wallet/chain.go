// Package wallet 钱包服务：链上充提（Phase 2）。
//
// 职责：
//   - 多链适配器抽象（ChainAdapter）：EVM（ETH/BSC/...含 ERC-20）、BTC、TRON
//   - HD 钱包（BIP39/BIP44）派生用户充值地址
//   - 充值扫描：扫块 → 确认数达标 → 幂等入账（对接资产账本）
//   - 提现：申请 → 审批 → 热钱包签名 → 广播 → 确认跟踪 → 扣账
//   - 归集（Sweep）：散落在充值地址的资产定期归集到热/冷钱包
//
// 安全约定：热钱包私钥仅存在于内存（MVP），生产环境接入 KMS/HSM；
// 冷钱包离线签名（人工流程），本服务不持有冷钱包私钥。
package wallet

import (
	"context"
	"time"
)

// Chain 链类型
type Chain string

const (
	ChainETH     Chain = "ETH"
	ChainBTC     Chain = "BTC"
	ChainTRON    Chain = "TRON"
	ChainBSC     Chain = "BSC"
	ChainPolygon Chain = "POLYGON"
)

// Asset 链上资产配置（一种链上资产 = 一条链上一个币种/合约）
type Asset struct {
	Symbol           string `json:"symbol"`        // BTC / ETH / USDT
	Chain            Chain  `json:"chain"`         // 所属链
	Contract         string `json:"contract"`      // ERC-20/TRC-20 合约地址；原生币为空
	Decimals         int    `json:"decimals"`      // 链上精度（BTC=8, ETH=18, USDT=6）
	Confirmations    uint64 `json:"confirmations"` // 充值入账所需确认数
	HotKeyPath       string `json:"hotKeyPath"`    // 热钱包密钥标识（MVP 内存助记词派生路径，生产为 KMS key id）
	WithdrawEnabled  bool   `json:"withdrawEnabled"`
	MinWithdrawUnits int64  `json:"minWithdrawUnits"` // 最小提现额（最小单位）
	FeeUnits         int64  `json:"feeUnits"`         // 提现固定手续费（最小单位，MVP 简化）
}

// Tx 链上交易统一模型（充值/提现/归集共用）
type Tx struct {
	Hash          string    `json:"hash"`
	From          string    `json:"from"`
	To            string    `json:"to"`
	Asset         string    `json:"asset"`
	AmountUnits   int64     `json:"amount"` // 最小单位
	Confirmations uint64    `json:"confirmations"`
	BlockHeight   uint64    `json:"blockHeight"`
	Time          time.Time `json:"time"`
	// ERC-20 等日志型转账的定位（防止同区块重复解析）
	LogIndex uint `json:"logIndex"`
	// 目标地址类型：USER（用户充值地址）/ HOT（热钱包归集）
	Kind string `json:"kind"`
}

// Block 区块（含交易）
type Block struct {
	Height uint64
	Txs    []*Tx
}

// ChainAdapter 链适配器统一接口。
// 每支持一条链实现一个 Adapter，内部封装节点 RPC / SDK 差异。
type ChainAdapter interface {
	Chain() Chain
	// LatestHeight 最新区块高度
	LatestHeight(ctx context.Context) (uint64, error)
	// Block 获取指定高度区块内的相关交易（充值/归集/提现回执均在此解析）
	Block(ctx context.Context, height uint64) (*Block, error)
	// TxConfirmations 交易确认数
	TxConfirmations(ctx context.Context, hash string) (uint64, error)
	// Broadcast 广播签名后的原始交易，返回 tx hash
	Broadcast(ctx context.Context, rawHex string) (string, error)
	// Balance 查询地址余额（最小单位；asset.Symbol + contract 定位）
	Balance(ctx context.Context, address string, asset *Asset) (int64, error)
	// BuildSignedTx 构造并签名交易（热钱包私钥签名），返回 raw hex
	//   fromKey：私钥字节（EVM=ECDSA 私钥、BTC=WIF 私钥）
	//   nonce：EVM 用 nonce；BTC 忽略
	BuildSignedTx(ctx context.Context, fromKey []byte, to string, asset *Asset, amountUnits int64, nonce uint64) (rawHex string, err error)
}

// ChainConfig 链节点连接配置
type ChainConfig struct {
	Chain    Chain
	RPCURL   string // JSON-RPC / gRPC 节点地址
	ChainID  *int64 // EVM 链 ID（签名必需）
	User     string // RPC 认证（BTC 节点）
	Password string
}

// DepositEvent 充值入账事件（钱包服务 → 资产账本）
type DepositEvent struct {
	TxHash      string
	UserID      string
	Asset       string
	AmountUnits int64
}

// WithdrawalRequest 提现申请（领域对象）
type WithdrawalRequest struct {
	ID           string    `json:"id"`
	UserID       string    `json:"userId"`
	Asset        string    `json:"asset"`
	ToAddress    string    `json:"toAddress"`
	AmountUnits  int64     `json:"amount"`
	FeeUnits     int64     `json:"fee"`
	Status       string    `json:"status"` // PENDING/APPROVED/REJECTED/SIGNED/BROADCAST/SUCCESS/FAILED
	TxHash       string    `json:"txHash,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Reviewer     string    `json:"reviewer,omitempty"`
	RejectReason string    `json:"rejectReason,omitempty"`
}

// 提现状态机：PENDING → APPROVED → SIGNED → BROADCAST → SUCCESS
//
//	↘ REJECTED          ↘ FAILED
const (
	WithdrawPending   = "PENDING"
	WithdrawApproved  = "APPROVED"
	WithdrawRejected  = "REJECTED"
	WithdrawSigned    = "SIGNED"
	WithdrawBroadcast = "BROADCAST"
	WithdrawSuccess   = "SUCCESS"
	WithdrawFailed    = "FAILED"
)

// 目标地址类型
const (
	KindUser = "USER"
	KindHot  = "HOT"
)
