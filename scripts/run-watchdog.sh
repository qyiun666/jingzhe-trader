#!/bin/zsh
# 惊蛰看门狗 launchd/cron 入口: 注入应急凭据覆盖 → 运行检查 → 结果落日志
# 独立于 server 进程运行, 专门捕获 "调度器整体沉默" (进程死/机器重启未拉起)
# 凭据本体存在数据库 config_kv 的配置文档内; 下面的 env 文件只是"非空才顶换"的应急通道
ENV_FILE="$HOME/.jingzhe.env"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "[watchdog] 无 $ENV_FILE, 使用库内配置的凭据继续" >&2
else
  set -a
  source "$ENV_FILE"
  set +a
fi

# 项目根由脚本自身位置推导: 目录搬家或换挂载点都不用再改这里
# (看门狗同样要读 data/jingzhe.db, 该默认路径是相对路径)
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 2
exec "$ROOT/bin/watchdog"
