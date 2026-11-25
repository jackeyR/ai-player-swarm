#!/usr/bin/env python3
"""把 Markdown 转成排版好的 Word 文档。

为什么自己写而不是找现成工具：这份文档的特点是**表格密度极高**（6 张大表），
通用转换器要么把表压成纯文本、要么列宽失控。这里按表头签名给每张表单独分配列宽，
保证 A4 纵向一页能读完一张表。

支持的语法（够这份文档用，不做完整 CommonMark）：
    标题 #/##/###      表格 |a|b|      无序列表 -      有序列表 1.
    引用 >              代码块 ```       分隔线 ---
    行内 **加粗** / `代码` / [文字](链接)

用法：
    python scripts/md2docx.py docs/HANDWRITE.md docs/必须手写的部分.docx
"""

from __future__ import annotations

import datetime
import os
import re
import sys

from docx import Document
from docx.enum.section import WD_SECTION
from docx.enum.table import WD_TABLE_ALIGNMENT
from docx.enum.text import WD_ALIGN_PARAGRAPH, WD_LINE_SPACING
from docx.oxml import OxmlElement
from docx.oxml.ns import qn
from docx.shared import Cm, Pt, RGBColor

# --------------------------------------------------------------------------- #
# 字体与配色
# --------------------------------------------------------------------------- #

FONT_BODY = "DengXian"        # 等线：现代 Office 中文默认，字重均匀
FONT_HEAD = "Microsoft YaHei"  # 微软雅黑：标题更醒目
FONT_MONO = "Consolas"

COLOR_TEXT = RGBColor(0x1A, 0x1A, 0x1A)
COLOR_MUTED = RGBColor(0x5A, 0x5A, 0x5A)
COLOR_HEAD = RGBColor(0x1F, 0x3A, 0x5F)
COLOR_CODE = RGBColor(0x9E, 0x2B, 0x35)
COLOR_LINK = RGBColor(0x1F, 0x5F, 0xB3)

# A4 纵向 + 1.6cm 页边距 => 可用宽度 17.8cm
PAGE_W_CM = 21.0
MARGIN_CM = 1.6
USABLE_CM = PAGE_W_CM - 2 * MARGIN_CM  # 17.8

# 表格列宽：按表头签名匹配，保证每类表在该宽度下不挤
TABLE_WIDTHS = {
    ("#", "要手写什么", "文档出处", "验收", "代码对照"): [0.9, 5.3, 3.6, 4.5, 3.5],
    ("#", "内容", "文档出处"): [0.9, 9.6, 7.3],
    ("#", "命令组", "文档出处"): [0.9, 11.0, 5.9],
    ("#", "要写的", "文档出处", "验收"): [0.9, 6.2, 4.9, 5.8],
    ("模块", "题号", "题目要点"): [3.5, 1.2, 13.1],
    ("#", "问题", "答题抓手（一句话）"): [1.1, 4.9, 11.8],
    # —— CODE-MAP.md ——
    ("文件", "行", "作用", "关键导出"): [4.3, 0.9, 8.2, 4.4],
    ("文件", "行", "作用"): [5.0, 0.9, 11.9],
    ("项", "数量"): [5.2, 12.6],
    ("二进制", "命令", "默认监听", "职责"): [2.1, 5.0, 4.3, 6.4],
    ("步", "动作", "协议", "代码入口"): [0.9, 5.6, 4.3, 7.0],
    ("测试文件", "行", "函数数", "覆盖的关键性质"): [4.6, 0.9, 1.3, 11.0],
    ("顺序", "读什么", "为什么先读它"): [1.3, 6.5, 10.0],
}

HEADER_FILL = "1F3A5F"
HEADER_TEXT = RGBColor(0xFF, 0xFF, 0xFF)
ROW_FILL_ALT = "F5F7FA"


# --------------------------------------------------------------------------- #
# 底层工具
# --------------------------------------------------------------------------- #

