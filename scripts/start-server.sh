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

cd /Volumes/zt_hd/projects/jingzhe-trader || exit 1
exec /Volumes/zt_hd/projects/jingzhe-trader/bin/jingzhe-server
