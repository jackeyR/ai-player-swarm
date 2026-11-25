#!/usr/bin/env python3
"""生成 docs/CODE-MAP.md —— 项目源码与组成清单。

为什么用生成器而不是手写：清单里的**行数、导出符号、测试函数**会随代码变化而失效，
手工维护的文档三个月后一定开始说谎。这里只把「每个文件干什么」写成人工描述，
其余事实全部现场扫描，所以重跑一次就永远与实际代码一致。

用法：
    python scripts/gen_codemap.py            # 输出 docs/CODE-MAP.md
    python scripts/gen_codemap.py --check    # 只校验：是否有文件缺少作用描述
"""

from __future__ import annotations

import os
import re
import sys

ROOT = "."
OUT = "docs/CODE-MAP.md"
SKIP_DIRS = {"results", "bin", ".git", "node_modules", "__pycache__", ".idea", ".vscode"}

# --------------------------------------------------------------------------- #
# 人工描述：每个文件「干什么」。键是仓库相对路径。
# --------------------------------------------------------------------------- #

ROLE = {
    # ===== 入口 =====
    "cmd/sutd/main.go": "被测系统的入口：解析参数 → 覆盖配置 → 组装四层 → 等信号优雅退出。真正的组装逻辑在 srv/sut，这里只做参数与打印。",
    "cmd/runner/main.go": "bot 集群单机入口：读负载计划 → 解析相对路径 → 建 runner → 跑到结束 → 打印分位数汇总。",
    "cmd/controller/main.go": "控制面入口：起 HTTP 服务，接受计划提交、查询、停止；退出时取消在跑的实验。",
    "cmd/client/main.go": "真人客户端：交互 TUI / 逐行 / 无人值守三模，记录真人数据（bots.jsonl + trace.jsonl + sessions.json）供保真度对照。",

    # ===== 协议层 =====
    "internal/wire/wire.go": "应用层二进制帧：裸 TCP 用「长度前缀 + 消息号 + 序列号」，WS 用无长度前缀的信封。含半包/短写/超大帧防护。",
    "internal/ws/ws.go": "RFC6455 基础定义：OpCode 常量、CloseError、ErrClosed / ErrBadHandshake / ErrFrameTooLarge。",
    "internal/ws/frame.go": "帧头编解码：7/16/64 位长度、FIN/RSV 位、掩码运算、close payload 解析。全是内部函数，不导出。",
    "internal/ws/conn.go": "连接本体：读写帧、分片重组、Ping 自动回 Pong、Close 状态机、并发写锁、幂等关闭。",
    "internal/ws/server.go": "服务端握手：校验 Upgrade/Version/Key、回 101 + Sec-WebSocket-Accept、Hijack 拿到底层 conn。",
    "internal/ws/client.go": "客户端 Dial：支持 ws/wss、自定义头、TLS、子协议，校验服务端握手响应。",
    "internal/protocol/protocol.go": "协议骨架：版本号、控制面/数据面消息号常量、MsgName、Error 载荷、信封 Marshal/Unmarshal。",
    "internal/protocol/gateway.go": "控制面消息体（JSON）：Hello/Welcome/Login/EnterQueue/MatchFound/Ping/Pong/Bye。",
    "internal/protocol/room.go": "数据面消息体（定长二进制）：Input/Snapshot/Settle/Join/Reconnect + 房间规则常量（速度、命中半径、伤害、血量）。",
    "internal/protocol/binbuf.go": "零依赖大端二进制读写器：越界只置错误位不 panic，字符串带 u8 长度前缀。",

    # ===== 调度与时间 =====
    "internal/clock/clock.go": "固定步长调度器（按绝对时间表推进、带滞后上报）+ 单帧预算计数器。对应知识清单 B9 / D2。",
    "internal/sched/sched.go": "开环到达调度：五种流量形状的到达时间表生成 + Dispatcher（准时喊人、不等人）。对应 C2 / C3 / E5。",

    # ===== 行为 =====
    "internal/behavior/behavior.go": "persona 模型：六种分布采样、双峰思考时间、退出危险率、可复现 PCG 随机、移动生成器。对应 E6 / E7 / J2。",
    "internal/behavior/defaults.go": "内置的 6 种玩家原型（休闲/新手/硬核/挂机/破坏者/组队）及其分布参数。",

    # ===== bot =====
    "internal/bot/bot.go": "bot 会话状态机：连接→握手→鉴权→排队(可取消)→匹配→进房→对局(可重连/秒退)→结算→再排。含非法输入注入与追击逻辑。",
    "internal/bot/client.go": "接入层客户端：WS 连接、Hello/Login 握手、排队请求、匹配推送接收；用 seq 做并发请求-响应配对。",
    "internal/bot/roomclient.go": "房间服客户端：TCP 连接、进房/重连、输入发送、快照/ack/结算接收，队列满时丢弃可丢帧。",
    "internal/bot/session.go": "对外公开的玩家会话 API（Player / RoomSession）：给真人客户端复用同一套协议实现，含 TrySettle 这类「默认正确」的竞态处理。",

    # ===== 客户端界面 =====
    "internal/play/view.go": "真人客户端的终端界面：TUI 全屏（战场网格 + 记分板 + 状态行）与逐行降级两套渲染。",
    "internal/play/keys.go": "按键事件与按键源接口，以及控制台字符到协议键位的归一化。",
    "internal/play/keys_windows.go": "Windows 控制台原始模式：关行缓冲/回显、开 ANSI，退出时恢复原模式（不恢复会让用户终端不再回显）。",
    "internal/play/keys_other.go": "非 Windows 平台的降级：明确返回「不支持原始键盘」，由调用方退回逐行输入。",

    # ===== 观测 =====
    "internal/hist/hist.go": "HdrHistogram：对数线性桶、CO 修正补记、Merge/Export/Buckets/快照 + 并发安全 Recorder。对应 C3 / C4。",
    "internal/metrics/metrics.go": "自实现的 Prometheus 指标库：Counter/Gauge/Histogram + 标签向量 + 文本暴露格式 + 运行时指标。对应 G2–G4。",
    "internal/obs/obs.go": "全平台指标清单（单一事实来源）：SUT 侧与 bot 侧全部指标名、标签、分桶；MMR 分桶映射。",
    "internal/report/reporter.go": "双路采集：把同一次测量同时喂给 Prometheus 桶与 HdrHistogram，并额外维护「真实观测次数」防止 CO 修正误导。",
    "internal/sink/sink.go": "结果落盘：并发安全的 JSONL 写入器 + 会话/运行汇总的记录结构（含阶段偏移与耗时）。",

    # ===== 运行与配置 =====
    "internal/runstats/runstats.go": "一次实验的跨 bot 原子计数（会话/对局/胜负/输入/断线），goodput 的业务口径来源。",
    "internal/plan/plan.go": "负载计划：YAML 加载、默认值、校验、稳定指纹、persona 目录解析、CO 预期间隔。对应 K1 / H8。",
    "internal/runner/runner.go": "单机 runner：生成到达表 → 信号量限并发 → 起 bot → 汇总分位数 → 写 plan/meta/result/bots 四个产物。",
    "internal/controller/controller.go": "控制面：6 个 HTTP 端点 + 实验生命周期 + 并发实验限制 + 配置快照。",

    # ===== 被测系统 =====
    "srv/config/config.go": "SUT 配置聚合：接入层/房间/分配器/匹配器/Redis/结算六段，全部可调，瓶颈即配置。",
    "srv/sut/sut.go": "四层组装点：接入层→匹配器→分配器→房间服，并负责指标端点、优雅关闭与测试用随机端口。",
    "srv/gateway/gateway.go": "接入层：WS 握手、Nonce+HMAC 登录鉴权、排队/取消、匹配结果推送、/metrics 与 /debug/state。",
    "srv/account/account.go": "账号与会话：登录签名校验、自动注册、token 签发与过期、Elo-lite MMR 更新。",
    "srv/match/match.go": "匹配器：MMR 窗口逐步放宽、FIFO 防饿死、按 MMR 分边、不满开局、把匹配与分配解耦成两段延迟。",
    "srv/alloc/alloc.go": "分配器：Redis 预热房间池 + 全局锁（两个刻意瓶颈）+ 分配延迟指标 + 补池循环。",
    "srv/room/room.go": "房间服核心：生命周期、10Hz tick、权威裁决（移动校验/命中判定/复活）、快照广播、同步结算写库、回收判定。",
    "srv/room/manager.go": "房间管理器：TCP 监听、进房 token、连接读写循环、janitor 回收与泄漏统计。",
    "srv/room/peer.go": "单连接的写侧：有界发送队列（背压）、异步写循环、同步写（结算帧必须送达）、幂等关闭。",
    "srv/store/store.go": "两个外部依赖的模拟：分片 Redis（可强制热键）+ 结算写库（可注入 fsync 延迟）。",

    # ===== 工具与脚本 =====
    "analysis/analyze.py": "离线分析：读 bots.jsonl → 分位数表 + KS 检验 + Wasserstein 距离 + persona 维度 + 手写 SVG 的 CDF 图。零依赖。",
    "scripts/md2docx.py": "Markdown → Word 转换器：表格按表头签名分列宽、OOXML 元素顺序归一化、页码与表头重复。",
    "scripts/gen_codemap.py": "本清单的生成器：扫描文件/行数/导出符号，套上人工描述，输出 docs/CODE-MAP.md。",
    "scripts/smoke.ps1": "冒烟脚本：编译 → 起 SUT → 跑 runner → 生成分析报告 → 收尾清进程。",
    "scripts/smoke.cmd": "smoke.ps1 的 cmd 包装：绕过 PowerShell 执行策略。",
    "scripts/demo.ps1": "一条命令开玩：起 SUT → 起陪练 bot → 启动交互客户端 → 退出时清理后台进程。",
    "scripts/demo.cmd": "demo.ps1 的 cmd 包装：绕过 PowerShell 执行策略。",
    "Makefile": "常用命令入口：build / vet / test / test-race / cover / smoke / baseline / fixed / clean。",

    # ===== 配置 =====
    "deploy/sut.yaml": "SUT 默认配置：三个刻意瓶颈全部打开（Redis 热键、分配器全局锁、结算同步写）。",
    "deploy/sut.fixed.yaml": "SUT 修复版配置：三个瓶颈全关 + 池子放大，用于「修复前后」对照实验。",
    "deploy/sut.smoke.yaml": "冒烟配置：一局 10 秒、瓶颈全关，供 CI 与本地联调。",
    "deploy/sut.duel.yaml": "1v1 快速对局配置（team_size=1）：给真人客户端自测用，两个客户端就能开一局。",
    "deploy/sut.leak.yaml": "泄漏注入配置：关掉房间回收，复现 room_leak_gauge 持续上涨。",
    "deploy/prometheus.yml": "Prometheus 抓取配置：2s 间隔（压测场景默认 15s 会抹平尖峰），抓 SUT/runner/controller。",
    "deploy/rules.yml": "SLO 告警规则：每条规则对应 docs/SLO.md 里的一行 SLO，附 runbook 指向。",
    "deploy/compose.yaml": "本地一键起：SUT + 控制面 + Prometheus + Grafana；runner 放在 profile 里，避免与 SUT 抢 CPU。",
    "deploy/Dockerfile": "多阶段构建：只把静态编译的二进制带进运行镜像。",
    "deploy/grafana/dashboards/swarm-overview.json": "Grafana 总览看板：11 个面板（活跃 bot、泄漏、超预算率、匹配/分配分位数、bot 阶段延迟等）。",
    "deploy/grafana/provisioning/dashboards/swarm.yml": "Grafana 看板自动加载配置。",
    "deploy/grafana/provisioning/datasources/prometheus.yml": "Grafana 数据源自动配置（timeInterval 与抓取间隔对齐）。",
    "plans/smoke.yaml": "冒烟负载计划：30 bot、5/s、90 秒，配套短会话 persona。",
    "plans/baseline.yaml": "基线负载计划：阶梯到 20/s、300 bot，用于看各层稳定分位数。",
    "plans/peak.yaml": "尖峰负载计划：基线 + 两次整点惊群，用于复现瞬时形状引发的事故。",
    "plans/duel.yaml": "陪练队列：4 个 bot 慢慢补位，让真人客户端随时能配到对手。",
    "plans/personas/default.yaml": "默认 persona 目录（6 种原型、带分布参数），与 internal/behavior/defaults.go 有测试保证一致。",
    "plans/personas/smoke.yaml": "冒烟 persona：会话压到 1–2 分钟、失败路径概率调高，专供联调。",
    "analysis/reference.example.json": "参照样本占位文件：演示 KS / Wasserstein 对比流程，明确标注「不可用于报告」。",

    # ===== 文档 =====
    "README.md": "项目总览：架构图、指标表、代码地图、快速开始、三个刻意瓶颈、刻意不做、已知限制。",
    "docs/SLO.md": "四层 SLO（登录/匹配/分配/房间）+ 业务口径 + 错误预算用法。",
    "docs/METRICS.md": "每个指标的定义、标签、PromQL、以及「为什么 p99 和别人不一样」的口径说明。",
    "docs/FINDINGS.md": "压测发现并修复的 8 个真实缺陷：现象→定位→根因→修复→面试怎么讲。",
    "docs/HANDWRITE.md": "必须自己手写的部分（从两份文档提取）：白板代码、必背数字、自写文字、自检题库。",
    "docs/CODE-MAP.md": "本文件：项目从源码到组成的完整清单，由 scripts/gen_codemap.py 生成。",
    "docs/必须手写的部分.docx": "docs/HANDWRITE.md 的 Word 版，由 scripts/md2docx.py 生成。",
    "docs/项目源码与组成清单.docx": "本文件的 Word 版，由 scripts/md2docx.py 生成。",
    "analysis/README.md": "离线分析的使用说明：为什么不用 scipy/matplotlib、参照数据从哪来、口径怎么对齐。",
    ".gitignore": "忽略构建产物与实验结果（results/、bin/、parquet/jsonl），配置与计划必须进版本库。",
    "go.mod": "模块声明与唯一的第三方依赖：gopkg.in/yaml.v3。",
    "go.sum": "依赖校验和。",
}

