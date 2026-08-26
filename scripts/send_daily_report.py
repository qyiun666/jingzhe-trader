#!/usr/bin/env python3
"""
send_daily_report.py - 将 captain 生成的每日操盘报告通过邮件发送

用法:
    python3 scripts/send_daily_report.py \
        --smtp-server smtp.qq.com \
        --smtp-port 465 \
        --username your@qq.com \
        --password your_smtp_password \
        --to recipient@example.com \
        --report reports/daily_report_20260716.html \
        [--subject "惊蛰操盘报告 2026-07-16"]

SMTP 密码说明:
    QQ邮箱: 使用"授权码"(在QQ邮箱设置→账户→POP3/SMTP服务→生成授权码)
    163邮箱: 使用"客户端授权密码"
    Gmail: 使用"应用专用密码"
"""

import argparse
import os
import re
import smtplib
import sys
from datetime import datetime
from email.mime.multipart import MIMEMultipart
from email.mime.text import MIMEText

# RFC 5322 限制单行 ≤998 字符, QQ 邮箱超限拒收 (留余量)
MAX_MAIL_LINE = 990
# 邮件精简规则: 邮件只发结果性内容, 去掉网页展示用噪音
MAX_TABLE_ROWS = 25       # 数据行超过该值的表格触发截断
KEEP_TABLE_ROWS = 15      # 截断后保留的数据行数 (不含表头)
MAIL_BODY_WIDTH = "680px" # 网页版容器 1200px+ 在邮件中过宽

# 邮件兼容兜底样式: 部分邮箱不支持 CSS Grid, 用 Flex 降级 (置于 </head> 前覆盖原样式)
MAIL_FALLBACK_STYLE = """<style>
.grid-4, .grid-3, .summary { display: flex; flex-wrap: wrap; gap: 12px; }
.grid-4 > *, .grid-3 > *, .summary > * { flex: 1 1 200px; }
</style>
"""


def sanitize_for_mail(html):
    """邮件精简: 去 JS 图表 (邮件不执行 JS, 无法渲染), 截断超长表格, 容器宽度收窄

    完整报告是网页展示用; 邮件只保留结果性内容 (指标/清单/结论),
    删掉图表数据与完整明细这些纯噪音。
    """
    # 1. 删除 script 块 (外链 chart.js 与内联 navData 图表数据)
    html = re.sub(r"<script\b[^>]*>.*?</script>", "", html, flags=re.S | re.I)
    html = re.sub(r"<script\b[^>]*/>", "", html, flags=re.I)
    # 2. 删除 canvas 图表载体, 以及空壳的图表区块 (邮件无法渲染图表)
    html = re.sub(r"<canvas\b[^>]*>.*?</canvas>", "", html, flags=re.S | re.I)
    html = re.sub(r"<canvas\b[^>]*/>", "", html, flags=re.I)
    html = re.sub(r"<div class=\"section\">\s*<h2>净值曲线</h2>.*?</div>", "", html, flags=re.S)
    # 3. 容器宽度压到邮件友好范围
    html = re.sub(r"max-width:\s*\d{3,4}px", "max-width: %s" % MAIL_BODY_WIDTH, html)
    # 4. 截断超长表格: 只保留表头 + 前 KEEP_TABLE_ROWS 行
    html = _truncate_long_tables(html)
    # 5. 回测报告标题条美化 (captain 报告的 h1 在渐变 header 内, 不处理)
    html = html.replace(
        "<h1>回测报告",
        '<h1 style="padding:14px 18px;background:linear-gradient(135deg,#1a3c6e,#2a5298);color:#fff;border-radius:8px;font-size:20px;">回测报告',
    )
    return html


def _truncate_long_tables(html):
    """截断超长表格: 保留表头 + 前 KEEP_TABLE_ROWS 行, 末尾加提示行"""
    def repl(m):
        table = m.group(0)
        trs = re.findall(r"<tr[^>]*>.*?</tr>", table, flags=re.S | re.I)
        if len(trs) <= MAX_TABLE_ROWS:
            return table
        # 按是否含 <th> 区分表头行与数据行 (避免 tbody 内重复表头)
        heads = [t for t in trs if re.search(r"<th\b", t, flags=re.I)]
        data = [t for t in trs if not re.search(r"<th\b", t, flags=re.I)]
        if not heads:
            heads, data = trs[:1], trs[1:]
        ncols = len(re.findall(r"<t[hd][^>]*>", heads[0], flags=re.I)) or 1
        keep_rows = data[:KEEP_TABLE_ROWS]
        note = ('<tr><td colspan="%d" style="padding:8px;color:#98a2b3;font-size:12px;text-align:center;">'
                '… 共 %d 条记录, 完整明细见网页版报告</td></tr>') % (ncols, len(data))
        tb = re.search(r"<tbody[^>]*>.*?</tbody>", table, flags=re.S | re.I)
        if tb:
            # 表头在 <thead> 中原样保留; 无 <thead> 时表头才进 tbody
            rows = keep_rows if re.search(r"<thead", table, flags=re.I) else heads[:1] + keep_rows
            return table[:tb.start()] + "<tbody>" + "".join(rows) + note + "</tbody>" + table[tb.end():]
        cleaned = re.sub(r"<tr[^>]*>.*?</tr>", "", table, flags=re.S | re.I)
        return cleaned.replace("</table>", "".join(heads[:1] + keep_rows) + note + "</table>")

    return re.sub(r"<table\b[^>]*>.*?</table>", repl, html, flags=re.S | re.I)


