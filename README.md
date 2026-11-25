# AI 玩家集群 / 对局压测

用 bot 玩家模拟真人行为，去压房间服和匹配器。

最初只是为了回答一个问题：**一局对局从排队到结算，到底在哪一步开始变慢**。
所以顺序反过来了——先写一个能压的最小游戏后端，再写能跑满它的 bot 集群，
最后补一个真人能坐进去打的终端客户端。最后这个是必需的：只有真人能证明
这套协议不是「bot 和服务端一起错」。

Go 写的，除 YAML 解析没有第三方依赖，Windows 上两条命令就能跑。

```
控制面   controller → runner 池 → results/*.jsonl
协议面   gateway（WebSocket，登录/排队/匹配）  room（裸 TCP，10Hz 输入/快照/结算）
行为面   persona 分布 · 会话状态机 · 双峰思考时间 · 危险率退出 · 非法输入注入
观测面   Prometheus 文本暴露 · HdrHistogram（CO 修正） · JSONL
           ↓ 被测系统 sutd：接入层 → 匹配器 → 分配器 → 房间服 → 结算写库
```

## 先跑起来

```
.\scripts\smoke.cmd
```

编译、起被测系统、打 30 个 bot、出一份分析和 CDF 图，四十秒左右，不需要 Docker。

想自己进去打一局：

```
.\scripts\demo.cmd
```

它会起一个 1v1 的环境、拉几个陪练 bot，然后把你丢进队列，通常几秒就能排上。
WASD 移动、空格攻击、Q 退出，打完会问要不要再来一局，退出时后台进程自动清理。

（如果提示「未对文件进行数字签名」，用 `.cmd` 那个包装，或者
`powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\smoke.ps1`。）

想分步看着跑：

```powershell
go build -o bin/sutd.exe ./cmd/sutd
go build -o bin/runner.exe ./cmd/runner
.\bin\sutd.exe -config deploy/sut.smoke.yaml     # 终端 1
.\bin\runner.exe -plan plans/smoke.yaml           # 终端 2
```

自检就一句 `go test ./...`。17 个包，其中一个是进程内起整套服务的端到端测试。

## 它由什么组成

被测系统是 `sutd`，一个进程里塞了四层：接入层、匹配器、分配器、房间服。
放一个进程里纯粹为了一个人好调试，真实集群里这四层会分开部署。
但指标是按层报的——登录、匹配、分配、房间各有各的分位数，混在一起压只会得到「整体很慢」。

协议面分成两段，这个选择值得单独说。登录、排队、匹配是低频、要能翻日志查的，
走 WebSocket + JSON，外面套一层「消息号 + 序列号」的信封；对局里每秒十次输入和快照，
走裸 TCP + 长度前缀的定长二进制，一个输入帧二十字节。一开始我也想全用 WebSocket，
后来发现每次全量快照还要多背一层 HTTP 语义，才拆开的。

bot 那一侧基本都在围绕「别再骗自己」这件事：

到达时刻用的是绝对时间表，不是「做完一个再做下一个」。所有延迟从应到时刻算起，
而不是从真正开始的时刻算起——探针迟到了是系统的锅，不能让探针自己把这段藏起来。
分位数用 HdrHistogram，并且按平均到达间隔做了 coordinated omission 修正。
这三件事加起来才对得上账：同一个事故，我这边的 p99 是三秒，而别人的工具报两百毫秒。

观测是自写的 Prometheus 文本暴露加 HdrHistogram 两路。看板用桶（能聚合），
报告用高精度（能跨进程合并），同一次测量各喂一份。

## 真人客户端

`cmd/client` 是后来补的，起因不是「想做个游戏」，是想给自己找个参照物。

项目里其它流量都是 bot 造的，而 bot 和服务端是同一套假设写出来的，一起错最难发现。
真人客户端复用同一套 wire/protocol，但由完全不同的路径驱动（人按键 → 输入 → 服务端裁决），
它能连上、能打完，协议层就等于被独立验证过一次。

它确实抓到过东西。第一版把一局打完的对局记成了「掉线」：服务端先发结算帧再关连接，
客户端 select 里两个分支同时就绪，随机选了关闭那个。同一个坑我在 bot 里修过一次，
写客户端时又踩了一遍。第二次我没有再复制那段补丁，而是把它做成了 `TrySettle()` 这个方法。

顺带它把保真度的闭环补上了。以前只有 bot 自己的分布，没有参照物：

```powershell
.\scripts\smoke.cmd                              # 先跑一轮 bot
.\scripts\demo.cmd -Account me                   # 自己玩几局，数据落到 results/human-me/
python analysis\analyze.py results\<bot-run> --reference results\human-me\sessions.json
```