def set_east_asian(run, font: str) -> None:
    """python-docx 的 run.font.name 只管西文；中文必须显式写 w:eastAsia。"""
    run.font.name = font
    rpr = run._element.get_or_add_rPr()
    rfonts = rpr.find(qn("w:rFonts"))
    if rfonts is None:
        rfonts = OxmlElement("w:rFonts")
        rpr.append(rfonts)
    rfonts.set(qn("w:eastAsia"), font)
    rfonts.set(qn("w:ascii"), font)
    rfonts.set(qn("w:hAnsi"), font)


def shade(element, fill: str) -> None:
    """给单元格或段落加底色。"""
    pr = element.get_or_add_tcPr() if hasattr(element, "get_or_add_tcPr") else element
    shd = OxmlElement("w:shd")
    shd.set(qn("w:val"), "clear")
    shd.set(qn("w:color"), "auto")
    shd.set(qn("w:fill"), fill)
    pr.append(shd)


def cell_shade(cell, fill: str) -> None:
    shade(cell._tc, fill)


def para_shade(paragraph, fill: str) -> None:
    ppr = paragraph._p.get_or_add_pPr()
    shd = OxmlElement("w:shd")
    shd.set(qn("w:val"), "clear")
    shd.set(qn("w:color"), "auto")
    shd.set(qn("w:fill"), fill)
    ppr.append(shd)


def set_cell_margins(table, top=40, bottom=40, left=70, right=70) -> None:
    tbl_pr = table._tbl.tblPr
    mar = OxmlElement("w:tblCellMar")
    for side, val in (("top", top), ("left", left), ("bottom", bottom), ("right", right)):
        node = OxmlElement(f"w:{side}")
        node.set(qn("w:w"), str(val))
        node.set(qn("w:type"), "dxa")
        mar.append(node)
    tbl_pr.append(mar)


def repeat_header(row) -> None:
    """跨页时重复表头。长表格不重复表头，打印出来根本对不上列。"""
    tr_pr = row._tr.get_or_add_trPr()
    node = OxmlElement("w:tblHeader")
    node.set(qn("w:val"), "true")
    tr_pr.append(node)


def no_split(row) -> None:
    tr_pr = row._tr.get_or_add_trPr()
    node = OxmlElement("w:cantSplit")
    node.set(qn("w:val"), "true")
    tr_pr.append(node)


def fixed_layout(table) -> None:
    """固定列宽布局。python-docx 的 autofit=False 已经写过 tblLayout，
    这里只保证它存在且为 fixed，不重复追加（重复元素会让 Word 提示修复）。"""
    tbl_pr = table._tbl.tblPr
    existing = tbl_pr.find(qn("w:tblLayout"))
    if existing is None:
        existing = OxmlElement("w:tblLayout")
        tbl_pr.append(existing)
    existing.set(qn("w:type"), "fixed")


# --------------------------------------------------------------------------- #
# OOXML 顺序归一化
#
# Word 对 pPr / tcPr / trPr / tblPr 的子元素**顺序**是有要求的（ECMA-376 里
# 这些都是 xsd:sequence，不是 all）。python-docx 只管自己创建的元素，
# 我们手工 append 的 shd / pBdr / tblCellMar 会跑到末尾，顺序错了轻则样式不生效，
# 重则 Word 打开时弹「此文件需要修复」。所以最后统一按 schema 顺序重排。
# --------------------------------------------------------------------------- #

PPR_ORDER = [
    "pStyle", "keepNext", "keepLines", "pageBreakBefore", "framePr", "widowControl",
    "numPr", "suppressLineNumbers", "pBdr", "shd", "tabs", "suppressAutoHyphens",
    "kinsoku", "wordWrap", "overflowPunct", "topLinePunct", "autoSpaceDE",
    "autoSpaceDN", "bidi", "adjustRightInd", "snapToGrid", "spacing", "ind",
    "contextualSpacing", "mirrorIndents", "suppressOverlap", "jc", "textDirection",
    "textAlignment", "textboxTightWrap", "outlineLvl", "divId", "cnfStyle",
    "rPr", "sectPr", "pPrChange",
]