def inject_mail_style(html):
    """注入邮件兼容样式 (Grid → Flex 兜底), 置于 </head> 前保证覆盖原样式"""
    if re.search(r"</head>", html, flags=re.I):
        return re.sub(r"</head>", MAIL_FALLBACK_STYLE + "</head>", html, count=1, flags=re.I)
    return MAIL_FALLBACK_STYLE + html


def _safe_cut(line, max_line):
    """安全断点: 优先空格, 其次标签闭合符 > 之后 (避免切断 HTML 标签), 最后硬折"""
    cut = line.rfind(" ", 0, max_line)
    if cut < 1:
        cut = line.rfind(">", 0, max_line)
        if cut < 1:
            cut = max_line
        else:
            cut += 1
    return cut


def fold_html_lines(html, max_line=MAX_MAIL_LINE):
    """折叠超长行: 优先在空格/标签边界断行, 无安全位置时硬折

    HTML/JS 中插入换行会被解析为空白, 不影响渲染;
    避免切断 HTML 标签 (如 </tr>), 否则破坏页面结构。
    Python str 按 Unicode 码点索引, 不会切断多字节字符。
    """
    lines = []
    for line in html.split("\n"):
        while len(line) > max_line:
            cut = _safe_cut(line, max_line)
            lines.append(line[:cut])
            line = line[cut:].lstrip(" ")
        lines.append(line)
    return "\n".join(lines)


def send_report(smtp_server, smtp_port, username, password, to_addr, report_path, subject=None):
    """发送 HTML 报告邮件"""

    # 读取 HTML 报告
    if not os.path.exists(report_path):
        print(f"错误: 报告文件不存在: {report_path}", file=sys.stderr)
        sys.exit(1)

    with open(report_path, "r", encoding="utf-8") as f:
        html_content = f.read()

    # 邮件化: 去噪音 (JS 图表/超长表格) → 兼容样式 → 折叠超长行 (避免 QQ 拒收)
    html_content = sanitize_for_mail(html_content)
    html_content = inject_mail_style(html_content)
    html_content = fold_html_lines(html_content)

    # 默认主题
    if subject is None:
        date_str = datetime.now().strftime("%Y-%m-%d")
        subject = f"惊蛰操盘报告 {date_str}"

    # 构建邮件
    msg = MIMEMultipart("alternative")
    msg["Subject"] = subject
    msg["From"] = f"惊蛰交易助手 <{username}>"
    msg["To"] = to_addr

    # 纯文本备选 (邮件客户端不支持 HTML 时显示)
    text_content = f"惊蛰操盘报告 - {subject}\n\n请查看 HTML 附件或在线版本。"

    msg.attach(MIMEText(text_content, "plain", "utf-8"))
    msg.attach(MIMEText(html_content, "html", "utf-8"))

    # 发送
    try:
        if smtp_port == 465:
            # SSL
            server = smtplib.SMTP_SSL(smtp_server, smtp_port, timeout=30)
        else:
            # STARTTLS
            server = smtplib.SMTP(smtp_server, smtp_port, timeout=30)
            server.starttls()

        server.login(username, password)
        server.sendmail(username, to_addr, msg.as_string())
        server.quit()

        print(f"邮件发送成功: {subject}")
        print(f"  收件人: {to_addr}")
        print(f"  报告: {report_path}")

    except Exception as e:
        print(f"邮件发送失败: {e}", file=sys.stderr)
        sys.exit(1)


def main():
    parser = argparse.ArgumentParser(description="发送惊蛰每日操盘报告邮件")
    parser.add_argument("--smtp-server", required=True, help="SMTP 服务器地址")
    parser.add_argument("--smtp-port", type=int, default=465, help="SMTP 端口 (默认 465=SSL)")
    parser.add_argument("--username", required=True, help="发件邮箱")
    parser.add_argument("--password", required=True, help="SMTP 授权码/密码")
    parser.add_argument("--to", required=True, help="收件邮箱")
    parser.add_argument("--report", required=True, help="报告 HTML 文件路径")
    parser.add_argument("--subject", default=None, help="邮件主题 (默认自动生成)")

    args = parser.parse_args()
    send_report(
        smtp_server=args.smtp_server,
        smtp_port=args.smtp_port,
        username=args.username,
        password=args.password,
        to_addr=args.to,
        report_path=args.report,
        subject=args.subject,
    )


if __name__ == "__main__":
    main()