LAYERS = [
    ("3.1 入口层（cmd）", ["cmd/"],
     "三个可执行文件，各自只做「解析参数 + 调用下层 + 打印」。真正的组装在 srv/sut 与 internal/runner。"),
    ("3.2 协议层（wire / ws / protocol）", ["internal/wire/", "internal/ws/", "internal/protocol/"],
     "从字节流到业务消息的三层：帧（怎么分边界）→ 传输（WS 握手与分片）→ 消息（字段语义）。"
     "这一层换掉，行为层与观测层不用动。"),
    ("3.3 调度与时间层（clock / sched）", ["internal/clock/", "internal/sched/"],
     "整个压测平台的地基：开环到达时间表 + 固定步长 tick。开环与闭环的区别就在这两百行里。"),
    ("3.4 行为层（behavior）", ["internal/behavior/"],
     "「像不像人」的全部来源：persona 分布、双峰节奏、危险率退出、可复现随机。"),
    ("3.5 bot 与客户端层（bot / play）", ["internal/bot/", "internal/play/"],
     "协议级 bot 的会话状态机与两个客户端；以及真人客户端的终端界面与键盘。"
     "两者复用同一套 wire/protocol——这是「真人能玩通」等于「协议实现正确」的原因。"),
    ("3.6 观测层（hist / metrics / obs / report / sink / runstats）",
     ["internal/hist/", "internal/metrics/", "internal/obs/", "internal/report/", "internal/sink/", "internal/runstats/"],
     "测量与出口：分位数（HdrHistogram）、指标暴露（Prometheus 文本）、指标清单（单一事实来源）、"
     "双路采集、结果落盘、计数。"),
    ("3.7 运行与控制面（plan / runner / controller）", ["internal/plan/", "internal/runner/", "internal/controller/"],
     "把「一份 YAML」变成「一次可复现的实验」：计划校验与指纹 → 单机执行 → HTTP 控制。"),
    ("3.8 被测系统（srv）", ["srv/"],
     "靶子本身：接入层 + 匹配器 + 分配器 + 房间服 + 存储模拟。"
     "三个「故意留下的瓶颈」都在这一层，且全部做成配置开关。"),
]