TCPR_ORDER = [
    "cnfStyle", "tcW", "gridSpan", "hMerge", "vMerge", "tcBorders", "shd",
    "noWrap", "tcMar", "textDirection", "tcFitText", "vAlign", "hideMark",
    "headers", "cellIns", "cellDel", "cellMerge", "tcPrChange",
]

TRPR_ORDER = [
    "cnfStyle", "divId", "gridBefore", "gridAfter", "wBefore", "wAfter",
    "cantSplit", "trHeight", "tblHeader", "tblCellSpacing", "jc", "hidden",
    "ins", "del", "trPrChange",
]

TBLPR_ORDER = [
    "tblStyle", "tblpPr", "tblOverlap", "bidiVisual", "tblStyleRowBandSize",
    "tblStyleColBandSize", "tblW", "jc", "tblCellSpacing", "tblInd", "tblBorders",
    "shd", "tblLayout", "tblCellMar", "tblLook", "tblCaption", "tblDescription",
    "tblPrChange",
]

_NORMALIZE = {
    "pPr": PPR_ORDER,
    "tcPr": TCPR_ORDER,
    "trPr": TRPR_ORDER,
    "tblPr": TBLPR_ORDER,
}


def normalize_order(doc: Document) -> None:
    """按 schema 顺序重排并去重 pPr / tcPr / trPr / tblPr 的子元素。"""
    body = doc.element.body
    for tag, order in _NORMALIZE.items():
        for pr in body.iter(qn(f"w:{tag}")):
            children = list(pr)
            if not children:
                continue
            # 同名元素只保留最后一个（后写的是最终意图）
            latest = {}
            for ch in children:
                latest[ch.tag.split("}")[-1]] = ch
            for ch in children:
                pr.remove(ch)
            rank = {name: i for i, name in enumerate(order)}
            for name in sorted(latest, key=lambda n: rank.get(n, len(order))):
                pr.append(latest[name])



# --------------------------------------------------------------------------- #
# 行内格式：**加粗** / `代码` / [文字](链接)
# --------------------------------------------------------------------------- #

INLINE_RE = re.compile(r"(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\([^)]+\))")


def add_inline(paragraph, text: str, size=10.0, base_bold=False, color=COLOR_TEXT):
    """把一行 Markdown 行内语法写进段落。

    加粗分支会**递归**再解析一次：`**` + 行内代码 + `**` 这种嵌套很常见
    （比如「**`internal/ws/`** — 说明」），不递归就会把反引号原样印进 Word。
    """
    for token in INLINE_RE.split(text):
        if not token:
            continue
        if token.startswith("**") and token.endswith("**") and len(token) > 4:
            add_inline(paragraph, token[2:-2], size=size, base_bold=True, color=color)
        elif token.startswith("`") and token.endswith("`") and len(token) > 2:
            run = paragraph.add_run(token[1:-1])
            set_east_asian(run, FONT_MONO)
            run.font.size = Pt(size - 1)
            run.bold = base_bold
            run.font.color.rgb = COLOR_CODE
        elif token.startswith("[") and "](" in token:
            label = token[1:].split("]")[0]
            # 链接文字里可能再套一层行内代码标记（[`a.md`](a.md)），去掉再说
            label = label.strip("`").strip("*")
            # Word 里放相对路径的超链接会指向文档所在目录之外的仓库文件，
            # 打印稿/转发场景一律是死链，所以只保留文字，样式上标成路径色。
            run = paragraph.add_run(label)
            set_east_asian(run, FONT_MONO)
            run.font.size = Pt(size - 1)
            run.bold = base_bold
            run.font.color.rgb = COLOR_LINK
        else:
            run = paragraph.add_run(token)
            run.bold = base_bold
            set_east_asian(run, FONT_BODY)
            run.font.size = Pt(size)
            run.font.color.rgb = color


