# blockchain-exchange 项目长期记忆

## 项目约定
- 金额一律使用最小单位 int64（1e8 精度），换算统一走 `pkg/decimal.PriceQtyToQuote(price, qty)`，严禁 float64 处理资金
- 撮合引擎：单 goroutine 事件循环（channel 分发），订单簿价格优先+时间优先；listener 回调（如行情）**不得同步调用引擎方法**（会自死锁），需异步
- 撮合 match 循环必须检查剩余量，防止 qty=0 忙循环
- 订单状态机：NEW → PARTIALLY_FILLED → FILLED / CANCELED / REJECTED / EXPIRED（IOC 用 EXPIRED）

## 环境（重要）
- 本机 Go 环境变量损坏：GOROOT 系统级指向 D:\golang\goroot（无效），Go 1.23.11 安装 src 与二进制不匹配。**可用工具链**：D:\golang\go1.25.4.extract\go（go1.25.4），GOPATH=D:\golang\gopath
- 运行 go 命令需：`export GOROOT="D:\golang\go1.25.4.extract\go" && export GOPATH="D:\golang\gopath"`，然后用完整路径 `D:\golang\go1.25.4.extract\go\bin\go.exe` 调用（PATH 中的旧 go 会优先）
- go test 在本机 bash 工具下输出会被吞，需重定向到文件（`go test ./... > /tmp/x.log 2>&1`）再 cat 读取
- 依赖版本兼容 Go 1.23+：gin v1.10.0、gorilla/websocket v1.5.3、golang-jwt v5.2.1、x/crypto v0.31.0

## 现状（2026-08-19）
- 已实现 MVP：撮合引擎、账本、订单/清算、行情聚合、WebSocket、认证、REST API，全部测试通过
- 已实现 Phase 2 链上充提（internal/wallet，5 单测全过）：
  - HD 钱包 BIP39/BIP44（ETH/BTC 派生，验证向量正确：test junk 助记词 m/44'/60'/0'/0/1 → 0x70997970...）
  - EVM 适配器（go-ethereum v1.17.1）、BTC RPC 适配器、TRON 自研 HTTP 适配器（TRX+TRC-20，本地 secp256k1 签名）、充值扫描、提现状态机、归集
  - 无节点时 MOCK 链演示；API：deposit-address/withdraw/withdrawals
- 待做：BTC UTXO 自签名（当前依赖节点钱包）、Kafka 事件总线、MySQL 持久化、K8s（见技术方案 Phase 2.5-3）
- 文件写保护坑：本机 go.exe 只能新建不能修改已有 go.mod/go.sum/exchange.exe（且这些文件常被加 append-only 属性）→ 需 chattr -a 清除后删除重建，或用 Write 工具写回；构建产物用新文件名（如 exchange_v2.exe）避免替换失败
