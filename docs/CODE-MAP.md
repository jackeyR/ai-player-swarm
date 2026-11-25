# 项目源码与组成清单

> 本文由 [`scripts/gen_codemap.py`](../scripts/gen_codemap.py) 生成：**行数与导出符号是现场扫描的**，只有「作用」一列是人工描述。改了代码重跑一次即可刷新，不会出现「文档写着 300 行、代码已经 800 行」的窘境。

```bash
python scripts/gen_codemap.py
```

## 1. 这个项目是什么

一个**用 bot 玩家模拟真人行为、压房间服与匹配器**的压测平台。它同时是被测系统的作者与压测工具的作者——这既是它能一个人做完的原因，也是它和「拿 JMeter 压别人的接口」在叙事上的根本区别。

四层架构，每层独立可测、独立可换：

```
控制面   controller(HTTP) → runner(单机执行) → results/*.jsonl
行为面   persona 分布 · 会话状态机 · 双峰思考时间 · 危险率退出 · 非法输入注入
协议面   gateway: WebSocket + 信封      （登录 / 排队 / 匹配）
         room   : 裸 TCP + 定长二进制    （10Hz 输入 / 快照 / 结算）
观测面   Prometheus 文本暴露 · HdrHistogram(CO 修正) · JSONL → DuckDB
           ↓
被测系统 登录 → 匹配器 → 分配器(Redis 热池) → 房间服(10Hz tick) → 结算写库
```

## 2. 组成总览

### 2.1 规模

| 项 | 数量 |
|---|---|
| 源码文件（Go，非测试） | 45 |
| 源码行数（Go，非测试） | 12,412 |
| 测试文件 | 19 |
| 测试行数 | 4,348 |
| 测试函数 | 125 |
| Go 包（含 cmd 与 srv） | 29 |
| 第三方依赖 | 1（`gopkg.in/yaml.v3`） |
| 非 Go 文件（配置/文档/脚本） | 42 |

### 2.2 目录树

```
swarm/
  cmd/
    sutd/         被测系统入口（接入层+匹配器+分配器+房间服）
    runner/       单机 bot 集群执行器
    controller/   控制面 HTTP API
  internal/
    wire/         长度前缀 + 消息号 + 序列号 的二进制帧
    ws/           RFC6455 WebSocket（零依赖）
    protocol/     控制面 JSON / 数据面定长二进制 的消息定义
    clock/        固定步长调度 + 帧预算
    sched/        开环到达时间表与派发器
    behavior/     persona 分布、双峰节奏、危险率退出、可复现随机
    bot/          会话状态机 + 接入层客户端 + 房间客户端
    hist/         HdrHistogram + coordinated-omission 修正
    metrics/      Prometheus 文本暴露
    obs/          全平台指标清单
    report/       分位数双路采集
    sink/         JSONL 结果落盘
    runstats/     跨 bot 原子计数
    plan/         负载计划（YAML + 指纹）
    runner/       单机执行器
    controller/   控制面
  srv/
    config/       SUT 配置聚合
    sut/          四层组装（被 sutd 与端到端测试共用）
    gateway/      接入层（WS + 鉴权 + 排队）
    account/      账号、token、MMR
    match/        匹配器（窗口放宽）
    alloc/        分配器（Redis 池 + 全局锁）
    room/         房间服（10Hz tick / 权威裁决 / 回收）
    store/        Redis 与结算库模拟
  plans/          负载计划与 persona 目录
  deploy/         compose / Prometheus / Grafana / SUT 配置
  analysis/       离线分析（KS / Wasserstein / CDF）
  scripts/        冒烟、md→docx、本清单生成器
  docs/           SLO / METRICS / FINDINGS / HANDWRITE / CODE-MAP
```

### 2.3 四个可执行文件

