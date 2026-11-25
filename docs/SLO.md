# SLO 文档

> 没有 SLO 的压测只能得出「整体很慢」这一句废话（方案 §1.1 第三条红线）。
> 这份文件是验收的唯一锚点：每层各自独立，任何一条不达标都要能定位到具体层。

## 1. 四层 SLO

| 层 | SLI（指标） | SLO | 数据来源 | 破了先看什么 |
|---|---|---|---|---|
| **登录** | `login_errors_total / login_total` | 错误率 < 0.5% | gateway | 鉴权签名、连接数上限、fd |
| | `bot_phase_seconds{phase="login"}` p99 | < 1s | bot | TLS/握手、DNS、accept 队列 |
| **匹配** | `match_wait_seconds` p99 | < 8s | matchmaker | 窗口放宽策略、在线量、分桶饥饿 |
| | `match_immediate_total` 占比 | < 5% | matchmaker | 冷启动/低在线，考虑 AI 补位 |
| **分配** | `room_alloc_seconds` p99 | < 300ms | allocator | 池子容量、Redis 热键、全局锁 |
| **房间** | `room_tick_overrun_total / room_tick_total` | < 0.1% | room | 同步写库、锁竞争、GC、快照包量 |
| | `room_tick_lag_seconds` p99 | < 20ms | room | 进程被挤占（CPU 配额、GC） |
| **会话** | `session_errors_total / session_total` | < 0.5% | room+gateway | 心跳超时、房间回收、重连 |
| | `reconnect_success_total / reconnect_total` | > 99% | roomnode | token 有效期、房间是否已回收 |
| **回收** | `room_leak_gauge` | 恒为 0 | janitor | janitor 逻辑、空房判定、TTL |

## 2. 业务口径（报告首页那张表）

| 指标 | 定义 | MVP 目标 |
|---|---|---|
| 峰值活跃 bot | 稳定跑 30 分钟不掉线 | ≥ 3000 |
| 有效对局吞吐（goodput） | 每分钟**真正打完**的局数（不是连接数） | ≥ 120 局/分 |
| 行为保真度 | 会话时长 KS 检验 / Wasserstein 距离 | p > 0.05 / < 10% 中位数 |
| 控制器开销占比 | bot 端 CPU / 总 CPU | < 8% |
| 缺陷产出 | 压测发现并修复的有效问题 | ≥ 5 个 |

> goodput 从 `results/<run-id>/result.json` 的 `stats.matches_completed` 直接读，
> 不从 Prometheus 求和——它是业务口径，必须能被复算。

## 3. 错误预算怎么用

以 tick 超预算率 0.1% 为 SLO 为例：一个 30 分钟的实验有 `30*60*10*N` 次 tick 预算，
超预算次数超过 0.1% 即视为「当次实验判失败」，不是「平均值还行就通过」。
本项目的做法是把每次实验的失败原因写进 `docs/FINDINGS.md`，而不是只留一张好看的图。

## 4. 分层压测的顺序（别跳步）

```
登录 → 匹配 → 分配 → 房间
```

每次只压一层时把其余层的负载压低（改 `plans/*.yaml` 的 `traffic`），
否则你永远在「整体很慢」里打转。这也是 `plans/` 里同时放了 smoke / baseline / peak 的原因。