NON_CODE_DIRS = {
    "deploy/", "plans/", "docs/", "analysis/", "scripts/",
}

TOP_LEVEL = {".gitignore", "go.mod", "go.sum", "Makefile", "README.md"}


# --------------------------------------------------------------------------- #
# 扫描
# --------------------------------------------------------------------------- #

PKG_RE = re.compile(r"^package\s+(\w+)", re.M)
EXPORT_RE = re.compile(r"^(?:func|type|const|var)\s+(?:\([^)]*\)\s*)?([A-Z]\w*)", re.M)
TEST_RE = re.compile(r"^func (Test\w+|Benchmark\w+|Fuzz\w+)", re.M)


def scan() -> dict:
    """返回 {相对路径: 事实字典}。"""
    facts = {}
    for base, dirs, files in os.walk(ROOT):
        dirs[:] = sorted(d for d in dirs if d not in SKIP_DIRS)
        for f in sorted(files):
            path = os.path.relpath(os.path.join(base, f), ROOT).replace("\\", "/")
            try:
                text = open(path, encoding="utf-8").read()
            except (UnicodeDecodeError, OSError):
                facts[path] = {"lines": 0, "pkg": "", "exports": [], "tests": []}
                continue
            lines = text.count("\n") + (0 if text.endswith("\n") else 1)
            rec = {"lines": lines, "pkg": "", "exports": [], "tests": []}
            if path.endswith(".go"):
                m = PKG_RE.search(text)
                rec["pkg"] = m.group(1) if m else "?"
                rec["exports"] = sorted(set(EXPORT_RE.findall(text)))
                rec["tests"] = sorted(set(TEST_RE.findall(text)))
            facts[path] = rec
    return facts