| 二进制 | 命令 | 默认监听 | 职责 |
|---|---|---|---|
| `sutd` | `sutd -config deploy/sut.yaml` | 8080(WS) / 8090(TCP) / 9090(指标) | 被测系统：接入层 + 匹配器 + 分配器 + 房间服 |
| `runner` | `runner -plan plans/smoke.yaml` | 9091(指标) | bot 集群单机执行器 |
| `controller` | `controller -addr :7070` | 7070(HTTP) | 控制面：提交/查询/停止实验 |
| `client` | `client -account me` | — | 真人客户端：交互 / 逐行 / 无人值守，并把真人数据落盘 |

### 2.4 一次完整链路（7 步）

| 步 | 动作 | 协议 | 代码入口 |
|---|---|---|---|
| 1 | bot 连接入层，Hello/Welcome 交换版本与 nonce | WS 二进制 + 信封 | `internal/bot/client.go` → `srv/gateway/gateway.go` |
| 2 | 用 nonce 做 HMAC 签名登录，拿 token 与 MMR | WS | `srv/account/account.go` |
| 3 | 进匹配队列，等待匹配推送（可取消） | WS | `srv/match/match.go` |
| 4 | 匹配成功，分配器从一个 Redis 热键池里取房间 | 进程内 | `srv/alloc/alloc.go` |
| 5 | bot 用 room_token 连房间服 TCP，进房拿槽位 | 裸 TCP + 长度前缀帧 | `srv/room/manager.go` |
| 6 | 10Hz：bot 发输入、房间服权威裁决后广播全量快照 | 裸 TCP 定长二进制 | `srv/room/room.go` |
| 7 | 结算：同步写库 → 推送结果 → 房间回收 → bot 再排或退出 | 裸 TCP + 进程内 | `srv/room/room.go` / `srv/store/store.go` |

## 3. 源码清单

按层排列。`行` 是文件行数（含注释）；`关键导出` 是包外可见的标识符，标「内部实现」说明那个文件只能被同包的其他文件使用。

> 测试文件不在这里列，统一在 §5「测试清单」——每条测试都对应一个会真出问题的性质。

### 3.1 入口层（cmd）

三个可执行文件，各自只做「解析参数 + 调用下层 + 打印」。真正的组装在 srv/sut 与 internal/runner。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `cmd/client/main.go` | 736 | 真人客户端：交互 TUI / 逐行 / 无人值守三模，记录真人数据（bots.jsonl + trace.jsonl + sessions.json）供保真度对照。 | `Close` / `Count` / `Events` / `Match` / `Trace` |
| `cmd/controller/main.go` | 58 | 控制面入口：起 HTTP 服务，接受计划提交、查询、停止；退出时取消在跑的实验。 | —（内部实现，不导出） |
| `cmd/runner/main.go` | 96 | bot 集群单机入口：读负载计划 → 解析相对路径 → 建 runner → 跑到结束 → 打印分位数汇总。 | —（内部实现，不导出） |
| `cmd/sutd/main.go` | 120 | 被测系统的入口：解析参数 → 覆盖配置 → 组装四层 → 等信号优雅退出。真正的组装逻辑在 srv/sut，这里只做参数与打印。 | —（内部实现，不导出） |

### 3.2 协议层（wire / ws / protocol）