# --------------------------------------------------------------------------- #
# 文档骨架
# --------------------------------------------------------------------------- #

def setup_document() -> Document:
    doc = Document()

    section = doc.sections[0]
    section.page_width = Cm(PAGE_W_CM)
    section.page_height = Cm(29.7)
    section.left_margin = Cm(MARGIN_CM)
    section.right_margin = Cm(MARGIN_CM)
    section.top_margin = Cm(1.8)
    section.bottom_margin = Cm(1.8)

    normal = doc.styles["Normal"]
    normal.font.name = FONT_BODY
    normal.font.size = Pt(10)
    normal.font.color.rgb = COLOR_TEXT
    normal.element.rPr.rFonts.set(qn("w:eastAsia"), FONT_BODY)
    pf = normal.paragraph_format
    pf.space_before = Pt(0)
    pf.space_after = Pt(4)
    pf.line_spacing_rule = WD_LINE_SPACING.MULTIPLE
    pf.line_spacing = 1.15

    # 页脚页码
    footer = section.footer
    fp = footer.paragraphs[0]
    fp.alignment = WD_ALIGN_PARAGRAPH.CENTER
    fld = OxmlElement("w:fldSimple")
    fld.set(qn("w:instr"), "PAGE")
    inner = OxmlElement("w:r")
    text = OxmlElement("w:t")
    text.text = "1"
    inner.append(text)
    fld.append(inner)
    fp._p.append(fld)

    # 三号标题样式统一在这里定，正文里只管调用
    for name, size in (("Heading 1", 15), ("Heading 2", 12.5), ("Heading 3", 11)):
        st = doc.styles[name]
        st.font.name = FONT_HEAD
        st.font.size = Pt(size)
        st.font.bold = True
        st.font.color.rgb = COLOR_HEAD
        st.element.rPr.rFonts.set(qn("w:eastAsia"), FONT_HEAD)
        st.paragraph_format.space_before = Pt(12 if name == "Heading 1" else 8)
        st.paragraph_format.space_after = Pt(4)
        st.paragraph_format.keep_with_next = True

    return doc


def add_title_block(doc: Document, title: str, subtitle_lines) -> None:
    p = doc.add_paragraph()
    p.paragraph_format.space_after = Pt(2)
    run = p.add_run(title)
    set_east_asian(run, FONT_HEAD)
    run.font.size = Pt(21)
    run.bold = True
    run.font.color.rgb = COLOR_HEAD

    for line in subtitle_lines:
        sp = doc.add_paragraph()
        sp.paragraph_format.space_after = Pt(1)
        add_inline(sp, line, size=9, color=COLOR_MUTED)

    # 封面下方的细线
    rule = doc.add_paragraph()
    rule.paragraph_format.space_after = Pt(10)
    ppr = rule._p.get_or_add_pPr()
    borders = OxmlElement("w:pBdr")
    bottom = OxmlElement("w:bottom")
    bottom.set(qn("w:val"), "single")
    bottom.set(qn("w:sz"), "8")
    bottom.set(qn("w:space"), "1")
    bottom.set(qn("w:color"), "1F3A5F")
    borders.append(bottom)
    ppr.append(borders)


def add_heading(doc: Document, text: str, level: int) -> None:
    p = doc.add_heading(level=level)
    run = p.add_run(text)
    set_east_asian(run, FONT_HEAD)
    size = {1: 15, 2: 12.5, 3: 11}[level]
    run.font.size = Pt(size)
    run.bold = True
    run.font.color.rgb = COLOR_HEAD
    p.paragraph_format.keep_with_next = True


def add_bullet(doc: Document, text: str, numbered: bool = False) -> None:
    style = "List Number" if numbered else "List Bullet"
    p = doc.add_paragraph(style=style)
    p.paragraph_format.space_after = Pt(2)
    p.paragraph_format.line_spacing = 1.1
    add_inline(p, text)


