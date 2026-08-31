#!/bin/bash
# chaogu 一键启动脚本
# 用法: ./scripts/start.sh [dataloader|backtest|server] [数据库路径]
# 配置与行情都在同一个库里; 省略路径时取 JZ_DB_PATH 或 data/jingzhe.db

MODE=${1:-server}
DB=${2:-}

# 统一入口: 给了路径才追加 -db, 避免把用户输入拼进命令行求值
run_pkg() {
    local pkg="$1"
    if [[ -n "$DB" ]]; then
        go run "$pkg" -db "$DB"
    else
        go run "$pkg"
    fi
}

case $MODE in
    dataloader)
        echo "启动数据采集..."
        run_pkg ./cmd/dataloader
        ;;
    backtest)
        echo "启动回测..."
        run_pkg ./cmd/backtest
        ;;
    server)
        echo "启动交易服务..."
        run_pkg ./cmd/server
        ;;
    *)
        echo "用法: $0 [dataloader|backtest|server] [数据库路径]"
        exit 1
        ;;
esac