从字节流到业务消息的三层：帧（怎么分边界）→ 传输（WS 握手与分片）→ 消息（字段语义）。这一层换掉，行为层与观测层不用动。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `internal/protocol/binbuf.go` | 161 | 零依赖大端二进制读写器：越界只置错误位不 panic，字符串带 u8 长度前缀。 | `BinReader` / `BinWriter` / `Bytes` / `Err` / `ErrBadBody` / `I16` …+9 |
| `internal/protocol/gateway.go` | 112 | 控制面消息体（JSON）：Hello/Welcome/Login/EnterQueue/MatchFound/Ping/Pong/Bye。 | `Bye` / `CancelQueueResp` / `EnterQueueReq` / `EnterQueueResp` / `Hello` / `LoginReq` …+6 |
| `internal/protocol/protocol.go` | 162 | 协议骨架：版本号、控制面/数据面消息号常量、MsgName、Error 载荷、信封 Marshal/Unmarshal。 | `Envelope` / `Error` / `Marshal` / `MsgName` / `ProtoVersion` / `Unmarshal` |
| `internal/protocol/room.go` | 380 | 数据面消息体（定长二进制）：Input/Snapshot/Settle/Join/Reconnect + 房间规则常量（速度、命中半径、伤害、血量）。 | `DecodeInput` / `DecodeInputAck` / `DecodeReconnectReq` / `DecodeRoomJoinReq` / `DecodeRoomJoinResp` / `DecodeSessionProgress` …+14 |
| `internal/wire/wire.go` | 181 | 应用层二进制帧：裸 TCP 用「长度前缀 + 消息号 + 序列号」，WS 用无长度前缀的信封。含半包/短写/超大帧防护。 | `DecodeEnvelope` / `EncodeEnvelope` / `Frame` / `NewReader` / `NewWriter` / `ReadFrame` …+4 |
| `internal/ws/client.go` | 200 | 客户端 Dial：支持 ws/wss、自定义头、TLS、子协议，校验服务端握手响应。 | `Dial` / `DialOptions` |
| `internal/ws/conn.go` | 505 | 连接本体：读写帧、分片重组、Ping 自动回 Pong、Close 状态机、并发写锁、幂等关闭。 | `Close` / `Conn` / `LocalAddr` / `ReadMessage` / `RemoteAddr` / `SetReadDeadline` …+5 |
| `internal/ws/frame.go` | 105 | 帧头编解码：7/16/64 位长度、FIN/RSV 位、掩码运算、close payload 解析。全是内部函数，不导出。 | —（内部实现，不导出） |
| `internal/ws/server.go` | 160 | 服务端握手：校验 Upgrade/Version/Key、回 101 + Sec-WebSocket-Accept、Hijack 拿到底层 conn。 | `Upgrade` / `Upgrader` |
| `internal/ws/ws.go` | 58 | RFC6455 基础定义：OpCode 常量、CloseError、ErrClosed / ErrBadHandshake / ErrFrameTooLarge。 | `CloseError` / `Error` / `OpCode` |

### 3.3 调度与时间层（clock / sched）

整个压测平台的地基：开环到达时间表 + 固定步长 tick。开环与闭环的区别就在这两百行里。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `internal/clock/clock.go` | 166 | 固定步长调度器（按绝对时间表推进、带滞后上报）+ 单帧预算计数器。对应知识清单 B9 / D2。 | `Budget` / `C` / `GoroutineCount` / `NewBudget` / `NewStepper` / `NowMillis` …+7 |
| `internal/sched/sched.go` | 345 | 开环到达调度：五种流量形状的到达时间表生成 + Dispatcher（准时喊人、不等人）。对应 C2 / C3 / E5。 | `Arrival` / `Arrivals` / `Defaults` / `Dispatcher` / `ExpectedInterval` / `LagStats` …+7 |

### 3.4 行为层（behavior）

「像不像人」的全部来源：persona 分布、双峰节奏、危险率退出、可复现随机。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `internal/behavior/behavior.go` | 457 | persona 模型：六种分布采样、双峰思考时间、退出危险率、可复现 PCG 随机、移动生成器。对应 E6 / E7 / J2。 | `Catalog` / `Const` / `Dist` / `DistKind` / `ForIndex` / `LoadFile` …+17 |
| `internal/behavior/defaults.go` | 163 | 内置的 6 种玩家原型（休闲/新手/硬核/挂机/破坏者/组队）及其分布参数。 | `Default` |

### 3.5 bot 与客户端层（bot / play）

