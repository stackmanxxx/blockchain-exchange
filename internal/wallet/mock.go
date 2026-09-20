package wallet

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MockAdapter 内存模拟链适配器（本地开发/演示/单元测试用）。
// 生产环境不注册；仅当配置启用 MOCK 模式时使用。
type MockAdapter struct {
	mu          sync.Mutex
	chain       Chain
	height      uint64
	blocks      map[uint64]*Block
	confs       map[string]uint64
	broadcasted []string
	balances    map[string]int64
}

func NewMockAdapter(chain Chain) *MockAdapter {
	return &MockAdapter{
		chain:    chain,
		blocks:   make(map[uint64]*Block),
		confs:    make(map[string]uint64),
		balances: make(map[string]int64),
	}
}

func (m *MockAdapter) Chain() Chain { return m.chain }

// AddBlock 注入一个区块（测试/演示用）
func (m *MockAdapter) AddBlock(height uint64, txs ...*Tx) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocks[height] = &Block{Height: height, Txs: txs}
	if height > m.height {
		m.height = height
	}
}

// SetConf 设置交易确认数（测试/演示用）
func (m *MockAdapter) SetConf(hash string, conf uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confs[hash] = conf
}

// SetBalance 设置地址余额（归集测试用）
func (m *MockAdapter) SetBalance(addr string, units int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balances[addr] = units
}

// Broadcasted 已广播的 raw 交易（断言用）
func (m *MockAdapter) Broadcasted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.broadcasted...)
}

func (m *MockAdapter) LatestHeight(ctx context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.height, nil
}

func (m *MockAdapter) Block(ctx context.Context, height uint64) (*Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blocks[height]
	if !ok {
		return &Block{Height: height}, nil
	}
	return b, nil
}

func (m *MockAdapter) TxConfirmations(ctx context.Context, hash string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.confs[hash], nil
}

func (m *MockAdapter) Broadcast(ctx context.Context, rawHex string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broadcasted = append(m.broadcasted, rawHex)
	return fmt.Sprintf("0xmock%s", rawHex), nil
}

func (m *MockAdapter) Balance(ctx context.Context, address string, asset *Asset) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.balances[address], nil
}

func (m *MockAdapter) BuildSignedTx(ctx context.Context, fromKey []byte, to string, asset *Asset, amountUnits int64, nonce uint64) (string, error) {
	// 模拟签名：raw hex 编码参数
	return fmt.Sprintf("raw:%s:%d:%s", to, amountUnits, string(fromKey)), nil
}

var _ ChainAdapter = (*MockAdapter)(nil)
var _ = time.Now