def human(n: int) -> str:
    return f"{n:,}"


def exports_cell(rec: dict, path: str) -> str:
    if not path.endswith(".go"):
        return "—"
    if rec["tests"]:
        return f"{len(rec['tests'])} 个测试函数"
    if not rec["exports"]:
        return "—（内部实现，不导出）"
    shown = rec["exports"][:6]
    cell = " / ".join(f"`{e}`" for e in shown)
    if len(rec["exports"]) > len(shown):
        cell += f" …+{len(rec['exports']) - len(shown)}"
    return cell


# --------------------------------------------------------------------------- #
# 生成
# --------------------------------------------------------------------------- #

DIR_INTRO = {
    "cmd/sutd/": "被测系统入口（参数解析 → srv/sut 组装 → 优雅退出）。",
    "cmd/runner/": "单机 bot 集群入口（读计划 → 执行 → 打印分位数）。",
    "cmd/controller/": "控制面入口（起 HTTP 服务）。",
    "cmd/": "三个可执行文件，各自只做「解析参数 + 调用下层 + 打印」。",
    "internal/wire/": "帧格式：怎么把字节流切成消息。",
    "internal/ws/": "WebSocket 协议的零依赖实现（RFC6455）。",
    "internal/protocol/": "消息语义：字段含义与编码方式。",
    "internal/clock/": "固定步长调度与帧预算。",
    "internal/sched/": "开环到达时间表——本项目与普通压测工具的根本区别。",
    "internal/behavior/": "persona 分布与可复现随机。",
    "internal/bot/": "协议级 bot 的会话状态机与客户端。",
    "internal/hist/": "HdrHistogram 分位数与 CO 修正。",
    "internal/metrics/": "Prometheus 指标库（自实现，零依赖）。",
    "internal/obs/": "指标清单（单一事实来源）。",
    "internal/report/": "分位数双路采集。",
    "internal/sink/": "结果落盘。",
    "internal/runstats/": "跨 bot 计数。",
    "internal/plan/": "负载计划。",
    "internal/runner/": "单机执行器。",
    "internal/controller/": "控制面 HTTP API。",
    "srv/account/": "账号与会话。",
    "srv/alloc/": "房间分配器（含两个刻意瓶颈）。",
    "srv/config/": "SUT 配置聚合。",
    "srv/gateway/": "接入层。",
    "srv/match/": "匹配器。",
    "srv/room/": "房间服与房间管理器。",
    "srv/store/": "Redis 与结算库的模拟。",
    "srv/sut/": "四层组装 + 端到端测试。",
    "deploy/": "部署与观测栈配置。",
    "plans/": "负载计划与 persona 目录（配置即实验）。",
    "docs/": "文档。",
    "analysis/": "离线分析与报告。",
    "scripts/": "工具脚本。",
}


