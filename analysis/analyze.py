#!/usr/bin/env python3
"""压测结果的离线分析：保真度检验 + 分位数表 + CDF 图。

只用标准库，不依赖 numpy/scipy/matplotlib。理由有两个：

1. 这份脚本要能在任何装了 Python 3.8+ 的机器上跑（包括 CI 和面试现场）；
2. KS 检验与 Wasserstein 距离在**一维**情形下有闭式做法，
   自己实现比引 scipy 更短，也更能说清「我到底算的是什么」。

用法：
    python analysis/analyze.py results/<run-id>
    python analysis/analyze.py results/<run-id> --reference analysis/reference.example.json
    python analysis/analyze.py results/*        # 汇总多个 run

产出（写到 results/<run-id>/analysis/ 下）：
    report.md   人看的报告
    cdf.svg     会话时长 CDF 对比图（可直接贴进 README）

口径说明（必须和 docs/METRICS.md 一致）：
    - bots.jsonl 里 phases[i].offset_ms 是相对**应到时刻**的偏移，dur_ms 是阶段自身耗时。
    - 阶段延迟 = offset_ms + dur_ms（对外报告用的就是它，已含 coordinated-omission 语义）。
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
from collections import Counter, defaultdict


# --------------------------------------------------------------------------- #
# 统计工具（一维闭式实现）
# --------------------------------------------------------------------------- #

def ecdf(samples):
    """返回 (xs, ys)：经验分布函数的阶梯点。"""
    xs = sorted(samples)
    n = len(xs)
    ys = [(i + 1) / n for i in range(n)]
    return xs, ys


def ks_2samp(a, b):
    """两样本 Kolmogorov-Smirnov 检验。

    返回 (D, p)。p 用渐近公式 Q(λ)=2Σ(-1)^(k-1)exp(-2k²λ²)，λ=D·sqrt(n_eff)。
    样本量小（<20）时这个近似偏保守，报告里会标注。
    """
    a = sorted(a)
    b = sorted(b)
    n1, n2 = len(a), len(b)
    if n1 == 0 or n2 == 0:
        return 0.0, 1.0

    # 双指针合并求最大竖直距离
    i = j = 0
    d = 0.0
    while i < n1 and j < n2:
        if a[i] <= b[j]:
            i += 1
        else:
            j += 1
        d = max(d, abs(i / n1 - j / n2))
    d = max(d, abs(n1 / n1 - n2 / n2))

    n_eff = n1 * n2 / (n1 + n2)
    lam = d * math.sqrt(n_eff)
    if lam <= 0:
        return d, 1.0
    term = 0.0
    for k in range(1, 101):
        term += ((-1) ** (k - 1)) * math.exp(-2.0 * k * k * lam * lam)
    p = max(0.0, min(1.0, 2.0 * term))
    return d, p


def wasserstein_1d(a, b):
    """1 维 Wasserstein-1（推土机距离）距离，单位与样本一致。

    闭式做法：W1 = ∫|F1(x) - F2(x)| dx。对两条阶梯函数逐段积分即可，
    不需要最优传输求解器。好处是**有量纲、能直接念出来**：
    「bot 的会话时长中位数比真实数据少 7 分钟」。
    """
    a = sorted(a)
    b = sorted(b)
    n1, n2 = len(a), len(b)
    if n1 == 0 or n2 == 0:
        return float("nan")

    xs = sorted(set(a) | set(b))
    w = 0.0
    i = j = 0
    prev = xs[0]
    for x in xs[1:]:
        while i < n1 and a[i] <= prev:
            i += 1
        while j < n2 and b[j] <= prev:
            j += 1
        f1 = i / n1
        f2 = j / n2
        w += abs(f1 - f2) * (x - prev)
        prev = x
    return w


def quantile(samples, q):
    """线性插值分位数（与 numpy 默认口径一致）。"""
    if not samples:
        return 0.0
    xs = sorted(samples)
    if len(xs) == 1:
        return xs[0]
    pos = (len(xs) - 1) * q
    lo = int(math.floor(pos))
    hi = min(lo + 1, len(xs) - 1)
    frac = pos - lo
    return xs[lo] * (1 - frac) + xs[hi] * frac


def summarize(samples):
    if not samples:
        return {"count": 0}
    return {
        "count": len(samples),
        "min": min(samples),
        "p50": quantile(samples, 0.50),
        "p90": quantile(samples, 0.90),
        "p95": quantile(samples, 0.95),
        "p99": quantile(samples, 0.99),
        "max": max(samples),
        "mean": sum(samples) / len(samples),
    }


# --------------------------------------------------------------------------- #
# 读取结果
# --------------------------------------------------------------------------- #

def load_run(run_dir):
    path = os.path.join(run_dir, "bots.jsonl")
    if not os.path.exists(path):
        return None, None
    sessions, summary = [], None
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            if rec.get("type") == "run_summary":
                summary = rec
            else:
                sessions.append(rec)
    return sessions, summary


def phase_latency_ms(rec, name):
    for ph in rec.get("phases") or []:
        if ph.get("name") == name:
            return ph.get("offset_ms", 0) + ph.get("dur_ms", 0)
    return None


# --------------------------------------------------------------------------- #
# 报告与出图
# --------------------------------------------------------------------------- #

def svg_cdf(series, path, title, x_label="秒"):
    """手写 SVG 的 CDF 对比图。

    为什么不引 matplotlib：一张 CDF 图就是若干条折线，手写 SVG 反而没有依赖、
    没有字体问题，还能直接塞进 README（GitHub 支持渲染 SVG）。
    """
    width, height = 680, 400
    pad_l, pad_r, pad_t, pad_b = 56, 16, 36, 44

    all_xs = [x for xs, _ in series.values() for x in xs]
    if not all_xs:
        return
    x_min, x_max = min(all_xs), max(all_xs)
    if x_max <= x_min:
        x_max = x_min + 1
    y_min, y_max = 0.0, 1.0

    def sx(v):
        return pad_l + (v - x_min) / (x_max - x_min) * (width - pad_l - pad_r)

    def sy(v):
        return height - pad_b - (v - y_min) / (y_max - y_min) * (height - pad_t - pad_b)

    colors = ["#2f6feb", "#d29922", "#3fb950", "#f85149", "#a371f7", "#76b900"]
    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
        f'viewBox="0 0 {width} {height}" role="img" aria-label="{title}">',
        '<rect width="100%" height="100%" fill="#ffffff"/>',
        f'<text x="{pad_l}" y="22" font-family="sans-serif" font-size="14" fill="#111">{title}</text>',
    ]

    # 网格 + 坐标轴
    for frac in (0.0, 0.25, 0.5, 0.75, 1.0):
        y = sy(frac)
        parts.append(f'<line x1="{pad_l}" y1="{y:.1f}" x2="{width - pad_r}" y2="{y:.1f}" '
                     f'stroke="#e6e6e6" stroke-width="1"/>')
        parts.append(f'<text x="{pad_l - 8}" y="{y + 4:.1f}" text-anchor="end" '
                     f'font-family="sans-serif" font-size="10" fill="#555">{frac:.2f}</text>')
    for frac in (0.0, 0.25, 0.5, 0.75, 1.0):
        xv = x_min + frac * (x_max - x_min)
        x = sx(xv)
        parts.append(f'<text x="{x:.1f}" y="{height - pad_b + 16}" text-anchor="middle" '
                     f'font-family="sans-serif" font-size="10" fill="#555">{xv:.0f}</text>')
    parts.append(f'<line x1="{pad_l}" y1="{height - pad_b}" x2="{width - pad_r}" '
                 f'y2="{height - pad_b}" stroke="#999" stroke-width="1"/>')
    parts.append(f'<text x="{width / 2:.0f}" y="{height - 8}" text-anchor="middle" '
                 f'font-family="sans-serif" font-size="11" fill="#555">{x_label}</text>')

    for idx, (name, (xs, ys)) in enumerate(series.items()):
        color = colors[idx % len(colors)]
        pts = " ".join(f"{sx(x):.1f},{sy(y):.1f}" for x, y in zip(xs, ys))
        parts.append(f'<polyline points="{pts}" fill="none" stroke="{color}" stroke-width="2"/>')
        # 图例
        lx = pad_l + 12 + (idx % 2) * 300
        ly = pad_t + 12 + (idx // 2) * 16
        parts.append(f'<line x1="{lx}" y1="{ly}" x2="{lx + 18}" y2="{ly}" stroke="{color}" stroke-width="2"/>')
        parts.append(f'<text x="{lx + 24}" y="{ly + 4}" font-family="sans-serif" '
                     f'font-size="11" fill="#111">{name}</text>')

    parts.append("</svg>")
    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(parts))


def fmt(x, unit="", digits=1):
    if isinstance(x, float):
        return f"{x:.{digits}f}{unit}"
    return f"{x}{unit}"


def build_report(run_dir, sessions, summary, reference=None):
    lines = []
    add = lines.append

    add(f"# 压测分析报告 · `{os.path.basename(run_dir)}`")
    add("")

    # ---- 1. 运行概况 ----
    add("## 1. 运行概况")
    add("")
    if summary:
        add("| 项 | 值 |")
        add("|---|---|")
        add(f"| 计划 | {summary.get('experiment', '?')} |")
        add(f"| 计划指纹 | `{summary.get('plan_fingerprint', '?')}` |")
        add(f"| 到达 bot 数 | {summary.get('arrivals', '?')} |")
        add(f"| 会话数 | {summary.get('sessions', '?')} |")
        add(f"| 完成对局数 | {summary.get('matches', '?')} |")
        add(f"| 有效对局吞吐 | {fmt(summary.get('goodput_matches_per_min', 0.0), ' 局/分')} |")
        add(f"| 时长 | {fmt(summary.get('duration_ms', 0) / 1000.0, ' s')} |")
        add("")
    else:
        add("_未找到 run_summary 行（可能实验未正常结束）。_")
        add("")

    # ---- 2. 分位数表 ----
    add("## 2. 分层延迟分位数")
    add("")
    add("> 口径：从**应到时刻**算起的累计耗时（bot 侧 coordinated-omission 修正后的口径）。")
    add("> 服务端侧的原始分位数看 `result.json` 的 `histograms` 与 Prometheus。")
    add("")

    phases = ["connect", "handshake", "login", "queue_wait", "room_join", "match_start", "match_end"]
    add("| 阶段 | 样本 | p50 | p90 | p99 | max |")
    add("|---|---|---|---|---|---|")
    for ph in phases:
        vals = [v for v in (phase_latency_ms(r, ph) for r in sessions) if v is not None]
        if not vals:
            continue
        s = summarize(vals)
        add(f"| {ph} | {s['count']} | {s['p50']:.0f} ms | {s['p90']:.0f} ms | "
            f"{s['p99']:.0f} ms | {s['max']:.0f} ms |")
    add("")

    # ---- 3. 会话时长分布（保真度主判据） ----
    add("## 3. 会话时长分布（保真度主判据）")
    add("")
    secs = [r.get("session_ms", 0) / 1000.0 for r in sessions if r.get("session_ms", 0) > 0]
    if secs:
        s = summarize(secs)
        add("| 样本 | p50 | p90 | p99 | max | 均值 |")
        add("|---|---|---|---|---|---|")
        add(f"| {s['count']} | {s['p50']:.1f} s | {s['p90']:.1f} s | {s['p99']:.1f} s | "
            f"{s['max']:.1f} s | {s['mean']:.1f} s |")
        add("")
        # 右偏检验：均值/中位 > 1 说明有长尾
        ratio = s["mean"] / s["p50"] if s["p50"] else 0
        add(f"- 均值/中位 = **{ratio:.2f}**：{'存在长尾（右偏）' if ratio > 1.2 else '分布偏窄，长尾不足'}")
        add("")

    # ---- 4. 与参照数据对比 ----
    add("## 4. 与参照数据对比")
    add("")
    if reference:
        ref = [float(x) for x in reference]
        if secs and len(ref) >= 2:
            d, p = ks_2samp(secs, ref)
            w = wasserstein_1d(secs, ref)
            add("| 统计量 | 值 | 判读 |")
            add("|---|---|---|")
            add(f"| KS D | {d:.4f} | 越小越像 |")
            add(f"| KS p | {p:.4f} | p > 0.05 表示「不能拒绝同分布」 |")
            add(f"| Wasserstein-1 | {w:.1f} s | 有量纲，可直接念「差多少秒」 |")
            add("")
            if min(len(secs), len(ref)) < 20:
                add("> ⚠ 样本量偏小（<20），KS 的渐近 p 值不可靠，只能看 D 的量级。")
                add("")
            verdict = "不能拒绝同分布" if p > 0.05 else "**可以拒绝同分布**（bot 与参照不像）"
            add(f"结论：{verdict}。")
            add("")
            add("**能得出什么、不能得出什么**：KS p > 0.05 只说明「在本次样本量下没有证据表明两者不同」，"
                "不等于「证明了像真人」。要写进简历，还需要说明参照数据的来源与采集方式（C12 的边界）。")
            add("")
        else:
            add("_会话样本或参照样本不足，跳过检验。_")
            add("")
    else:
        add("_未提供参照数据（`--reference`）。因此**不能**声称「像真人」——"
            "只能说明分布形状是右偏长尾的。_")
        add("")
        add("参照数据可以这样准备：")
        add("")
        add("```bash")
        add("# 从真实遥测导出一列会话时长（秒），存成 JSON 数组")
        add('echo "[1440.5, 300.2, 960.0, ...]" > analysis/reference.json')
        add("python analysis/analyze.py results/<run-id> --reference analysis/reference.json")
        add("```")
        add("")

    # ---- 5. 结果分布 ----
    add("## 5. 结果与失败路径")
    add("")
    outcomes = Counter(r.get("outcome", "?") for r in sessions)
    add("| 会话结果 | 数量 | 占比 |")
    add("|---|---|---|")
    total = max(len(sessions), 1)
    for name, n in outcomes.most_common():
        add(f"| {name} | {n} | {n / total:.1%} |")
    add("")

    results = Counter(r.get("result") for r in sessions if r.get("result"))
    if results:
        add("| 对局结果 | 数量 |")
        add("|---|---|")
        for name, n in results.most_common():
            add(f"| {name} | {n} |")
        add("")

    personas = defaultdict(list)
    for r in sessions:
        personas[r.get("persona", "?")].append(r.get("session_ms", 0) / 1000.0)
    if personas:
        add("### persona 维度")
        add("")
        add("| persona | 会话数 | 会话时长 p50 | 输入条数均值 | 非法输入 |")
        add("|---|---|---|---|---|")
        for name in sorted(personas):
            rs = [r for r in sessions if r.get("persona") == name]
            add(f"| {name} | {len(rs)} | {quantile(personas[name], 0.5):.1f} s | "
                f"{sum(r.get('inputs_sent', 0) for r in rs) / max(len(rs), 1):.0f} | "
                f"{sum(r.get('invalid_inputs', 0) for r in rs)} |")
        add("")

    # ---- 6. 出图 ----
    series = {}
    if secs:
        xs, ys = ecdf(secs)
        series["bot"] = (xs, ys)
    if reference:
        xs, ys = ecdf([float(x) for x in reference])
        series["reference"] = (xs, ys)
    svg_path = os.path.join(run_dir, "analysis", "cdf.svg")
    os.makedirs(os.path.dirname(svg_path), exist_ok=True)
    if series:
        svg_cdf(series, svg_path, "会话时长 CDF 对比", x_label="秒")
        add("## 6. 分布对比图")
        add("")
        add("![会话时长 CDF](cdf.svg)")
        add("")

    return "\n".join(lines) + "\n"


def load_reference(path):
    """读取参照样本。接受两种格式：

    1. 纯数组：`[180, 240, 300, ...]`（秒）
    2. 带注释的对象：`{"_comment": "...", "session_seconds": [...]}`

    第二种是为了能在文件里写清「这份数据从哪来」，避免三个月后自己都忘了。
    """
    with open(path, "r", encoding="utf-8") as f:
        raw = json.load(f)
    if isinstance(raw, list):
        return [float(x) for x in raw]
    if isinstance(raw, dict):
        for key in ("session_seconds", "samples", "values", "data"):
            if isinstance(raw.get(key), list):
                return [float(x) for x in raw[key]]
    raise ValueError(
        "参照文件必须是 JSON 数组，或是含 session_seconds / samples / values 数组的对象"
    )


def main(argv=None):
    ap = argparse.ArgumentParser(description="压测结果离线分析")
    ap.add_argument("run_dirs", nargs="+", help="results/<run-id> 目录，可给多个")
    ap.add_argument("--reference", help="参照样本 JSON（一列秒数），用于 KS / Wasserstein")
    ap.add_argument("--stdout", action="store_true", help="同时打印到标准输出")
    args = ap.parse_args(argv)

    reference = None
    if args.reference:
        try:
            reference = load_reference(args.reference)
        except (OSError, ValueError) as exc:
            print(f"读取参照数据失败：{exc}", file=sys.stderr)
            return 2

    rc = 0
    for run_dir in args.run_dirs:
        sessions, summary = load_run(run_dir)
        if sessions is None:
            print(f"跳过 {run_dir}：没有 bots.jsonl", file=sys.stderr)
            rc = 1
            continue
        out_dir = os.path.join(run_dir, "analysis")
        os.makedirs(out_dir, exist_ok=True)
        report = build_report(run_dir, sessions, summary, reference)
        report_path = os.path.join(out_dir, "report.md")
        with open(report_path, "w", encoding="utf-8") as f:
            f.write(report)
        print(f"已生成 {report_path}")
        if args.stdout:
            print(report)
    return rc


if __name__ == "__main__":
    sys.exit(main())