协议级 bot 的会话状态机与两个客户端；以及真人客户端的终端界面与键盘。两者复用同一套 wire/protocol——这是「真人能玩通」等于「协议实现正确」的原因。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `internal/bot/bot.go` | 816 | bot 会话状态机：连接→握手→鉴权→排队(可取消)→匹配→进房→对局(可重连/秒退)→结算→再排。含非法输入注入与追击逻辑。 | `Bot` / `Config` / `Defaults` / `Deps` / `New` / `Run` …+2 |
| `internal/bot/client.go` | 293 | 接入层客户端：WS 连接、Hello/Login 握手、排队请求、匹配推送接收；用 seq 做并发请求-响应配对。 | —（内部实现，不导出） |
| `internal/bot/roomclient.go` | 223 | 房间服客户端：TCP 连接、进房/重连、输入发送、快照/ack/结算接收，队列满时丢弃可丢帧。 | —（内部实现，不导出） |
| `internal/bot/session.go` | 209 | 对外公开的玩家会话 API（Player / RoomSession）：给真人客户端复用同一套协议实现，含 TrySettle 这类「默认正确」的竞态处理。 | `Account` / `Acks` / `CancelQueue` / `Close` / `Closed` / `ConnectPlayer` …+21 |
| `internal/play/keys.go` | 35 | 按键事件与按键源接口，以及控制台字符到协议键位的归一化。 | `KeyEvent` / `KeySource` |
| `internal/play/keys_other.go` | 16 | 非 Windows 平台的降级：明确返回「不支持原始键盘」，由调用方退回逐行输入。 | `EnableVT` / `NewKeys` |
| `internal/play/keys_windows.go` | 161 | Windows 控制台原始模式：关行缓冲/回显、开 ANSI，退出时恢复原模式（不恢复会让用户终端不再回显）。 | `Close` / `EnableVT` / `Events` / `NewKeys` |
| `internal/play/view.go` | 369 | 真人客户端的终端界面：TUI 全屏（战场网格 + 记分板 + 状态行）与逐行降级两套渲染。 | `Cleanup` / `Draw` / `NewRenderer` / `Renderer` / `ShowResult` / `ShowStatus` …+1 |

### 3.6 观测层（hist / metrics / obs / report / sink / runstats）

测量与出口：分位数（HdrHistogram）、指标暴露（Prometheus 文本）、指标清单（单一事实来源）、双路采集、结果落盘、计数。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `internal/hist/hist.go` | 511 | HdrHistogram：对数线性桶、CO 修正补记、Merge/Export/Buckets/快照 + 并发安全 Recorder。对应 C3 / C4。 | `Bucket` / `Buckets` / `Export` / `Histogram` / `Lock` / `Max` …+15 |
| `internal/metrics/metrics.go` | 698 | 自实现的 Prometheus 指标库：Counter/Gauge/Histogram + 标签向量 + 文本暴露格式 + 运行时指标。对应 G2–G4。 | `Add` / `Counter` / `CounterVec` / `Dec` / `EnableRuntimeMetrics` / `Gauge` …+14 |
| `internal/obs/obs.go` | 211 | 全平台指标清单（单一事实来源）：SUT 侧与 bot 侧全部指标名、标签、分桶；MMR 分桶映射。 | `MMRBucketLabel` / `NewRunner` / `NewServer` / `Runner` / `Seconds` / `Server` |
| `internal/report/reporter.go` | 107 | 双路采集：把同一次测量同时喂给 Prometheus 桶与 HdrHistogram，并额外维护「真实观测次数」防止 CO 修正误导。 | `ExpectedInterval` / `Latency` / `New` / `Phase` / `Reporter` / `Samples` …+1 |
| `internal/runstats/runstats.go` | 62 | 一次实验的跨 bot 原子计数（会话/对局/胜负/输入/断线），goodput 的业务口径来源。 | `Snapshot` / `Stats` / `Take` |
| `internal/sink/sink.go` | 180 | 结果落盘：并发安全的 JSONL 写入器 + 会话/运行汇总的记录结构（含阶段偏移与耗时）。 | `Bytes` / `Close` / `Errors` / `Flush` / `Lines` / `New` …+6 |

### 3.7 运行与控制面（plan / runner / controller）

把「一份 YAML」变成「一次可复现的实验」：计划校验与指纹 → 单机执行 → HTTP 控制。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `internal/controller/controller.go` | 385 | 控制面：6 个 HTTP 端点 + 实验生命周期 + 并发实验限制 + 配置快照。 | `Config` / `Controller` / `Handler` / `ListenAndServe` / `New` / `Run` …+3 |
| `internal/plan/plan.go` | 215 | 负载计划：YAML 加载、默认值、校验、稳定指纹、persona 目录解析、CO 预期间隔。对应 K1 / H8。 | `Behaviors` / `Defaults` / `ExpectedInterval` / `Fingerprint` / `Limits` / `Load` …+6 |
| `internal/runner/runner.go` | 385 | 单机 runner：生成到达表 → 信号量限并发 → 起 bot → 汇总分位数 → 写 plan/meta/result/bots 四个产物。 | `Config` / `Dir` / `ID` / `New` / `Result` / `Run` …+2 |

