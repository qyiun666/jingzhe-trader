#!/bin/zsh
# moneyflow 一次性物理收缩: 删按日期的死索引 → 裁到 mf_days 窗口 → VACUUM 回收文件
#
# 为什么不能只靠每日清理: 清理只把页还给 freelist (本库 auto_vacuum=0, 页不会交还 OS),
# 文件体积要等一次 VACUUM 才真正下降; 而 VACUUM 需要独占写连接, 必须先把服务停掉。
# 所以这个脚本只在收盘后的维护窗口跑 (默认 16:30 清理之后、18:00 信号日报之前)。
#
# 用法: scripts/shrink_moneyflow.sh          # 窗口取 config 默认 60 天
#       MF_DAYS=90 scripts/shrink_moneyflow.sh
set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

DB="data/jingzhe.db"
MF_DAYS="${MF_DAYS:-60}"
LABEL="com.jingzhe.trader"
GUI_DOMAIN="gui/$(id -u)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
BACKUP="data/backup/jingzhe-$(date +%Y%m%d-%H%M)-preshrink.db"

# mf_days 要拼进 SQL, 只接受纯数字
[[ "$MF_DAYS" = <-> ]] || { echo "MF_DAYS 必须是天数整数, got: $MF_DAYS" >&2; exit 1; }
[[ -f "$DB" ]] || { echo "数据库不存在: $DB" >&2; exit 1; }
[[ -f "$PLIST" ]] || { echo "launchd 配置不存在: $PLIST" >&2; exit 1; }

now_min=$((10#$(date +%H%M)))
if (( now_min >= 910 && now_min <= 1505 )); then
  echo "拒绝执行: $(date '+%F %T') 在交易时段, 停服务会漏掉盘中止损监控" >&2
  exit 1
fi

# 停在维护状态比库大糟得多: 无论走到哪一步失败, 都要把服务拉回来并确认能响应
trap '
  launchctl bootstrap "$GUI_DOMAIN" "$PLIST" 2>/dev/null || true
  for i in {1..30}; do
    code=$(curl -s -m 5 -o /dev/null -w "%{http_code}" http://127.0.0.1:11270/health || echo 000)
    if [[ "$code" = 200 ]]; then
      echo "服务已恢复 (/health 200)"
      break
    fi
    sleep 2
  done
  if [[ "$code" != 200 ]]; then
    echo "!! 服务未能自动恢复, 需人工检查: launchctl print $GUI_DOMAIN/$LABEL" >&2
  fi
' EXIT

bar_count() { sqlite3 "$DB" 'SELECT COUNT(1) FROM daily_bar'; }

report() {
  echo "--- $1 ---"
  ls -lh "$DB" | awk '{print "  文件:", $5}'
  rows=$(sqlite3 "$DB" 'SELECT COUNT(1) FROM moneyflow')
  days=$(sqlite3 "$DB" 'SELECT COUNT(DISTINCT trade_date) FROM moneyflow')
  span=$(sqlite3 "$DB" 'SELECT MIN(trade_date) || "~" || MAX(trade_date) FROM moneyflow')
  echo "  moneyflow 行数: $rows  交易日: $days  区间: $span"
  echo "  页数: $(sqlite3 "$DB" 'PRAGMA page_count')  空闲页: $(sqlite3 "$DB" 'PRAGMA freelist_count')"
}

report "收缩前"
BAR_BEFORE=$(bar_count)

echo "=== 停服务 ==="
launchctl bootout "$GUI_DOMAIN/$LABEL" 2>/dev/null || true
for i in {1..20}; do
  if ! pgrep -f "$ROOT/bin/jingzhe-server" >/dev/null; then break; fi
  sleep 1
done
if pgrep -f "$ROOT/bin/jingzhe-server" >/dev/null; then
  echo "服务进程仍在, 放弃收缩" >&2
  exit 1
fi

echo "=== 备份 → $BACKUP ==="
sqlite3 "$DB" ".backup $BACKUP"
chmod 600 "$BACKUP"
sqlite3 "$BACKUP" "PRAGMA integrity_check;" | tail -1

CUTOFF=$(date -v-${MF_DAYS}d +%Y%m%d 2>/dev/null || date -d "${MF_DAYS} days ago" +%Y%m%d)
echo "=== 收缩 (保留 trade_date >= $CUTOFF) ==="
sqlite3 "$DB" <<SQL
PRAGMA busy_timeout = 15000;
DROP INDEX IF EXISTS idx_moneyflow_date;
DELETE FROM moneyflow WHERE trade_date < '$CUTOFF';
PRAGMA wal_checkpoint(TRUNCATE);
SQL

echo "=== VACUUM 回收文件 ==="
sqlite3 "$DB" "VACUUM;"

# 只有 moneyflow 该动: 日线行数变了说明窗口算错或语句跑偏, 立刻用备份回滚
if [[ "$(bar_count)" != "$BAR_BEFORE" ]]; then
  echo "!! daily_bar 行数变化, 疑似误删, 用 $BACKUP 回滚正式库" >&2
  cp "$BACKUP" "$DB"
  exit 1
fi

report "收缩后"
sqlite3 "$DB" "SELECT '  moneyflow 索引: '||group_concat(name, ', ') FROM sqlite_master WHERE type='index' AND tbl_name='moneyflow';"
echo "=== 完成, 准备重启服务 ==="
