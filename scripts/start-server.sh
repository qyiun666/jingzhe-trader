#!/bin/zsh
# 惊蛰服务 launchd 启动脚本
# 职责: 注入环境变量凭据 → 进入项目目录 → exec 前台运行服务 (launchd 需要前台进程才能 KeepAlive)
# 凭据一律来自 ~/.jingzhe.env (权限 600, 已 gitignore), 禁止写入本脚本/代码/配置
ENV_FILE="$HOME/.jingzhe.env"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "[jingzhe] 缺少 $ENV_FILE (参考 scripts/jingzhe.env.example), 无法注入 TUSHARE_TOKEN 等凭据, 拒绝空配置启动" >&2
  exit 1
fi

set -a
source "$ENV_FILE"
set +a

cd /Volumes/zt_hd/projects/jingzhe-trader || exit 1
exec /Volumes/zt_hd/projects/jingzhe-trader/bin/jingzhe-server -config /Volumes/zt_hd/projects/jingzhe-trader/config/config.yaml
