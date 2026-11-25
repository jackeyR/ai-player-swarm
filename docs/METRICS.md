# 指标定义（单一事实来源）

> 代码里的定义在 [`internal/obs/obs.go`](../internal/obs/obs.go)。
> **这个文件与代码必须一起改**；任何一边单独改动都算 bug。

## 0. 三条纪律

1. **个体 ID 不进标签。** `bot_id` / `player_id` / `room_id` 是无界基数，
   进标签会让 Prometheus 的倒排索引先崩掉（G5）。个体信息只进 `results/*/bots.jsonl`。
2. **分位数只认 histogram。** summary 的分位数不可跨实例聚合，所以统一用 histogram（G3）。
3. **所有延迟都要区分「原始」与「CO 修正后」。**
   修正后的直方图 `count` 不是观测次数，真实观测次数见 `result.json` 的 `samples`。

## 1. 被测系统（SUT）侧 —— 方案 §2.8 规定的 6 个必埋指标

| 指标 | 类型 | 标签 | 含义 | 目标 |
|---|---|---|---|---|
| `match_wait_seconds` | histogram | `mmr_bucket` | 入队 → 匹配成型 | p99 < 8s |
| `room_alloc_seconds` | histogram | `result`(pool/miss/error) | 分配器找到房间 | p99 < 300ms |
| `room_tick_total` / `room_tick_overrun_total` | counter | — | tick 超预算率的分子分母 | 超预算 < 0.1% |
| `session_total` / `session_errors_total` | counter | `result` / `code` | 会话级错误率 | < 0.5% |
| `reconnect_total` / `reconnect_success_total` | counter | — | 重连成功率 | > 99% |
| `room_leak_gauge` | gauge | — | 该回收却没回收的房间 | 恒为 0 |

`mmr_bucket` 是 MMR 的 6 档低基数映射（`lt900` / `900_1199` / `1200_1499` /
`1500_1799` / `1800_2099` / `ge2100`）。**按桶看是必须的**：整体 p99 会掩盖冷门分段的饥饿（D13）。

## 2. SUT 侧其余指标

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `login_total` / `login_errors_total` | counter | `result` / `code` | 接入层登录 |
| `queue_join_total` / `queue_cancel_total` | counter | `mode` | 排队与取消（取消率过高说明体验崩了） |
| `matches_created_total` | counter | `mode` | 成型局数 |
| `match_immediate_total` | counter | `mode` | 没凑齐就开局的次数（冷启动信号） |
| `match_mmr_spread` | histogram | `mode` | 一局内 MMR 极差，匹配质量代理指标（D11） |
| `rooms_running` / `rooms_created_total` / `rooms_closed_total` | gauge/counter | `mode`/`why` | 房间总量 |
| `room_tick_duration_seconds` | histogram | — | 单帧耗时（D2） |
| `room_tick_lag_seconds` | histogram | — | 理论 tick 时刻与实际触发时刻之差；涨说明进程被挤占 |
| `room_input_total` / `room_input_rejected_total` | counter | `code` | 权威裁决的接受与拒绝 |
| `room_snapshot_sent_total` / `room_snapshot_dropped_total` | counter | — | 状态同步与背压丢包（A14） |
| `room_fill_timeout_total` | counter | — | 等人进房超时 |
| `settle_write_seconds` | histogram | `result` | 结算写库耗时 |
| `players_online` / `matches_active` | gauge | — | 在线量 |

## 3. Bot 集群侧

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `bot_arrivals_total` | counter | `persona` | 调度器起量的个数 |
| `bot_schedule_lag_seconds` | histogram | — | **实际唤起**相对**应到时刻**的滞后。这是「bot 先成为瓶颈」的第一信号 |
| `bot_phase_seconds` | histogram | `phase` | 从应到时刻到各里程碑的累计耗时（已 CO 修正） |
| `bot_sessions_total` | counter | `persona`,`outcome` | 会话结果分布 |
| `bot_inputs_sent_total` / `bot_invalid_inputs_total` | counter | `persona` | 输入量与非法输入注入量 |
| `bot_inputs_rejected_total` | counter | `code` | 服务端拒绝了哪些输入（out_of_range / rate_limited / bad_tick） |
| `bot_disconnects_total` / `bot_reconnect_total` | counter | `persona` / `outcome` | 断线与重连 |
| `bot_snapshots_total` | counter | — | 收到的快照数；与 SUT 侧 `room_snapshot_sent_total` 对不上就是丢包 |
| `bot_heartbeat_rtt_seconds` | histogram | — | 应用层心跳 RTT |
| `bot_active` | gauge | `run` | 当前并发 bot 数 |
| `bot_goroutines` | gauge | — | goroutine 数（漏 goroutine 一眼看出，B1） |

`bot_phase_seconds` 的 `phase` 取值：`connect` / `handshake` / `login` / `queue_wait` /
`room_join` / `match_start` / `match_end`。

## 4. 关键 PromQL

```promql
# 匹配等待 p99（按 MMR 桶）—— 方案 §2.8 原样
histogram_quantile(0.99,
  sum by (le, mmr_bucket) (rate(match_wait_seconds_bucket[5m])))

# 分配 p99（区分走池子还是现建）
histogram_quantile(0.99,
  sum by (le, result) (rate(room_alloc_seconds_bucket[5m])))

# tick 超预算比例
sum(rate(room_tick_overrun_total[5m])) / sum(rate(room_tick_total[5m]))

# 会话错误率
sum(rate(session_errors_total[5m])) / sum(rate(session_total[5m]))

# 重连成功率
sum(rate(reconnect_success_total[5m])) / sum(rate(reconnect_total[5m]))

# 空房泄漏（应恒为 0；非 0 就是 D4 的 bug）
room_leak_gauge

# bot 端调度滞后 p99：涨了说明 bot 自己先扛不住了
histogram_quantile(0.99, sum by (le) (rate(bot_schedule_lag_seconds_bucket[5m])))

# 连接是否有实际在打（避免「连上了但房间没开」的假象）
sum(rate(room_snapshot_sent_total[1m])) / sum(rate(room_tick_total[1m]))
```

## 5. 为什么 p99 和别人的不一样（面试第 9 题）

- 我们**开环**：到达时间表是绝对时刻，慢响应不会让后续请求自动减速。
- 我们**用 HdrHistogram 并做 CO 修正**：以平均到达间隔为 expected interval 补记样本。
- 因此同一次事故，闭环 + 平均值的工具会报 `p99 = 200ms`，我们报 `p99 = 3.2s`。
  **不是我们的数字更差，是别人的数字把延迟藏起来了。**
