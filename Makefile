.PHONY: build test vet clean dataloader backtest signal trader trader-qmt run backtest-small trader-small datasync datasync-full server-small build-small optimize

# 编译所有二进制
build:
	go build -o bin/dataloader ./cmd/dataloader
	go build -o bin/backtest ./cmd/backtest
	go build -o bin/signal ./cmd/signal
	go build -o bin/trader ./cmd/trader
	@echo "编译完成: bin/dataloader, bin/backtest, bin/signal, bin/trader"

# 数据采集
dataloader:
	go run ./cmd/dataloader -config config/config.yaml

# 回测
backtest:
	go run ./cmd/backtest -config config/config.yaml -strategy ma_cross

# 回测 (MACD策略)
backtest-macd:
	go run ./cmd/backtest -config config/config.yaml -strategy macd

# 回测 (布林带策略)
backtest-boll:
	go run ./cmd/backtest -config config/config.yaml -strategy boll_breakout

# 回测 (多因子策略)
backtest-multi:
	go run ./cmd/backtest -config config/config.yaml -strategy multi_factor

# 参数优化 (均线交叉策略网格搜索, 找最优参数组合)
optimize:
	go run ./cmd/optimizer -config config/config.yaml -strategy ma_cross -start 20240101 -end 20260715 -capital 10000

# 信号生成 (每日模式)
signal-daily:
	go run ./cmd/signal -config config/config.yaml -mode daily -strategy ma_cross

# 信号生成 (批量模式)
signal-batch:
	go run ./cmd/signal -config config/config.yaml -mode batch -strategy ma_cross -start 20240101 -end 20240630

# 纸面交易
trader:
	go run ./cmd/trader -config config/config.yaml -strategy ma_cross

# 纸面交易 (MACD)
trader-macd:
	go run ./cmd/trader -config config/config.yaml -strategy macd

# QMT 实盘 (需先启动 sidecar)
trader-qmt:
	@curl -s http://127.0.0.1:16888/health > /dev/null 2>&1 && \
		go run ./cmd/trader -config config/config.yaml -broker qmt || \
		echo "错误: QMT sidecar 未运行, 请先执行: python scripts/qmt_sidecar.py"

# 操盘手每日报告
captain:
	go run ./cmd/captain -config config/config.yaml -mode daily -date $(DATE)

# 操盘手持仓诊断
captain-diagnose:
	go run ./cmd/captain -config config/config.yaml -mode diagnose

# 操盘手调仓建议
captain-rebalance:
	go run ./cmd/captain -config config/config.yaml -mode rebalance

# 一键运行 (参数: dataloader/backtest/signal/trader/trader-qmt)
run:
	bash scripts/start.sh $(MODE)

# 运行测试
test:
	go test ./internal/... -v -count=1

# 静态检查
vet:
	go vet ./...

# 清理
clean:
	rm -rf bin/ data/ reports/ logs/

# 安装依赖
deps:
	go mod tidy

# ============================================
# 小资金专用命令 (1万本金)
# ============================================

# 小资金回测 (均线交叉)
backtest-small:
	go run ./cmd/backtest -config config/config_small.yaml \
		-strategy ma_cross \
		-capital 10000 \
		-universe "000725.SZ,002230.SZ,002415.SZ,002475.SZ,000001.SZ,600030.SH,000625.SZ,601012.SZ,601899.SH,601318.SH,000333.SZ,600036.SH,600276.SH"

# 小资金模拟盘
trader-small:
	go run ./cmd/trader -config config/config_small.yaml \
		-strategy ma_cross \
		-capital 10000 \
		-broker paper \
		-universe "000725.SZ,002230.SZ,002415.SZ,002475.SZ,000001.SZ,600030.SH,000625.SZ,601012.SZ,601899.SH,601318.SH,000333.SZ,600036.SH,600276.SH"

# 数据采集 (增量更新)
datasync:
	go run ./cmd/dataloader -config config/config_small.yaml

# 数据采集 (含新闻+资金流向+龙虎榜)
datasync-full:
	go run ./cmd/dataloader -config config/config_small.yaml -news -moneyflow -toplist

# 服务启动 (小资金配置)
server-small:
	go run ./cmd/server -config config/config_small.yaml

# 编译所有二进制
build-small:
	go build -o bin/dataloader ./cmd/dataloader
	go build -o bin/backtest ./cmd/backtest
	go build -o bin/signal ./cmd/signal
	go build -o bin/trader ./cmd/trader
	go build -o bin/jingzhe-server ./cmd/server
	@echo "编译完成"
