# pkg/mq — Redis Streams 消息队列

本目录实现本服务使用的 Redis Streams 消息队列（生产者、消费者、死信队列），
只依赖公有包：`github.com/go-redis/redis/v8`、`github.com/google/uuid`。

## 结构

| 本地路径 | 作用 |
| --- | --- |
| `mq.go` | 生产者/消费者接口与消息结构 |
| `log/nop/nop.go` | 默认静默日志 handler |
| `redismq/redismq.go` | 生产者/消费者装配入口与默认值 |
| `redismq/consumer/` | 集群/广播消费、重试、死信、续租与关闭 |
| `redismq/producer/` | 发布、topic/label 到 stream 的映射 |
| `redismq/util/topics.go` | topic/label 组合与解析 |
| `redismq/model/message_values.go` | stream 字段布局 |

本服务不使用内存队列与示例程序，因此未包含 `memq/`、`mockmq/`、`example/`。

## 设计要点

### 1. 生产者默认不裁剪（`producer/option.go`、`redismq.go`）

默认不设置 `MAXLEN`。对工作队列来说按条数裁剪会静默丢任务：消费端宕机或积压超过保留条数时，
最早的待处理消息会被 Redis 直接删掉。

- 默认 `0` = 不裁剪；`WithStreamMaxLen(0)` 显式关闭裁剪。
- 新增 `WithStreamMinID`（`MINID` 裁剪，按时间而非条数，安全上限）、
  `WithApproximateTrim`、`WithTrimLimit`、`WithOperationTimeout`。
- `XADD` 只在显式配置时才带裁剪参数；有测试断言默认参数为空。

### 2. 长任务消费（`consumer/subscribe_cluster.go`）

分钟级转写任务需要正确处理超时、续租和关闭。

**(a) callback context 带超时**
按 `WithConsumeTimeout` 派生 `context.WithTimeout`；`<= 0` 表示不设 deadline
（仍随 Close 取消）。运行器把 consume timeout 设为 ≥ 任务执行超时。

**(b) keep 不抢回别人的消息**
keep 走 Lua 脚本 `keepScript`：先 `XPENDING ... consumer==自己` 判断归属，再
`XCLAIM ... JUSTID`（`JUSTID` 保证不增加投递计数）。归属已丢失返回 `ErrOwnershipLost`，
不再抢回。测试 `TestKeepDetectsForeignOwner`、`TestKeepScriptDoesNotBumpDeliveryCount` 覆盖。

**(c) keep 失败会停止 callback**
续租失败不能继续运行并 ACK：

- 归属丢失 → 立刻 `guard.fail()`，取消 callback context；
- 基础设施错误连续 3 次（`keepErrorRetries`）→ 同样取消；
- callback 返回后若 `guard.Lost()`，**不 ACK**，消息留给新的 owner。

**(d) DLQ 转移是原子的**
用单个 Lua 脚本 `dlqTransferScript` 完成 `XADD` + `XACK`，避免中途崩溃造成重复死信或永久 pending。
脚本先检查消息是否仍 pending —— `XACK` 不删除 stream 条目，
所以“脚本成功但进程随后崩溃”后的重跑必须只补一次 `XACK`，不能追加第二条死信。
测试 `TestDLQTransferIsAtomic` 断言重跑返回 0 且死信流仍只有 1 条。

**(e) glob topic 用 SCAN 扫描**
glob topic 用游标式 `SCAN`，避免 `KEYS` 在线上 Redis 上阻塞，
并按 `topicRefreshInterval` 缓存解析结果；精确 topic 直接由 namespace 推导，不查 Redis。

**(f) Close 真正停止消费**
`Close` 分四个阶段：

1. `gate` 关闭 handler 入口，之后 `beginHandler` 返回 false，新消息一律不 ACK 留在 PEL；
2. 关闭 `stopCh`，读循环停止拉新消息（`XREADGROUP` 也不再发起）；
3. `inflight.WaitGroup` 等待在跑的 callback，受 `closeTimeout` 约束：
   `<0` 立即取消，`=0` 无限等，`>0` 超时后真正 `runCancel()` 取消；
4. 分别有界等待读循环与 keep goroutine 退出。

测试 `TestCloseStopsPullingAndAcksInflight`（等在跑的活干完并 ACK，且不消费积压）、
`TestCloseTimeoutCancelsInflight`（超时真的取消）、`TestCancelStopsRunningCallback`。

### 3. 其他

- `ConsumeMode` 常量带显式类型（`ConsumeModeCluster` / `ConsumeModeBroadcast`），
  避免和裸字符串混用。
- `NewConsumer`/`NewProducer` 对 `nil` context、`nil` log handler、负数值做保护，不覆盖默认值。
- callback panic 被 recover 成 error，走正常重试/死信路径，不再打崩进程。
- 广播模式：`findLatestID` 用 `XREVRANGE ... COUNT 1`，
  `CONSUME_FROM_FIRST/LAST_OFFSET` 的起点在 `loadOffset` 里显式定义。
- 集群模式新建 group 固定从 `"0"` 开始：首次部署不丢已入队的任务。
  因此 `ConsumeFromWhere` 只影响广播模式，这一点写在 `topic.go` 的 `groupStartOffset` 注释里。

## API 兼容性

`internal/tasks` 的 agent 使用 `redismq.NewRedisMq(...)`、`Consumer/Producer(uniqueId)`、
`Connect/Publish/Subscribe/Close`，以及 `mq.Message`/`mq.MessageExt`。
`MessageExt.MsgId` 在集群模式下是 Redis stream entry id，可用于排障。

## 测试

单元测试不需要 Redis。集成测试需要一次性 Redis，未设置时自动 skip：

```bash
MUSIC_TEST_REDIS_URL=redis://127.0.0.1:6399/0 go test ./pkg/mq/... -count=1
```

本地开发用独立容器（测试会往里写数据）：

```bash
docker run --rm -d --name mt-redis-test -p 6399:6379 redis:7-alpine
```

测试隔离约定：

- 每个测试用 `mt-mqtest-<uuid>` 作为 namespace，key 形如 `mt-mqtest-xxx:Q:<topic>`；
- 清理只 `SCAN <namespace>:*` + `DEL`，**从不** `FLUSHDB`/`FLUSHALL`，不会动同实例里的其他数据；
- 断言都落在真实 Redis 状态上：`XPENDING`/`XPENDINGEXT`（pending 数量、owner、投递计数）、
  `XLEN`（未裁剪）、`XRANGE`（死信条目字段）、`XINFO GROUPS`（广播模式不建 group）。
- `-race` 需要 cgo，本机默认关闭时可加 `CGO_ENABLED=1`。

覆盖的场景：

| 文件 | 覆盖 |
| --- | --- |
| `consumer/redis_ack_test.go` | ACK、callback 失败重投、重试耗尽进死信 |
| `consumer/redis_longtask_test.go` | 长任务续租不被重投、cancel、Close 排空、Close 超时取消、owner 丢失、keep 连续失败 |
| `consumer/redis_semantics_test.go` | callback context deadline、`consumeTimeout=0`、panic、Connect/Subscribe/Close 状态机 |
| `consumer/redis_discovery_test.go` | glob + SCAN 发现与缓存刷新、广播模式、FIRST_OFFSET 重放、namespace 隔离 |
| `consumer/redis_dlq_test.go` | 死信转移原子性与幂等、字段保留、续租不改投递计数 |
| `producer/redis_producer_test.go` | 默认不裁剪（300 条全在）、字段布局与 label 编码、Close 后拒发 |
| `redismq/redismq_test.go` | 门面默认值、选项保护、namespace/stream 接线端到端 |
