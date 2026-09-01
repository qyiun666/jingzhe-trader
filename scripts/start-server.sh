#!/bin/zsh
# 惊蛰服务 launchd 启动脚本
# 职责: 注入应急凭据覆盖 → 进入项目目录 → exec 前台运行服务 (launchd 需要前台进程才能 KeepAlive)
# 配置与凭据本体都在数据库 data/jingzhe.db 的 config_kv 配置文档内;
# ~/.jingzhe.env 只是"变量非空才顶换库内值"的应急通道 (如库损坏/紧急轮换密钥)
ENV_FILE="$HOME/.jingzhe.env"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "[jingzhe] 无 $ENV_FILE, 使用库内配置的凭据启动" >&2
else
  set -a
  source "$ENV_FILE"
  set +a
fi

# 项目根由脚本自身位置推导: 目录搬家或换挂载点都不用再改这里
# (必须在正确目录下启动 —— 默认库路径 data/jingzhe.db 是相对路径)
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1
exec "$ROOT/bin/jingzhe-server"
