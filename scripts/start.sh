#!/bin/bash
# chaogu 一键启动脚本
# 用法: ./scripts/start.sh [dataloader|backtest|signal|trader]

MODE=${1:-trader}
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
    signal)
        echo "启动信号引擎..."
        go run ./cmd/signal -config $CONFIG -mode daily
        ;;
    trader)
        echo "启动交易程序..."
        go run ./cmd/trader -config $CONFIG -broker paper
        ;;
    trader-qmt)
        echo "启动 QMT 实盘交易..."
        # 检查 sidecar 是否运行
        if ! curl -s http://127.0.0.1:16888/health > /dev/null 2>&1; then
            echo "错误: QMT sidecar 未运行, 请先启动 python scripts/qmt_sidecar.py"
            exit 1
        fi
        go run ./cmd/trader -config $CONFIG -broker qmt
        ;;
    *)
        echo "用法: $0 [dataloader|backtest|signal|trader|trader-qmt]"
        exit 1
        ;;
esac