### 3.8 被测系统（srv）

靶子本身：接入层 + 匹配器 + 分配器 + 房间服 + 存储模拟。三个「故意留下的瓶颈」都在这一层，且全部做成配置开关。

| 文件 | 行 | 作用 | 关键导出 |
|---|---|---|---|
| `srv/account/account.go` | 314 | 账号与会话：登录签名校验、自动注册、token 签发与过期、Elo-lite MMR 更新。 | `Account` / `ApplyMatch` / `DefaultMaxSkew` / `FailureCount` / `Login` / `LoginCount` …+10 |
| `srv/alloc/alloc.go` | 173 | 分配器：Redis 预热房间池 + 全局锁（两个刻意瓶颈）+ 分配延迟指标 + 补池循环。 | `Allocate` / `Allocator` / `Config` / `Defaults` / `New` / `PoolSize` …+4 |
| `srv/config/config.go` | 107 | SUT 配置聚合：接入层/房间/分配器/匹配器/Redis/结算六段，全部可调，瓶颈即配置。 | `Config` / `Default` / `Defaults` / `Load` / `Redis` / `Settlement` …+1 |
| `srv/gateway/gateway.go` | 485 | 接入层：WS 握手、Nonce+HMAC 登录鉴权、排队/取消、匹配结果推送、/metrics 与 /debug/state。 | `AccountService` / `Addr` / `CheckQueueRequest` / `Config` / `Defaults` / `ListenAndServe` …+4 |
| `srv/match/match.go` | 424 | 匹配器：MMR 窗口逐步放宽、FIFO 防饿死、按 MMR 分边、不满开局、把匹配与分配解耦成两段延迟。 | `Assignment` / `Cancel` / `Config` / `Defaults` / `Enqueue` / `Matches` …+7 |
| `srv/room/manager.go` | 496 | 房间管理器：TCP 监听、进房 token、连接读写循环、janitor 回收与泄漏统计。 | `AdvertiseAddr` / `Create` / `Listen` / `Manager` / `NewManager` / `RoomCount` …+4 |
| `srv/room/peer.go` | 125 | 单连接的写侧：有界发送队列（背压）、异步写循环、同步写（结算帧必须送达）、幂等关闭。 | —（内部实现，不导出） |
| `srv/room/room.go` | 782 | 房间服核心：生命周期、10Hz tick、权威裁决（移动校验/命中判定/复活）、快照广播、同步结算写库、回收判定。 | `Close` / `Config` / `Defaults` / `Done` / `ErrRoomClosed` / `Hooks` …+9 |
| `srv/store/store.go` | 251 | 两个外部依赖的模拟：分片 Redis（可强制热键）+ 结算写库（可注入 fsync 延迟）。 | `Bytes` / `Count` / `Del` / `ForceHotShard` / `Get` / `Len` …+17 |
| `srv/sut/sut.go` | 214 | 四层组装点：接入层→匹配器→分配器→房间服，并负责指标端点、优雅关闭与测试用随机端口。 | `AccountService` / `GatewayWSURL` / `MetricsAddr` / `MetricsDisabled` / `New` / `RoomAddr` …+3 |

## 4. 配置、部署与计划（非 Go 文件）

这些文件同样是**实验条件的一部分**：改配置等于改实验，所以它们必须和代码一起进版本库。

| 目录 | 放什么 |
|---|---|
| `deploy/` | 部署与观测栈配置。 |
| `plans/` | 负载计划与 persona 目录（配置即实验）。 |
| `docs/` | 文档。 |
| `analysis/` | 离线分析与报告。 |
| `scripts/` | 工具脚本。 |
| `仓库根目录` | 模块声明、依赖校验和、忽略规则、README。 |

