# AI 玩家集群 / 对局压测平台

用 bot 玩家模拟真人行为，压房间服与匹配器。**自建可压的后端 + 协议级 bot 集群 + 分层观测**，
按《AI 玩家集群-对局压测·技术栈与单人简历项目方案》与《全栈知识点清单》的 A–K 模块落地。

```
┌─ 控制面 ─────────────────────────────────────────────
│  controller (HTTP API)  →  runner 池  →  results/*.jsonl
├─ 行为面 ─────────────────────────────────────────────
│  persona 分布 · 会话状态机 · 双峰思考时间 · 危险率退出 · 非法输入注入
├─ 协议面 ─────────────────────────────────────────────
│  gateway: WebSocket + 信封(msgID+seq) + JSON   ← 登录/排队/匹配
│  room   : 裸 TCP + 长度前缀 + 定长二进制        ← 10Hz 输入/快照/结算
├─ 观测面 ─────────────────────────────────────────────
│  Prometheus 文本暴露 · HdrHistogram(CO 修正) · JSONL → DuckDB
└──────────────────────────────────────────────────────
   ▼ 被测系统 sutd：登录 → 匹配器 → 分配器(Redis 热池) → 房间服(10Hz tick) → 结算写库
```

## 快速开始（Windows / PowerShell，2 条命令）

```powershell
# 一条命令跑完：编译 → 起 SUT → 打 30 个 bot → 出分析报告与 CDF 图
.\scripts\smoke.cmd
```

**想自己进去打一局？**

```powershell
# 一条命令：起被测系统 → 起陪练 bot → 你连进去玩（1v1，几秒就能排上）
.\scripts\demo.cmd
```

操作：`WASD` 移动、`空格` 攻击、`Q` 退出。打完会提示是否再来一局，退出时后台进程自动清理。
真人数据会落到 `results/human-<账号>/`，可以直接拿去和 bot 的分布做 KS 检验（见下）。

手工分步（想看着跑）：

```powershell
go build -o bin/sutd.exe ./cmd/sutd ; go build -o bin/runner.exe ./cmd/runner

# 终端 1：起被测系统（冒烟配置：一局 10 秒，瓶颈全关）
.\bin\sutd.exe -config deploy/sut.smoke.yaml

# 终端 2：跑 30 个 bot
.\bin\runner.exe -plan plans/smoke.yaml -metrics :9091
```

> 直接跑 `scripts\smoke.ps1` 若提示「未对文件进行数字签名」，用 `smoke.cmd`，
> 或执行一次 `powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\smoke.ps1`。

结果在 `results/<run-id>/`：`bots.jsonl`（每 bot 一行）、`result.json`（分位数汇总）、
`plan.yaml` / `meta.json`（实验快照，三个月后还能原样重跑）、`analysis/report.md` 与 `cdf.svg`。

全量自检：

```powershell
go build ./... ; go vet ./... ; go test ./... -count=1 -timeout 300s
```

## 真人客户端

`cmd/client` 是一个能真的坐下来玩的终端客户端：`WASD` 移动、空格攻击、10Hz 战场与记分板、
心跳保活、退出复位控制台。它有三种模式：

| 模式 | 命令 | 用途 |
|---|---|---|
| 交互 | `client -account me` | 人玩；stdout 是终端时自动用全屏界面 |
| 逐行 | `client -plain` | 输出被重定向、或终端不支持 ANSI 时自动降级 |
| 无人值守 | `client -auto -matches 3` | CI 里跑通整条协议路径（**不是**保真度数据） |

**为什么要它。** 项目里其它流量都是 bot 造的，而 bot 与服务器由同一套假设写出来——
「两边一起错」是最难发现的失败模式。真人客户端复用同一套 `wire`/`protocol`，
却由完全不同的代码路径驱动（人按键 → 输入 → 服务端权威裁决），所以它能连上、能打完，
等于给协议层做了一次独立的符合性验证。它也真的抓到过 bug：第一版客户端把「打完的对局」
记成了掉线（详见 [`docs/FINDINGS.md`](docs/FINDINGS.md) F9）。

**它补齐了保真度闭环。** 之前只有 bot 自己的分布，没有参照物；现在真人数据可以直接当参照：

```powershell
# 1. 先跑一轮 bot（例：冒烟档）
.\scripts\smoke.cmd

# 2. 自己玩几局，产生 results/human-me/sessions.json
.\scripts\demo.cmd -Account me

# 3. 用真人数据做参照，检验 bot 的会话时长分布像不像
python analysis\analyze.py results\<bot-run> --reference results\human-me\sessions.json
```

