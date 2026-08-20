#!/bin/bash
# 数据库瘦身脚本: 清理全市场历史数据, 只保留近1年
set -e

DB="${1:-data/jingzhe.db}"
if [ ! -f "$DB" ]; then echo "数据库不存在: $DB"; exit 1; fi

BEFORE=$(ls -lh "$DB" | awk '{print $5}')
echo "=== 数据库瘦身开始 ==="
echo "文件: $DB | 大小: $BEFORE"

CUTOFF=$(date -v-1y +%Y%m%d 2>/dev/null || date -d "1 year ago" +%Y%m%d)
echo "清理截止日期: $CUTOFF (删除此日期之前的数据)"
echo ""

echo "--- 清理 daily_bar ---"
sqlite3 "$DB" "DELETE FROM daily_bar WHERE trade_date < '$CUTOFF';"
sqlite3 "$DB" "SELECT COUNT(*) || ' rows remaining' FROM daily_bar;"

echo "--- 清理 daily_basic ---"
sqlite3 "$DB" "DELETE FROM daily_basic WHERE trade_date < '$CUTOFF';"
sqlite3 "$DB" "SELECT COUNT(*) || ' rows remaining' FROM daily_basic;"

echo "--- 清理 stk_limit ---"
sqlite3 "$DB" "DELETE FROM stk_limit WHERE trade_date < '$CUTOFF';"
sqlite3 "$DB" "SELECT COUNT(*) || ' rows remaining' FROM stk_limit;"

echo "--- 清理 moneyflow ---"
sqlite3 "$DB" "DELETE FROM moneyflow WHERE trade_date < '$CUTOFF';" 2>/dev/null || true

echo ""
echo "--- VACUUM 回收空间 ---"
sqlite3 "$DB" "VACUUM;"

AFTER=$(ls -lh "$DB" | awk '{print $5}')
echo ""
echo "=== 瘦身完成 ==="
echo "之前: $BEFORE -> 之后: $AFTER"
