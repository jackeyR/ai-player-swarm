# 离线分析

压测跑完之后，数据在这里变成「结论」。三步：

```bash
# 1. 生成报告与 CDF 图（只依赖标准库）
python analysis/analyze.py results/<run-id>

# 2. 有参照数据时做保真度检验
python analysis/analyze.py results/<run-id> --reference analysis/reference.json

# 3. 一次汇总多个 run（对照组 / 修复前后）
python analysis/analyze.py results/run-a results/run-b
```

产出写在 `results/<run-id>/analysis/`：

| 文件 | 内容 |
|---|---|
| `report.md` | 分层分位数表、会话时长分布、KS / Wasserstein 检验、结果分布、persona 维度 |
| `cdf.svg` | 会话时长 CDF 对比图（手写 SVG，可直接贴进 README） |

## 为什么不用 scipy / matplotlib

- **KS 与 Wasserstein 在一维下都有闭式做法**，自己实现二十行，还顺带能在面试里讲清
  「我算的到底是哪个统计量」；
- 手写 SVG 没有字体与后端依赖，GitHub、VS Code、浏览器都能直接看；
- 这份脚本要能在任何一台装了 Python 3.8+ 的机器上跑起来（CI、面试现场、公司电脑）。

如果你确实要更丰富的图（QQ 图、对数坐标、热力图），`pip install matplotlib`
之后另写一个脚本即可，别把主分析链路绑死在重依赖上。

## 参照数据从哪来

没有真实遥测时，别编数字。两条可行路径：

1. **公开数据**：OpenDota 的对局时长分布、SteamDB 的同时在线曲线，
   足以定出「中位数 25–35 分钟、长尾到 2 小时」这个形状；
2. **自采**：自己在真实游戏里录一段会话（哪怕只有几十条），比手写分布诚实得多。

`reference.example.json` 里放的是**占位数据**，只为演示流程，不要拿它写进报告。

## 口径对齐

`report.md` 的阶段延迟用 `offset_ms + dur_ms`，即「从应到时刻到该里程碑」的累计耗时。
这是开环压测下唯一有意义的口径（`docs/METRICS.md` 第 3 节）。
如果你把它当成「阶段耗时」来念，会得到「login 花了 40 秒」这种荒谬结论——那是调度滞后，不是登录慢。