注意：`sessions.json` 里是**对局时长**（秒），不是完整会话时长——要算「一次坐下玩多久」
需要连续多局并在同一账号下累计。`demo.cmd` 默认只打一局，多打几局用 `-matches 5`
（在 `client` 上）或直接多玩几轮。

## 指标表

| 指标 | 定义 | 目标 | 实测（冒烟档：30 bot / 一局 10s） |
|---|---|---|---|
| 峰值活跃 bot | 稳定不掉线 | ≥ 3000 | 30（冒烟档，非上限） |
| 有效对局吞吐 | 每分钟**打完**的局数 | ≥ 120 局/分 | 45.8 局/分 |
| 对局结果分布 | 胜 / 负 / 平 | — | 13 / 11 / 2 |
| 匹配 p99 | 入队 → 匹配成型 | < 8s | 5.1s |
| 分配 p99 | 分配器找到房间 | < 300ms | < 1ms（池子充足时） |
| 房间 tick 超预算率 | 超 16ms 的 tick 占比 | < 0.1% | 0.00%（冒烟档瓶颈全关） |
| 输入校验 | 非法输入被拒 / 注入数 | 100% | 101 / 101 |
| 行为保真度 | 会话时长 KS / Wasserstein | p > 0.05 | 冒烟 persona 是故意做短的，**不能**用来看保真度 |
| 缺陷产出 | 压测发现并修复的问题 | ≥ 5 | **14**（见 `docs/FINDINGS.md`，含 3 个并发/数据丢失类竞态） |

> 复现命令与逐项对账见 `docs/FINDINGS.md`。**别信这张表，信 `results/` 里的原始数据。**
> 冒烟档的意义是「一条命令验证整条链路」，性能数字要看 `plans/baseline.yaml` 与
> `plans/peak.yaml`，且必须用 `deploy/sut.yaml`（带刻意瓶颈）跑一遍。

## 代码地图

| 路径 | 职责 | 对应知识点 |
|---|---|---|
| `internal/wire` | 长度前缀 + 消息号 + 序列号的二进制帧；WS 信封 | A6 |
| `internal/ws` | 零依赖 RFC6455 实现（握手/分片/掩码/ping-pong/close） | A5 |
| `internal/protocol` | 两段应用协议：控制面 JSON、数据面定长二进制 | A7 D1 |
| `internal/clock` | 绝对时间表固定步长调度 + 帧预算 | B9 D2 |
| `internal/sched` | **开环**到达时间表（constant/ramp/step/spike/diurnal） | C2 C3 E5 |
| `internal/behavior` | persona 分布、双峰思考时间、危险率退出、可复现 PCG | E6 E7 J2 |
| `internal/hist` | HdrHistogram + coordinated-omission 修正 | C3 C4 |
| `internal/metrics` | Prometheus 文本暴露（自实现，零依赖） | G2 G3 G4 |
| `internal/obs` | 全平台指标清单（单一事实来源） | G5 |
| `internal/report` | 双路采集：Prometheus 桶 + HdrHistogram | C4 |
| `internal/bot` | 会话状态机：连接/握手/鉴权/排队/进房/对局/重连/结算；对外公开玩家会话 API | E4 E8 D6 |
| `internal/play` | 真人客户端的终端界面与键盘：TUI/逐行双模、Windows 原始键盘 | A5 |
| `cmd/client` | 真人客户端：交互 / 逐行 / 无人值守，记录真人数据供保真度对照 | C12 |
| `internal/sink` | JSONL 落盘（带 CO 修正线索的阶段记录） | E9 H1 |
| `internal/runner` | runner 池、信号量背压、结果汇总 | B10 E10 |
| `internal/controller` | 控制面 HTTP API（6 个端点） | §2.3 |
| `srv/gateway` | WS 接入层：握手、签名鉴权、排队、匹配推进 | A8 D6 |
| `srv/match` | MMR 桶 + 窗口放宽 + 不满开局 | D9 D10 D13 |
| `srv/alloc` | 分配器（**故意保留 Redis 热键 + 全局锁**） | D5 |
| `srv/room` | 房间生命周期、10Hz tick、权威裁决、回收 | D1–D4 D7 |
| `srv/store` | Redis 模拟（热分片）+ 结算同步写库 | §2.1 |
| `srv/sut` | 四层组装（被 `sutd` 与端到端测试共用） | K3 |
| `plans/` | 负载计划与 persona 目录（配置即实验） | K1 E6 |
| `analysis/` | 分布拟合、KS 检验、报告出图 | C12 C13 H7 |
## 三个「故意留下的瓶颈」

