#!/bin/zsh
# 惊蛰看门狗 launchd/cron 入口: 注入凭据 → 运行检查 → 结果落日志
# 独立于 server 进程运行, 专门捕获 "调度器整体沉默" (进程死/机器重启未拉起)
ENV_FILE="$HOME/.jingzhe.env"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "[watchdog] 缺少 $ENV_FILE, 无法注入 JZ_MAIL_PASSWORD, 告警通道不可用" >&2
  exit 2
fi

set -a
source "$ENV_FILE"
set +a

cd /Volumes/zt_hd/projects/jingzhe-trader || exit 2
exec ./bin/watchdog -config config/config.yaml
