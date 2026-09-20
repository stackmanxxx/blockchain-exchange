# blockchain-exchange

基于 **Golang** 的数字资产交易所后端（MVP 单进程版）`。

## 功能

- 用户注册/登录（bcrypt + JWT 24h）
- 资产账本：复式记账流水、可用/冻结双余额、充值入账
- 撮合引擎：内存订单簿、**价格优先 + 时间优先**、单 goroutine 事件循环
  - 限价单（GTC / IOC / FOK）、市价单（买按 quote 金额、卖按 base 数量）
- 清算：成交后买卖双方资金划转、撤单/剩余资金解冻
- 行情：K线（1m/5m/15m/1h/4h/1d）、深度、最近成交、24h Ticker
- WebSocket 实时推送（depth / trade / kline / ticker）
- 金额全程最小单位 int64（1e8 精度），杜绝浮点误差
- **链上充提（Phase 2）**：
  - 多链适配器抽象（EVM：原生币+ERC-20 / BTC），HD 钱包 BIP39/BIP44 派生充值地址
  - 充值扫描器：扫块 → 确认数达标 → 幂等入账
  - 提现：申请 → 冻结 → 签名 → 广播 → 确认跟踪（MVP 自动审批，生产走风控）
  - 归集（Sweep）：充值地址余额定期归集到热钱包
  - 无节点时可启用 MOCK 链模式本地演示

## 快速开始

```bash
# 环境：Go 1.23+
go build -o bin/exchange.exe ./cmd/exchange
./bin/exchange.exe
# 服务监听 :8080（可用 EXCHANGE_ADDR / JWT_SECRET 环境变量覆盖）
```

## API 示例

```bash
BASE=http://localhost:8080/api/v1

# 1. 注册 / 登录
curl -s -X POST $BASE/auth/register -H 'Content-Type: application/json' \
  -d '{"email":"alice@test.com","password":"password123"}'
TOKEN=$(curl -s -X POST $BASE/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"alice@test.com","password":"password123"}' | python -c "import sys,json;print(json.load(sys.stdin)['data']['token'])")

# 2. 充值（MVP 模拟入账——生产环境由链上扫描驱动）
# 代码/测试中直接调用 asset.Service.Deposit

# 3. 下单：卖 1 BTC @ 60000（先给卖家用例）
curl -s -X POST $BASE/orders -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"symbol":"BTCUSDT","side":"SELL","type":"LIMIT","price":"60000","quantity":"1"}'

# 4. 行情
curl -s $BASE/market/ticker/BTCUSDT
curl -s "$BASE/market/depth/BTCUSDT?limit=5"
curl -s "$BASE/market/klines/BTCUSDT?interval=1m"
```

### WebSocket 订阅

```bash
# 需支持 WSS 客户端（如 wscat / 浏览器）
wscat -c ws://localhost:8080/ws
> {"op":"subscribe","channel":"depth:BTCUSDT"}
> {"op":"subscribe","channel":"trade:BTCUSDT"}
> {"op":"subscribe","channel":"kline:BTCUSDT:1m"}
> {"op":"subscribe","channel":"ticker:BTCUSDT"}
```

### 链上充提（Phase 2）

```bash
# 获取充值地址（HD 派生，同一用户恒定）
curl -s $BASE/wallet/deposit-address/ETH -H "Authorization: Bearer $TOKEN"
# => {"address":"0x..."}  向该地址转入 ETH 即自动入账（扫块确认后）

# 提现（MVP 提交即自动签名广播；生产走审批流）
curl -s -X POST $BASE/wallet/withdraw -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"asset":"ETH","toAddress":"0xRecipient...","amount":"0.5"}'

# 提现记录
curl -s $BASE/wallet/withdrawals -H "Authorization: Bearer $TOKEN"
```

> 未配置节点时服务以 **MOCK 链** 模式启动（日志有提示），可用 `mock.SetBalance` 模拟充值/归集。
> 连接真实节点：设置 `ETH_RPC_URL`（+ `ETH_CHAIN_ID`）、`BTC_RPC_URL`/`BTC_RPC_USER`/`BTC_RPC_PASS`；
> 助记词通过 `WALLET_MNEMONIC` 注入（生产从 KMS 获取）。

## 目录结构

```
cmd/exchange/       入口（组装全部服务）
internal/
  auth/            用户注册/登录/JWT
  asset/           资产账本（冻结/划转/流水）
  order/           订单服务（校验/冻结/清算/撤单）
  engine/          撮合引擎（订单簿/事件循环/快照）
  market/          行情聚合（K线/Ticker/深度）
  ws/              WebSocket hub
  api/             Gin 路由 + handlers + 鉴权中间件
  domain/          领域模型与错误
pkg/decimal/       金额运算（1e8 精度）
```

## 测试

```bash
go test ./... -v
```

## 与生产架构的差距

| MVP（本仓库） | 生产                               |
| -------- | -------------------------------- |
| 进程内内存存储  | MySQL + Redis + ClickHouse       |
| 进程内同步清算  | Kafka 事件 + settlement-svc 异步幂等消费 |
| 撮合内存快照   | 快照落 Redis + Kafka 回放（RTO < 30s）  |
| 模拟充值     | 链上扫块（go-ethereum/btcd）+ 冷热钱包     |
| 单进程 HTTP | 微服务 + gRPC + K8s 水平扩展            |
