#!/bin/zsh
# 安装/更新 macOS launchd 常驻任务 (server + watchdog)
#
# 为什么需要这个脚本: plist 里的路径必须是绝对路径, 写死在仓库里就会在目录搬家/换挂载点后
# 静默失效 —— 上次 /Volumes/zt_hd 消失后, 服务连续几天没自启, 行情与信号链路整体停摆,
# 而任务状态看起来一切正常。因此仓库里的 plist 只是带 __JZ_ROOT__ 占位符的模板,
# 由本脚本按"当前项目根"渲染后再装载。搬家后重跑本脚本即可。
#
# 用法: scripts/install-launchd.sh          # 安装或更新并立即启动
#       scripts/install-launchd.sh stop     # 卸载装载 (保留文件, 停止常驻)
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AGENT_DIR="$HOME/Library/LaunchAgents"
DOMAIN="gui/$(id -u)"
LABELS=(com.jingzhe.trader com.jingzhe.watchdog)

bootout_all() {
  for label in $LABELS; do
    launchctl bootout "$DOMAIN/$label" 2>/dev/null && echo "[launchd] 已卸载 $label"
  done
}

case "${1:-install}" in
  stop)
    bootout_all
    exit 0
    ;;
  install) ;;
  *)
    echo "用法: $0 [install|stop]" >&2
    exit 2
    ;;
esac

missing=0
for bin in jingzhe-server watchdog; do
  [[ -x "$ROOT/bin/$bin" ]] || { echo "  缺 $ROOT/bin/$bin" >&2; missing=1; }
done
if (( missing )); then
  echo "[launchd] 先编译: make build" >&2
  exit 1
fi

mkdir -p "$AGENT_DIR" "$ROOT/logs"
bootout_all

for label in $LABELS; do
  tmpl="$ROOT/scripts/$label.plist"
  dst="$AGENT_DIR/$label.plist"
  if [[ ! -f "$tmpl" ]]; then
    echo "[launchd] 模板缺失: $tmpl" >&2
    exit 1
  fi
  # 始终从模板渲染: 便宜且幂等, 模板改动会在下次安装时生效
  sed "s#__JZ_ROOT__#$ROOT#g" "$tmpl" > "$dst"

  # 同名任务的 .disabled 残留: 内容就是本任务的旧副本, 留着只会让人误启错的那份
  stale="$AGENT_DIR/$label.plist.disabled"
  if [[ -f "$stale" ]]; then
    echo "[launchd] 移除残留 $stale"
    rm -f "$stale"
  fi

  # bootout 返回时 launchd 往往还在拆任务, 紧接着 bootstrap 同一个 label 会报
  # "Bootstrap failed: 5: Input/output error" —— 必须等它彻底消失再装, 并留重试余地
  for _ in {1..25}; do
    launchctl print "$DOMAIN/$label" >/dev/null 2>&1 || break
    sleep 0.2
  done
  ok=0
  for _ in {1..5}; do
    launchctl bootstrap "$DOMAIN" "$dst" 2>/dev/null && { ok=1; break; }
    sleep 1
  done
  if (( ! ok )); then
    echo "[launchd] 装载失败: $dst" >&2
    exit 1
  fi
  launchctl enable "$DOMAIN/$label"
  echo "[launchd] 已装载并启动 $label"
done

sleep 2
for label in $LABELS; do
  if ! launchctl print "$DOMAIN/$label" >/dev/null 2>&1; then
    echo "[launchd] $label 未装载成功, 查日志: $ROOT/logs/launchd-stderr.log" >&2
    exit 1
  fi
  # 只有常驻任务平时持有 PID; watchdog 是 StartCalendarInterval 定时任务, 无 PID 属正常
  pid=$(launchctl list "$label" 2>/dev/null | awk -F'= ' '/"PID"/{gsub(/[^0-9]/,"",$2); print $2}')
  echo "[launchd] $label 已装载${pid:+ (当前 PID $pid)}"
done
echo "[launchd] 项目根: $ROOT"