| 文件 | 行 | 作用 |
|---|---|---|
| `.github/workflows/ci.yml` | 74 | ⚠ 未填写作用描述 |
| `analysis/README.md` | 47 | 离线分析的使用说明：为什么不用 scipy/matplotlib、参照数据从哪来、口径怎么对齐。 |
| `analysis/analyze.py` | 453 | 离线分析：读 bots.jsonl → 分位数表 + KS 检验 + Wasserstein 距离 + persona 维度 + 手写 SVG 的 CDF 图。零依赖。 |
| `analysis/reference.example.json` | 4 | 参照样本占位文件：演示 KS / Wasserstein 对比流程，明确标注「不可用于报告」。 |
| `deploy/Dockerfile` | 19 | 多阶段构建：只把静态编译的二进制带进运行镜像。 |
| `deploy/compose.yaml` | 100 | 本地一键起：SUT + 控制面 + Prometheus + Grafana；runner 放在 profile 里，避免与 SUT 抢 CPU。 |
| `deploy/grafana/dashboards/swarm-overview.json` | 232 | Grafana 总览看板：11 个面板（活跃 bot、泄漏、超预算率、匹配/分配分位数、bot 阶段延迟等）。 |
| `deploy/grafana/provisioning/dashboards/swarm.yml` | 13 | Grafana 看板自动加载配置。 |
| `deploy/grafana/provisioning/datasources/prometheus.yml` | 12 | Grafana 数据源自动配置（timeInterval 与抓取间隔对齐）。 |
| `deploy/prometheus.yml` | 35 | Prometheus 抓取配置：2s 间隔（压测场景默认 15s 会抹平尖峰），抓 SUT/runner/controller。 |
| `deploy/rules.yml` | 110 | SLO 告警规则：每条规则对应 docs/SLO.md 里的一行 SLO，附 runbook 指向。 |
| `deploy/sut.duel.yaml` | 59 | 1v1 快速对局配置（team_size=1）：给真人客户端自测用，两个客户端就能开一局。 |
| `deploy/sut.fixed.yaml` | 54 | SUT 修复版配置：三个瓶颈全关 + 池子放大，用于「修复前后」对照实验。 |
| `deploy/sut.leak.yaml` | 52 | 泄漏注入配置：关掉房间回收，复现 room_leak_gauge 持续上涨。 |
| `deploy/sut.smoke.yaml` | 47 | 冒烟配置：一局 10 秒、瓶颈全关，供 CI 与本地联调。 |
| `deploy/sut.yaml` | 59 | SUT 默认配置：三个刻意瓶颈全部打开（Redis 热键、分配器全局锁、结算同步写）。 |
| `docs/CODE-MAP.md` | 328 | 本文件：项目从源码到组成的完整清单，由 scripts/gen_codemap.py 生成。 |
| `docs/FINDINGS.md` | 379 | 压测发现并修复的 8 个真实缺陷：现象→定位→根因→修复→面试怎么讲。 |
| `docs/HANDWRITE.md` | 204 | 必须自己手写的部分（从两份文档提取）：白板代码、必背数字、自写文字、自检题库。 |
| `docs/METRICS.md` | 100 | 每个指标的定义、标签、PromQL、以及「为什么 p99 和别人不一样」的口径说明。 |
| `docs/SLO.md` | 47 | 四层 SLO（登录/匹配/分配/房间）+ 业务口径 + 错误预算用法。 |
| `docs/必须手写的部分.docx` | 0 | docs/HANDWRITE.md 的 Word 版，由 scripts/md2docx.py 生成。 |
| `docs/项目源码与组成清单.docx` | 0 | 本文件的 Word 版，由 scripts/md2docx.py 生成。 |
| `plans/baseline.yaml` | 44 | 基线负载计划：阶梯到 20/s、300 bot，用于看各层稳定分位数。 |
| `plans/duel.yaml` | 45 | 陪练队列：4 个 bot 慢慢补位，让真人客户端随时能配到对手。 |
| `plans/peak.yaml` | 42 | 尖峰负载计划：基线 + 两次整点惊群，用于复现瞬时形状引发的事故。 |
| `plans/personas/default.yaml` | 148 | 默认 persona 目录（6 种原型、带分布参数），与 internal/behavior/defaults.go 有测试保证一致。 |
| `plans/personas/smoke.yaml` | 76 | 冒烟 persona：会话压到 1–2 分钟、失败路径概率调高，专供联调。 |
| `plans/smoke.yaml` | 43 | 冒烟负载计划：30 bot、5/s、90 秒，配套短会话 persona。 |
| `scripts/demo.cmd` | 7 | demo.ps1 的 cmd 包装：绕过 PowerShell 执行策略。 |
| `scripts/demo.ps1` | 92 | 一条命令开玩：起 SUT → 起陪练 bot → 启动交互客户端 → 退出时清理后台进程。 |
| `scripts/gen_codemap.py` | 546 | 本清单的生成器：扫描文件/行数/导出符号，套上人工描述，输出 docs/CODE-MAP.md。 |
| `scripts/md2docx.py` | 601 | Markdown → Word 转换器：表格按表头签名分列宽、OOXML 元素顺序归一化、页码与表头重复。 |
| `scripts/smoke.cmd` | 6 | smoke.ps1 的 cmd 包装：绕过 PowerShell 执行策略。 |
| `scripts/smoke.ps1` | 94 | 冒烟脚本：编译 → 起 SUT → 跑 runner → 生成分析报告 → 收尾清进程。 |
| `.gitattributes` | 16 | ⚠ 未填写作用描述 |
| `.gitignore` | 22 | 忽略构建产物与实验结果（results/、bin/、parquet/jsonl），配置与计划必须进版本库。 |
| `LICENSE` | 21 | ⚠ 未填写作用描述 |
| `Makefile` | 96 | 常用命令入口：build / vet / test / test-race / cover / smoke / baseline / fixed / clean。 |
| `README.md` | 208 | 项目总览：架构图、指标表、代码地图、快速开始、三个刻意瓶颈、刻意不做、已知限制。 |
| `go.mod` | 5 | 模块声明与唯一的第三方依赖：gopkg.in/yaml.v3。 |
| `go.sum` | 4 | 依赖校验和。 |

