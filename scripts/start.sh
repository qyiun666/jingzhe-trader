#!/bin/bash
# chaogu 一键启动脚本
# 用法: ./scripts/start.sh [dataloader|backtest|server]

MODE=${1:-server}
CONFIG=${2:-config/config.yaml}

case $MODE in
    dataloader)
        echo "启动数据采集..."
        go run ./cmd/dataloader -config $CONFIG
        ;;
    backtest)
        echo "启动回测..."
        go run ./cmd/backtest -config $CONFIG
        ;;
    server)
        echo "启动交易服务..."
        go run ./cmd/server -config $CONFIG
        ;;
    *)
        echo "用法: $0 [dataloader|backtest|server]"
        exit 1
        ;;
esac