def gen(facts: dict) -> str:
    out = []
    add = out.append

    go_files = [p for p in facts if p.endswith(".go")]
    go_code = [p for p in go_files if not p.endswith("_test.go")]
    go_test = [p for p in go_files if p.endswith("_test.go")]
    pkg_dirs = sorted({os.path.dirname(p) for p in go_code})
    total_fn = sum(len(facts[p]["tests"]) for p in go_test)

    add("# 项目源码与组成清单")
    add("")
    add("> 本文由 [`scripts/gen_codemap.py`](../scripts/gen_codemap.py) 生成：**行数与导出符号是现场扫描的**，"
        "只有「作用」一列是人工描述。改了代码重跑一次即可刷新，不会出现「文档写着 300 行、代码已经 800 行」的窘境。")
    add("")
    add("```bash")
    add("python scripts/gen_codemap.py")
    add("```")
    add("")
    add("## 1. 这个项目是什么")
    add("")
    add("一个**用 bot 玩家模拟真人行为、压房间服与匹配器**的压测平台。它同时是被测系统的作者与压测工具的作者——"
        "这既是它能一个人做完的原因，也是它和「拿 JMeter 压别人的接口」在叙事上的根本区别。")
    add("")
    add("四层架构，每层独立可测、独立可换：")
    add("")
    add("```")
    add("控制面   controller(HTTP) → runner(单机执行) → results/*.jsonl")
    add("行为面   persona 分布 · 会话状态机 · 双峰思考时间 · 危险率退出 · 非法输入注入")
    add("协议面   gateway: WebSocket + 信封      （登录 / 排队 / 匹配）")
    add("         room   : 裸 TCP + 定长二进制    （10Hz 输入 / 快照 / 结算）")
    add("观测面   Prometheus 文本暴露 · HdrHistogram(CO 修正) · JSONL → DuckDB")
    add("           ↓")
    add("被测系统 登录 → 匹配器 → 分配器(Redis 热池) → 房间服(10Hz tick) → 结算写库")
    add("```")
    add("")

    # ---- 2. 组成总览 ----
    add("## 2. 组成总览")
    add("")
    add("### 2.1 规模")
    add("")
    add("| 项 | 数量 |")
    add("|---|---|")
    add("| 源码文件（Go，非测试） | " + str(len(go_code)) + " |")
    add("| 源码行数（Go，非测试） | " + human(sum(facts[p]["lines"] for p in go_code)) + " |")
    add("| 测试文件 | " + str(len(go_test)) + " |")
    add("| 测试行数 | " + human(sum(facts[p]["lines"] for p in go_test)) + " |")
    add("| 测试函数 | " + str(total_fn) + " |")
    add("| Go 包（含 cmd 与 srv） | " + str(len(pkg_dirs)) + " |")
    add("| 第三方依赖 | 1（`gopkg.in/yaml.v3`） |")
    add("| 非 Go 文件（配置/文档/脚本） | " + str(len([p for p in facts if not p.endswith(".go")])) + " |")
    add("")
    add("### 2.2 目录树")
    add("")
    add("```")
    add("swarm/")
    add("  cmd/")
    add("    sutd/         被测系统入口（接入层+匹配器+分配器+房间服）")
    add("    runner/       单机 bot 集群执行器")
    add("    controller/   控制面 HTTP API")
    add("  internal/")
    add("    wire/         长度前缀 + 消息号 + 序列号 的二进制帧")
    add("    ws/           RFC6455 WebSocket（零依赖）")
    add("    protocol/     控制面 JSON / 数据面定长二进制 的消息定义")
    add("    clock/        固定步长调度 + 帧预算")
    add("    sched/        开环到达时间表与派发器")
    add("    behavior/     persona 分布、双峰节奏、危险率退出、可复现随机")
    add("    bot/          会话状态机 + 接入层客户端 + 房间客户端")
    add("    hist/         HdrHistogram + coordinated-omission 修正")
    add("    metrics/      Prometheus 文本暴露")
    add("    obs/          全平台指标清单")
    add("    report/       分位数双路采集")
    add("    sink/         JSONL 结果落盘")
    add("    runstats/     跨 bot 原子计数")
    add("    plan/         负载计划（YAML + 指纹）")
    add("    runner/       单机执行器")
    add("    controller/   控制面")
    add("  srv/")
    add("    config/       SUT 配置聚合")
    add("    sut/          四层组装（被 sutd 与端到端测试共用）")
    add("    gateway/      接入层（WS + 鉴权 + 排队）")
    add("    account/      账号、token、MMR")
    add("    match/        匹配器（窗口放宽）")
    add("    alloc/        分配器（Redis 池 + 全局锁）")
    add("    room/         房间服（10Hz tick / 权威裁决 / 回收）")
    add("    store/        Redis 与结算库模拟")
    add("  plans/          负载计划与 persona 目录")
    add("  deploy/         compose / Prometheus / Grafana / SUT 配置")
    add("  analysis/       离线分析（KS / Wasserstein / CDF）")
    add("  scripts/        冒烟、md→docx、本清单生成器")
    add("  docs/           SLO / METRICS / FINDINGS / HANDWRITE / CODE-MAP")
    add("```")
    add("")

    add("### 2.3 四个可执行文件")
    add("")
    add("| 二进制 | 命令 | 默认监听 | 职责 |")
    add("|---|---|---|---|")
    add("| `sutd` | `sutd -config deploy/sut.yaml` | 8080(WS) / 8090(TCP) / 9090(指标) | 被测系统：接入层 + 匹配器 + 分配器 + 房间服 |")
    add("| `runner` | `runner -plan plans/smoke.yaml` | 9091(指标) | bot 集群单机执行器 |")
    add("| `controller` | `controller -addr :7070` | 7070(HTTP) | 控制面：提交/查询/停止实验 |")
    add("| `client` | `client -account me` | — | 真人客户端：交互 / 逐行 / 无人值守，并把真人数据落盘 |")
    add("")
    add("### 2.4 一次完整链路（7 步）")
    add("")
    add("| 步 | 动作 | 协议 | 代码入口 |")
    add("|---|---|---|---|")
    add("| 1 | bot 连接入层，Hello/Welcome 交换版本与 nonce | WS 二进制 + 信封 | `internal/bot/client.go` → `srv/gateway/gateway.go` |")
    add("| 2 | 用 nonce 做 HMAC 签名登录，拿 token 与 MMR | WS | `srv/account/account.go` |")
    add("| 3 | 进匹配队列，等待匹配推送（可取消） | WS | `srv/match/match.go` |")
    add("| 4 | 匹配成功，分配器从一个 Redis 热键池里取房间 | 进程内 | `srv/alloc/alloc.go` |")
    add("| 5 | bot 用 room_token 连房间服 TCP，进房拿槽位 | 裸 TCP + 长度前缀帧 | `srv/room/manager.go` |")
    add("| 6 | 10Hz：bot 发输入、房间服权威裁决后广播全量快照 | 裸 TCP 定长二进制 | `srv/room/room.go` |")
    add("| 7 | 结算：同步写库 → 推送结果 → 房间回收 → bot 再排或退出 | 裸 TCP + 进程内 | `srv/room/room.go` / `srv/store/store.go` |")
    add("")

    # ---- 3. 源码清单 ----
    add("## 3. 源码清单")
    add("")
    add("按层排列。`行` 是文件行数（含注释）；`关键导出` 是包外可见的标识符，"
        "标「内部实现」说明那个文件只能被同包的其他文件使用。")
    add("")
    add("> 测试文件不在这里列，统一在 §5「测试清单」——每条测试都对应一个会真出问题的性质。")
    add("")

    for title, prefixes, intro in LAYERS:
        add(f"### {title}")
        add("")
        add(intro)
        add("")
        files = [p for p in facts
                 if p.endswith(".go") and not p.endswith("_test.go")
                 and any(p.startswith(pre) for pre in prefixes)]
        # 一层一张表：按路径排序天然把同一个包的文件排在一起，
        # 不再按包切小表（Word 里 27 张单行表会碎得没法读）。
        add("| 文件 | 行 | 作用 | 关键导出 |")
        add("|---|---|---|---|")
        for p in sorted(files):
            rec = facts[p]
            role = ROLE.get(p, "⚠ 未填写作用描述")
            add(f"| `{p}` | {rec['lines']} | {role} | {exports_cell(rec, p)} |")
        add("")

    # ---- 4. 非 Go 产物 ----
    add("## 4. 配置、部署与计划（非 Go 文件）")
    add("")
    add("这些文件同样是**实验条件的一部分**：改配置等于改实验，所以它们必须和代码一起进版本库。")
    add("")
    add("| 目录 | 放什么 |")
    add("|---|---|")
    for d in ("deploy/", "plans/", "docs/", "analysis/", "scripts/", "/"):
        add(f"| `{d if d != '/' else '仓库根目录'}` | {DIR_INTRO.get(d, '模块声明、依赖校验和、忽略规则、README。')} |")
    add("")
    add("| 文件 | 行 | 作用 |")
    add("|---|---|---|")
    non_go = [p for p in facts if not p.endswith(".go")]
    # 根目录文件排在最后，读起来先看子目录再看根
    for p in sorted(non_go, key=lambda x: (0 if "/" in x else 1, x)):
        rec = facts[p]
        role = ROLE.get(p, "⚠ 未填写作用描述")
        add(f"| `{p}` | {rec['lines']} | {role} |")
    add("")

    # ---- 5. 测试 ----
    add("## 5. 测试清单")
    add("")
    add("测试不是「补一个覆盖率」，每条都对应一个会真出问题的性质。")
    add("")
    add("| 测试文件 | 行 | 函数数 | 覆盖的关键性质 |")
    add("|---|---|---|---|")
    test_notes = {
        "internal/wire/wire_test.go": "帧往返、半包截断、超大帧、短写、信封",
        "internal/protocol/protocol_test.go": "二进制读写越界不 panic、全部消息往返、快照人数上限、截断消息报错",
        "internal/ws/ws_test.go": "握手成功/失败、长度边界 125/126/65535、分片、掩码、ping-pong、close、超限、并发写、wss",
        "internal/clock/clock_test.go": "无累积漂移、滞后非负、慢消费者不阻塞调度、帧预算",
        "internal/sched/sched_test.go": "五种形状单调性、爬坡/尖峰速率、不可达报错、**派发不等待**、ctx 取消、抖动不改总数",
        "internal/behavior/behavior_test.go": "目录权重和、persona 可复现、样本占比、对数正态右偏、截断、双峰、危险率单调、移动越界、**YAML 与内置默认值一致**",
        "internal/hist/hist_test.go": "分位精度、常量/空图、钳位、补记计数、**CO 修正效果**、Merge、累积桶、并发、JSON",
        "internal/metrics/metrics_test.go": "文本快照、转义、累积桶、无标签不写 {}、SetFunc、并发、序列清理、运行时指标",
        "internal/controller/controller_test.go": "提交→轮询→完成→列表→停止 409、四种错误请求",
        "internal/runner/runner_test.go": "**四个产物都能被对应解析器读进去**（result.json 是 JSON、plan.yaml 是 YAML、bots.jsonl 逐行 JSON）",
        "srv/room/room_test.go": "**命中能击杀**、超距不命中、三类权威校验、结算与生命周期",
        "srv/sut/e2e_test.go": "**进程内起整套 SUT 跑完一局并断言有击杀**、房间回收、指标端点格式",
        "srv/sut/client_e2e_test.go": "真人客户端公开 API：两个客户端打同一局，**结算 tick/结果一致**（守住结算与关连接的竞态）",
        "srv/account/account_test.go": "重登让旧 token 失效且不误伤别人、过期拒绝与摊还清理、Elo 上下界，以及新旧登录算法对照基准",
        "srv/gateway/queue_test.go": "入队参数校验：模式不匹配、组队请求被**明确拒绝**而不是静默忽略",
        "internal/plan/plan_test.go": "计划指纹**跨机器/跨目录稳定**，但语义变化必须变；示例计划都能解析",
        "internal/protocol/snapshot_test.go": "快照解码的分配语义（DecodeSnapshot 必分配 / Into 零分配）与缓冲复用不残留旧帧",
        "internal/play/view_test.go": "TUI 与逐行两套渲染、ANSI 可见宽度、血条定宽、原地刷新与退出复位",
        "cmd/client/main_test.go": "结果码机器可读、按住/一次性两种输入语义、自动策略、记录产物是合法 JSONL",
    }
    for p in sorted(x for x in facts if x.endswith("_test.go")):
        rec = facts[p]
        add(f"| `{p}` | {rec['lines']} | {len(rec['tests'])} | {test_notes.get(p, '—')} |")
    add("")

    # ---- 6. 依赖关系 ----
    add("## 6. 包级依赖方向")
    add("")
    add("依赖是单向的，没有环。**这条是这套代码能被一个人维护的前提**——改协议层不会牵动观测层。")
    add("")
    add("```")
    add("cmd/*  ──►  srv/sut  ──►  srv/{gateway, match, alloc, room, store, config}")
    add("                  │                │")
    add("                  │                ├──► internal/protocol")
    add("                  │                ├──► internal/wire")
    add("                  │                └──► internal/obs ──► internal/metrics")
    add("                  │")
    add("  cmd/runner  ──► internal/runner ──► internal/bot ──► {ws, wire, protocol, behavior, report, sink, runstats}")
    add("                        │                  └──► internal/{plan, sched, obs, hist}")
    add("                        └──► internal/controller（仅 cmd 层组合，不反向依赖）")
    add("")
    add("  internal/ws      不依赖任何内部包（纯标准库）")
    add("  internal/wire    不依赖任何内部包")
    add("  internal/hist    不依赖任何内部包")
    add("  internal/metrics 不依赖任何内部包")
    add("```")
    add("")
    add("四个「叶子包」（ws / wire / hist / metrics）都是零内部依赖、零第三方依赖，"
        "可以整包拷到别的项目里用。")
    add("")

    # ---- 7. 阅读顺序 ----
    add("## 7. 建议阅读顺序")
    add("")
    add("按「先看清骨架，再看肌肉」的顺序，一条线读下来大约 2 小时：")
    add("")
    add("| 顺序 | 读什么 | 为什么先读它 |")
    add("|---|---|---|")
    add("| 1 | [`README.md`](../README.md) | 先知道这东西想证明什么，否则读代码会迷路 |")
    add("| 2 | [`internal/sched/sched.go`](../internal/sched/sched.go) | 开环 vs 闭环是整项目的题眼，200 行看完 |")
    add("| 3 | [`internal/bot/bot.go`](../internal/bot/bot.go) | 一条 bot 生命周期的全貌，顺带理解所有失败路径 |")
    add("| 4 | [`srv/room/room.go`](../srv/room/room.go) | 靶子的核心：tick、权威裁决、结算、回收 |")
    add("| 5 | [`srv/sut/sut.go`](../srv/sut/sut.go) | 四层怎么拼起来的，看这一个文件就够 |")
    add("| 6 | [`internal/hist/hist.go`](../internal/hist/hist.go) + [`internal/report/reporter.go`](../internal/report/reporter.go) | 为什么 p99 是这个数 |")
    add("| 7 | [`docs/FINDINGS.md`](FINDINGS.md) | 8 个真实缺陷，比读代码更快理解「哪些地方容易错」 |")
    add("")
    add("想直接上手跑，看 [`README.md`](../README.md) 的「快速开始」；"
        "想改一处看指标怎么动，改 [`deploy/sut.yaml`](../deploy/sut.yaml) 里的开关。")
    add("")

    return "\n".join(out) + "\n"


