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
import smtplib
import sys
from datetime import datetime
from email.mime.multipart import MIMEMultipart
from email.mime.text import MIMEText


def send_report(smtp_server, smtp_port, username, password, to_addr, report_path, subject=None):
    """发送 HTML 报告邮件"""

    # 读取 HTML 报告
    if not os.path.exists(report_path):
        print(f"错误: 报告文件不存在: {report_path}", file=sys.stderr)
        sys.exit(1)

    with open(report_path, "r", encoding="utf-8") as f:
        html_content = f.read()

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
