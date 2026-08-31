#!/bin/bash
# ============================================================
# NAS 健康检查与自愈脚本
# 用法: 配合 cron 每 10 分钟执行一次
#   */10 * * * * /opt/jingzhe-trader/scripts/nas_health_check.sh >> /opt/jingzhe-trader/logs/health_check.log 2>&1
#
# 功能:
#   1. 检查 server 进程是否存活, 挂了则自动重启
#   2. 检查数据是否过期, 过期则触发数据更新
#   3. 检查 API 是否可访问
# ============================================================

# ===== 配置 (按实际部署路径修改) =====
INSTALL_DIR="/opt/jingzhe-trader"
BINARY="${INSTALL_DIR}/bin/jingzhe-server"
DATA_LOADER="${INSTALL_DIR}/bin/dataloader"
DB_PATH="${INSTALL_DIR}/data/jingzhe.db"
LOG_DIR="${INSTALL_DIR}/logs"
API_URL="http://127.0.0.1:11270"

# ===== 工具函数 =====
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1"
}

alert() {
    log "ALERT: $1"
}

# 进程匹配按可执行文件路径: 启动参数已不含 config.yaml, 用旧模式会永远误判为已死
server_running() {
    pgrep -f "bin/jingzhe-server" > /dev/null 2>&1
}

# ===== 1. 检查 server 进程 =====
check_server_process() {
    if server_running; then
        log "OK: server process running"
        return 0
    fi

    log "WARN: server process not running, restarting..."
    cd "${INSTALL_DIR}" || return 1
    nohup "${BINARY}" -db "${DB_PATH}" > "${LOG_DIR}/server.out.log" 2>&1 &
    sleep 3

    if server_running; then
        log "OK: server restarted"
    else
        alert "server restart failed! Check: ${LOG_DIR}/server.out.log"
        return 1
    fi
}

# ===== 2. 检查 API 可访问性 =====
check_api() {
    if ! curl -s --max-time 5 "${API_URL}/health" > /dev/null 2>&1; then
        alert "API unreachable (${API_URL}/health)"
        return 1
    fi
    log "OK: API accessible"
    return 0
}

# ===== 3. 检查数据新鲜度 =====
check_data_freshness() {
    local today=$(date '+%Y%m%d')

    local max_bar_date=$(sqlite3 "${DB_PATH}" "SELECT MAX(trade_date) FROM daily_bar;" 2>/dev/null)

    if [ -z "$max_bar_date" ]; then
        alert "No daily bar data in database!"
        return 1
    fi

    # 检查今天是否在日历中
    local today_in_cal=$(sqlite3 "${DB_PATH}" "SELECT COUNT(1) FROM trade_cal WHERE cal_date='${today}';" 2>/dev/null)

    if [ "$today_in_cal" = "0" ]; then
        log "WARN: today not in trade calendar, triggering calendar sync..."
        cd "${INSTALL_DIR}" || return 1
        "${DATA_LOADER}" -db "${DB_PATH}" > "${LOG_DIR}/dataloader_auto.log" 2>&1
        log "OK: data sync done (calendar fix)"
        return 0
    fi

    # 检查今天是否交易日
    local is_open=$(sqlite3 "${DB_PATH}" "SELECT is_open FROM trade_cal WHERE cal_date='${today}';" 2>/dev/null)

    if [ "$is_open" = "1" ]; then
        if [ "$max_bar_date" \< "$today" ]; then
            local hour=$(date '+%H')
            if [ "$hour" -ge "15" ]; then
                log "WARN: data stale (latest: ${max_bar_date}, today: ${today}), triggering update..."
                cd "${INSTALL_DIR}" || return 1
                "${DATA_LOADER}" -db "${DB_PATH}" > "${LOG_DIR}/dataloader_auto.log" 2>&1
                local new_max=$(sqlite3 "${DB_PATH}" "SELECT MAX(trade_date) FROM daily_bar;" 2>/dev/null)
                if [ "$new_max" \> "$max_bar_date" ]; then
                    log "OK: data updated (latest: ${new_max})"
                else
                    alert "data still stale after update (latest: ${new_max}), Tushare API may be down"
                fi
            else
                log "INFO: trade day, data up to ${max_bar_date} (will update after close)"
            fi
        else
            log "OK: data fresh (${max_bar_date})"
        fi
    else
        log "INFO: non-trading day, data up to ${max_bar_date}"
    fi
    return 0
}

# ===== 主流程 =====
log "========== NAS Health Check =========="
check_server_process
check_api
check_data_freshness
log "========== Check Complete =========="