def main(argv) -> int:
    # Windows 控制台默认 GBK，打印中文与符号会 UnicodeEncodeError；
    # 这个脚本要在任何机器上都能跑，所以显式切到 UTF-8。
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

    facts = scan()
    md = gen(facts)

    # 测试文件的作用写在 §5 的测试清单里，不参与「作用描述」完整性检查
    missing = [p for p in facts
               if p not in ROLE and not p.endswith((".docx", "_test.go"))]
    # 反向检查：ROLE 里写了、仓库里却没有，说明文件被删/改名而描述没跟上
    stale = sorted(p for p in ROLE if p not in facts)
    if "--check" in argv:
        if missing:
            print("缺少作用描述的文件：")
            for p in missing:
                print("  ", p)
        if stale:
            print("描述了但文件不存在的条目（已删除或改名？）：")
            for p in stale:
                print("  ", p)
        if missing or stale:
            return 1
        print(f"全部 {len(facts)} 个文件都有作用描述，且没有失效条目")
        return 0

    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    with open(OUT, "w", encoding="utf-8") as f:
        f.write(md)
    print(f"已生成 {OUT}（{len(md.splitlines())} 行）")
    if missing:
        print("警告：以下文件缺少作用描述，已在清单里标记：")
        for p in missing:
            print("  ", p)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