**压测平台的价值不在于能起多少连接，而在于能定位瓶颈。** 所以被测系统刻意留了三个可被压垮的点，
并且全部做成配置开关，可以在同一个二进制里跑「修复前 / 修复后」对照：

| 瓶颈 | 开关（`deploy/sut.yaml`） | 症状 |
|---|---|---|
| 空闲房间池落在单个 Redis 热分片 | `alloc.pool_hot_key: true` | `room_alloc_seconds` p99 台阶式上升，`room_alloc_miss_total` 同步涨 |
| 分配器全局锁 | `alloc.global_lock: true` | 分配吞吐被锁死在 `1/持锁时间`，与房间数无关 |
| 结算在 tick goroutine 里同步写库 | `settlement.fsync_ms: 2.0` | `room_tick_overrun_total` 涨、`room_tick_lag_seconds` 涨 |

对照实验：

```powershell
.\bin\sutd.exe -config deploy/sut.yaml          # 带瓶颈
.\bin\sutd.exe -config deploy/sut.fixed.yaml    # 修复版
# 两次都用同一份 plans/baseline.yaml，对比 results/*/result.json 的 histograms
```

## 为什么 bot 不能由 LLM 在线控制

1 万 bot × 5% × 每分钟 1 次 × 800 token ≈ 40 万 token/分钟 ≈ 2400 万 token/小时；
全量则是 4.8 亿/小时。加上延迟抖动与不可复现，三条全占。
**正确姿势：LLM 离线造玩家（persona / 用例 / 异常路径），引擎在线跑玩家。**
本仓库只做了「引擎在线跑玩家」这一半——LLM 那一半（离线生成 persona、2% 在线 pilot）
留在路线图里，因为它的成本模型和降级链路才是真正需要单独设计的东西。

## 刻意不做（和上面的「做」一样重要）

- 完整游戏（战斗数值、技能、地图寻路）：房间只做到「移动 + 近战命中 + 结算」，够压就行。
- 1 万并发：本地跑到数千再谈云上，`plans/` 里的目标值都是本地可复现的。
- k8s / Agones：单机 `docker compose` 到底，编排是放大器不是起点。
- 全量 RL / 行为克隆：只做「参数化 bot」，这是压测项目里唯一站得住的位置。
- 在线全量 LLM 控制：见上一节。
- 美术、UI、音效：零。

## 已知限制（写给自己和面试官）

- 房间状态同步是**全量快照**，没有 AOI 兴趣管理与增量压缩（D3 只做了最简单的形态）。
- **组队匹配未实现**：协议里保留了 `party` 字段，但接入层会明确拒绝 `party` 长度 > 1 的请求
  （`party_unsupported`）。这是刻意选择的——静默忽略比如实报错恶劣得多，
  见 `docs/FINDINGS.md` F12。D14 仍未完成。
- `bot_phase_seconds` 是「从应到时刻算起」的累计耗时，与直觉上的「阶段耗时」不同，
  见 `docs/METRICS.md` 第 3 节。JSONL 里同时给了 `offset_ms` 与 `dur_ms`。
- 控制器单进程、实验串行：并发跑两个实验会互相污染指标。
- 真人客户端的对局时长是**墙钟时间**，机器满载时会因调度滞后而长于 `ticks × tick_rate`；
  拿它当保真度参照时，最好在空载机器上玩，或者只比「输入间隔」这类与调度无关的量。
- 真人客户端只支持 Windows 原始键盘模式；其他平台自动降级为逐行输入。

## 文档

- [`docs/CODE-MAP.md`](docs/CODE-MAP.md)：**每个源文件干什么**（行数与导出符号现场扫描生成，不会过期）
- [`docs/HANDWRITE.md`](docs/HANDWRITE.md)：**必须自己手写的部分**（从两份文档提取的 12 项白板代码、
  11 项必背数字、8 项自写文字、24 题自检 + 9 问答案骨架）
- [`docs/SLO.md`](docs/SLO.md)：四层 SLO 与业务口径
- [`docs/METRICS.md`](docs/METRICS.md)：每个指标的定义、标签、PromQL
- [`docs/FINDINGS.md`](docs/FINDINGS.md)：14 个压测发现的问题与修复前后对比
- [`analysis/README.md`](analysis/README.md)：保真度分析与报告出图

## 许可

[MIT](LICENSE)。文档里的方案与知识清单来自项目内部材料，代码为原创实现。

## 已知依赖

只有一个第三方依赖（`gopkg.in/yaml.v3`）。WebSocket、HdrHistogram、Prometheus 文本暴露
都是自己写的——不是为了炫技，而是这几处正好是知识清单里的「必会」项，
抄一个库等于放弃那部分的可解释性。