`sessions.json` 里存的是对局时长。要算「一次坐下玩多久」得连续多局、同一账号累计，
所以这里只比单局，别拿它当完整会话分布。

## 压出来的东西

冒烟那轮是 30 个 bot、34 秒，打完 26 局，折合每分钟 45.8 局；匹配 p99 5.1 秒；
注入的 101 条非法输入全部被服务端按 out_of_range 拒掉；每局结束后房间都被回收，泄漏计数为 0。

这些数字只说明链路是通的，不代表性能——冒烟档把对局压到 10 秒，而且刻意瓶颈全关。
要看真实形状用 `plans/baseline.yaml`（阶梯到 300 bot）和 `plans/peak.yaml`（两次尖峰），
配 `deploy/sut.yaml` 跑。

被测系统里我故意留了三个能压垮的点，全部做成配置开关，所以「修复前后对比」
是改配置而不是改代码：

- 空闲房间池全落在一个 Redis 分片上（`alloc.pool_hot_key`）。症状是分配 p99 台阶式上升，
  同时 `room_alloc_miss_total` 跟着涨。
- 分配器一把全局锁（`alloc.global_lock`）。分配吞吐会被锁死在 1/持锁时间，加房间也没用。
- 结算在 tick goroutine 里同步写库，还模拟 2ms 的 fsync（`settlement.fsync_ms`）。
  症状是 tick 超预算率涨，调度滞后跟着涨。

`deploy/sut.fixed.yaml` 是三个都关掉的版本。同一份计划各跑一次就能出对照曲线。

## 踩过的坑

`docs/FINDINGS.md` 里记了 15 个，每个都是「现象 → 怎么定位 → 根因 → 修复 → 验证」的格式。
挑三个说：

**所有对局都是 0:0 平局。** 链路全绿、指标有数，但胜负逻辑一行都没执行。
出生点相距六千多单位、移动速度 1.5m/s，一局十秒根本走不到一起；再加上 bot 的双峰操作间隔
里间歇段是三到十秒，人还没走过去就结束了。改了出生点距离、提了速度、加了追击逻辑，
并且把「这局有没有击杀」写进了端到端断言——只断言「跑完一局」远远不够。

**一局打完了，却被记成掉线。** 服务端先发结算帧再关连接，客户端 select 随机选了关闭那个分支。
同一个坑修过两次：第一次修在 bot 里，写真人客户端时又复发了。

**CI 抓到的跨平台 bug。** 修完「指纹不能含机器路径」之后本地全绿，推上去 Linux 作业挂了：
`filepath.Base` 在 Linux 上不认反斜杠，Windows 风格的路径取不到 basename，指纹还是不一致。
这类问题本机永远测不出来，得有第二个平台真的跑一次。

## 没做完的

组队匹配没做。协议里留着 `party` 字段，但服务端会明确拒绝长度大于 1 的请求，
而不是静默按单人排队——后者比报错恶劣得多。

房间同步是全量快照，没有 AOI 和增量压缩。十人房够用，五十人房带宽会炸。

控制器是单进程、实验串行的。同时跑两个实验会互相污染指标，这属于不作为而不是 bug。

真人客户端只有 Windows 有原始键盘模式，其他平台自动降级成逐行输入。

还有一些是主动不做的：完整游戏逻辑、一万并发（本地压不出来）、k8s/Agones、全量 RL。
判断标准就一条：一个功能如果不能出现在指标表或者 demo 里，就不做。

## 目录

```
cmd/        sutd / runner / controller / client 四个可执行
internal/   协议、调度、行为、bot、观测、运行与配置
srv/        被测系统：接入层 / 匹配 / 分配 / 房间 / 存储模拟
plans/      负载计划与 persona 目录（配置即实验，进版本库）
deploy/     SUT 配置、Prometheus 规则、Grafana 看板、compose
analysis/   离线分析：KS 检验、Wasserstein 距离、CDF 出图
docs/       SLO、指标定义、FINDINGS、源码清单、手写清单
scripts/    smoke / demo
```

每个源文件干什么，`docs/CODE-MAP.md` 里有一张现场扫描生成的表，行数和导出符号不会过期。

## 文档

- [docs/SLO.md](docs/SLO.md) 四层 SLO 与业务口径
- [docs/METRICS.md](docs/METRICS.md) 每个指标的定义、标签和 PromQL
- [docs/FINDINGS.md](docs/FINDINGS.md) 15 个问题
- [docs/CODE-MAP.md](docs/CODE-MAP.md) 源码清单
- [docs/HANDWRITE.md](docs/HANDWRITE.md) 我必须能手写的部分（自己的复习清单，不感兴趣可以跳过）
- [analysis/README.md](analysis/README.md) 离线分析怎么用

## 许可

MIT。代码是我写的；项目参照了一份内部的技术方案与知识清单，那两份材料没放进仓库。