def add_quote(doc: Document, lines) -> None:
    """引用块：浅底 + 缩进。块内的 `- ` 视为项目符号，
    否则会渲染成「- 《文档名》」这种把符号也印出来的样子。"""
    for line in lines:
        if not line:
            continue
        p = doc.add_paragraph()
        p.paragraph_format.left_indent = Cm(0.6)
        p.paragraph_format.space_after = Pt(2)
        para_shade(p, "F2F4F7")
        if line.startswith("- "):
            p.paragraph_format.left_indent = Cm(1.0)
            bullet = p.add_run("•  ")
            set_east_asian(bullet, FONT_BODY)
            bullet.font.size = Pt(9.5)
            bullet.font.color.rgb = COLOR_MUTED
            add_inline(p, line[2:], size=9.5, color=COLOR_MUTED)
        else:
            add_inline(p, line, size=9.5, color=COLOR_MUTED)


def add_code_block(doc: Document, lines) -> None:
    for line in lines:
        p = doc.add_paragraph()
        p.paragraph_format.left_indent = Cm(0.4)
        p.paragraph_format.space_after = Pt(0)
        p.paragraph_format.line_spacing = 1.0
        para_shade(p, "F6F8FA")
        run = p.add_run(line if line else " ")
        set_east_asian(run, FONT_MONO)
        run.font.size = Pt(9)
        run.font.color.rgb = RGBColor(0x24, 0x29, 0x2F)


def add_rule(doc: Document) -> None:
    p = doc.add_paragraph()
    p.paragraph_format.space_before = Pt(6)
    p.paragraph_format.space_after = Pt(6)
    ppr = p._p.get_or_add_pPr()
    borders = OxmlElement("w:pBdr")
    bottom = OxmlElement("w:bottom")
    bottom.set(qn("w:val"), "single")
    bottom.set(qn("w:sz"), "6")
    bottom.set(qn("w:space"), "1")
    bottom.set(qn("w:color"), "D0D7DE")
    borders.append(bottom)
    ppr.append(borders)


def add_table(doc: Document, rows) -> None:
    header, body = rows[0], rows[1:]
    ncols = len(header)
    widths = TABLE_WIDTHS.get(tuple(header))
    if widths is None:
        widths = [USABLE_CM / ncols] * ncols

    table = doc.add_table(rows=1, cols=ncols)
    table.style = "Table Grid"
    table.alignment = WD_TABLE_ALIGNMENT.CENTER
    table.autofit = False
    fixed_layout(table)
    set_cell_margins(table)

    # 表头
    hdr = table.rows[0]
    repeat_header(hdr)
    no_split(hdr)
    for i, text in enumerate(header):
        cell = hdr.cells[i]
        cell.width = Cm(widths[i])
        cell_shade(cell, HEADER_FILL)
        p = cell.paragraphs[0]
        p.paragraph_format.space_after = Pt(0)
        p.paragraph_format.line_spacing = 1.0
        add_inline(p, text, size=9, base_bold=True, color=HEADER_TEXT)

    # 数据行
    for ri, row in enumerate(body):
        tr = table.add_row()
        no_split(tr)
        for ci in range(ncols):
            cell = tr.cells[ci]
            cell.width = Cm(widths[ci])
            if ri % 2 == 1:
                cell_shade(cell, ROW_FILL_ALT)
            p = cell.paragraphs[0]
            p.paragraph_format.space_after = Pt(0)
            p.paragraph_format.line_spacing = 1.0
            text = row[ci] if ci < len(row) else ""
            add_inline(p, text, size=9)

    # 列宽再写一遍：Word 会按第一行宽度回填，逐格设置才不会错位
    for row in table.rows:
        for i, cell in enumerate(row.cells):
            cell.width = Cm(widths[i])

    # 表后留白，避免与下一段贴死
    doc.add_paragraph().paragraph_format.space_after = Pt(2)


# --------------------------------------------------------------------------- #
# 主流程
# --------------------------------------------------------------------------- #