## 5. 测试清单

测试不是「补一个覆盖率」，每条都对应一个会真出问题的性质。

| 测试文件 | 行 | 函数数 | 覆盖的关键性质 |
|---|---|---|---|
| `cmd/client/main_test.go` | 196 | 7 | 结果码机器可读、按住/一次性两种输入语义、自动策略、记录产物是合法 JSONL |
| `internal/behavior/behavior_test.go` | 249 | 12 | 目录权重和、persona 可复现、样本占比、对数正态右偏、截断、双峰、危险率单调、移动越界、**YAML 与内置默认值一致** |
| `internal/clock/clock_test.go` | 113 | 7 | 无累积漂移、滞后非负、慢消费者不阻塞调度、帧预算 |
| `internal/controller/controller_test.go` | 204 | 2 | 提交→轮询→完成→列表→停止 409、四种错误请求 |
| `internal/hist/hist_test.go` | 405 | 12 | 分位精度、常量/空图、钳位、补记计数、**CO 修正效果**、Merge、累积桶、并发、JSON |
| `internal/metrics/metrics_test.go` | 466 | 13 | 文本快照、转义、累积桶、无标签不写 {}、SetFunc、并发、序列清理、运行时指标 |
| `internal/plan/plan_test.go` | 152 | 5 | 计划指纹**跨机器/跨目录稳定**，但语义变化必须变；示例计划都能解析 |
| `internal/play/view_test.go` | 155 | 7 | TUI 与逐行两套渲染、ANSI 可见宽度、血条定宽、原地刷新与退出复位 |
| `internal/protocol/protocol_test.go` | 163 | 6 | 二进制读写越界不 panic、全部消息往返、快照人数上限、截断消息报错 |
| `internal/protocol/snapshot_test.go` | 70 | 2 | 快照解码的分配语义（DecodeSnapshot 必分配 / Into 零分配）与缓冲复用不残留旧帧 |
| `internal/runner/runner_test.go` | 142 | 1 | **四个产物都能被对应解析器读进去**（result.json 是 JSON、plan.yaml 是 YAML、bots.jsonl 逐行 JSON） |
| `internal/sched/sched_test.go` | 213 | 10 | 五种形状单调性、爬坡/尖峰速率、不可达报错、**派发不等待**、ctx 取消、抖动不改总数 |
| `internal/wire/wire_test.go` | 150 | 7 | 帧往返、半包截断、超大帧、短写、信封 |
| `internal/ws/ws_test.go` | 845 | 18 | 握手成功/失败、长度边界 125/126/65535、分片、掩码、ping-pong、close、超限、并发写、wss |
| `srv/account/account_test.go` | 195 | 8 | 重登让旧 token 失效且不误伤别人、过期拒绝与摊还清理、Elo 上下界，以及新旧登录算法对照基准 |
| `srv/gateway/queue_test.go` | 51 | 1 | 入队参数校验：模式不匹配、组队请求被**明确拒绝**而不是静默忽略 |
| `srv/room/room_test.go` | 186 | 4 | **命中能击杀**、超距不命中、三类权威校验、结算与生命周期 |
| `srv/sut/client_e2e_test.go` | 167 | 1 | 真人客户端公开 API：两个客户端打同一局，**结算 tick/结果一致**（守住结算与关连接的竞态） |
| `srv/sut/e2e_test.go` | 226 | 2 | **进程内起整套 SUT 跑完一局并断言有击杀**、房间回收、指标端点格式 |

## 6. 包级依赖方向

依赖是单向的，没有环。**这条是这套代码能被一个人维护的前提**——改协议层不会牵动观测层。

```
cmd/*  ──►  srv/sut  ──►  srv/{gateway, match, alloc, room, store, config}
                  │                │
                  │                ├──► internal/protocol
                  │                ├──► internal/wire
                  │                └──► internal/obs ──► internal/metrics
                  │
  cmd/runner  ──► internal/runner ──► internal/bot ──► {ws, wire, protocol, behavior, report, sink, runstats}
                        │                  └──► internal/{plan, sched, obs, hist}
                        └──► internal/controller（仅 cmd 层组合，不反向依赖）

  internal/ws      不依赖任何内部包（纯标准库）
  internal/wire    不依赖任何内部包
  internal/hist    不依赖任何内部包
  internal/metrics 不依赖任何内部包
```

四个「叶子包」（ws / wire / hist / metrics）都是零内部依赖、零第三方依赖，可以整包拷到别的项目里用。

## 7. 建议阅读顺序

按「先看清骨架，再看肌肉」的顺序，一条线读下来大约 2 小时：

| 顺序 | 读什么 | 为什么先读它 |
|---|---|---|
| 1 | [`README.md`](../README.md) | 先知道这东西想证明什么，否则读代码会迷路 |
| 2 | [`internal/sched/sched.go`](../internal/sched/sched.go) | 开环 vs 闭环是整项目的题眼，200 行看完 |
| 3 | [`internal/bot/bot.go`](../internal/bot/bot.go) | 一条 bot 生命周期的全貌，顺带理解所有失败路径 |
| 4 | [`srv/room/room.go`](../srv/room/room.go) | 靶子的核心：tick、权威裁决、结算、回收 |
| 5 | [`srv/sut/sut.go`](../srv/sut/sut.go) | 四层怎么拼起来的，看这一个文件就够 |
| 6 | [`internal/hist/hist.go`](../internal/hist/hist.go) + [`internal/report/reporter.go`](../internal/report/reporter.go) | 为什么 p99 是这个数 |
| 7 | [`docs/FINDINGS.md`](FINDINGS.md) | 8 个真实缺陷，比读代码更快理解「哪些地方容易错」 |

想直接上手跑，看 [`README.md`](../README.md) 的「快速开始」；想改一处看指标怎么动，改 [`deploy/sut.yaml`](../deploy/sut.yaml) 里的开关。