def split_row(line: str):
    line = line.strip()
    if line.startswith("|"):
        line = line[1:]
    if line.endswith("|"):
        line = line[:-1]
    return [c.strip() for c in line.split("|")]


def convert(md_path: str, docx_path: str, title: str | None = None, subtitle=None) -> None:
    with open(md_path, encoding="utf-8") as f:
        lines = f.read().split("\n")

    # 标题默认取第一个一级标题。**不要写死**——写死的后果是第二份文档
    # 拿第一份文档的标题当封面，而且因为封面是 Normal 样式，肉眼检查很容易漏掉。
    if not title:
        for line in lines:
            m = re.match(r"^#\s+(.*)$", line.strip())
            if m:
                title = m.group(1).strip()
                break
        else:
            title = os.path.splitext(os.path.basename(md_path))[0]

    if subtitle is None:
        subtitle = [
            f"源文件：`{os.path.basename(md_path)}`　·　"
            f"生成日期：{datetime.date.today().isoformat()}",
            "由 `scripts/md2docx.py` 生成；与源文件同源，改了 md 重跑一次即可同步。",
        ]

    doc = setup_document()
    add_title_block(doc, title, subtitle)

    i = 0
    n = len(lines)
    first_h1_done = False

    while i < n:
        line = lines[i]
        stripped = line.strip()

        # 空行
        if not stripped:
            i += 1
            continue

        # 代码块
        if stripped.startswith("```"):
            i += 1
            block = []
            while i < n and not lines[i].strip().startswith("```"):
                block.append(lines[i])
                i += 1
            i += 1  # 跳过结束围栏
            add_code_block(doc, block)
            continue

        # 表格
        if stripped.startswith("|") and i + 1 < n and set(lines[i + 1].strip()) <= set("|-: "):
            block = [split_row(line)]
            i += 2  # 跳过表头与分隔行
            while i < n and lines[i].strip().startswith("|"):
                block.append(split_row(lines[i]))
                i += 1
            add_table(doc, block)
            continue

        # 分隔线
        if stripped in ("---", "***", "___"):
            add_rule(doc)
            i += 1
            continue

        # 标题
        m = re.match(r"^(#{1,3})\s+(.*)$", stripped)
        if m:
            level = len(m.group(1))
            text = m.group(2).strip()
            if level == 1:
                # 一级标题已作封面，正文里不再重复
                first_h1_done = True
                i += 1
                continue
            add_heading(doc, text, level)
            i += 1
            continue

        # 引用块（连续多行合并为一个块）
        if stripped.startswith(">"):
            block = []
            while i < n and lines[i].strip().startswith(">"):
                block.append(lines[i].strip().lstrip(">").strip())
                i += 1
            add_quote(doc, [b for b in block if b])
            continue

        # 有序列表
        m = re.match(r"^(\d+)\.\s+(.*)$", stripped)
        if m:
            add_bullet(doc, m.group(2), numbered=True)
            i += 1
            continue

        # 无序列表
        if re.match(r"^[-*]\s+", stripped):
            add_bullet(doc, re.sub(r"^[-*]\s+", "", stripped))
            i += 1
            continue

        # 普通段落
        p = doc.add_paragraph()
        add_inline(p, stripped)
        i += 1

    os.makedirs(os.path.dirname(os.path.abspath(docx_path)) or ".", exist_ok=True)
    normalize_order(doc)
    doc.save(docx_path)


if __name__ == "__main__":
    import argparse

    ap = argparse.ArgumentParser(description="把 Markdown 转成排版好的 Word")
    ap.add_argument("input", help="输入 .md")
    ap.add_argument("output", help="输出 .docx")
    ap.add_argument("--title", help="封面标题（默认取第一个一级标题）")
    ap.add_argument("--subtitle", action="append", help="封面副标题，可重复")
    args = ap.parse_args()

    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

    convert(args.input, args.output, args.title, args.subtitle)
    print(f"已生成 {args.output}")
