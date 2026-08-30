.PHONY: build test vet clean deps dataloader backtest backtest-macd backtest-boll backtest-multi optimize run email-report backtest-small datasync datasync-full server-small build-small watchdog

# 编译所有二进制
build:
	go build -o bin/dataloader ./cmd/dataloader
	go build -o bin/backtest ./cmd/backtest
	go build -o bin/optimizer ./cmd/optimizer
	go build -o bin/jingzhe-server ./cmd/server
	go build -o bin/watchdog ./cmd/watchdog
	@echo "编译完成: bin/dataloader, bin/backtest, bin/optimizer, bin/jingzhe-server, bin/watchdog"

# 单独编译看门狗 (进程外信号链路截止时间检查)
watchdog:
	go build -o bin/watchdog ./cmd/watchdog

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

# 发送每日报告邮件 (Hermes 18:00 cron 调用)
# 用法: make email-report SMTP_SERVER=smtp.qq.com SMTP_USER=xxx@qq.com SMTP_PASS=xxx TO=you@qq.com REPORT=reports/daily_report_20260716.html
email-report:
	python3 scripts/send_daily_report.py \
		--smtp-server $(SMTP_SERVER) \
		--smtp-port $(or $(SMTP_PORT),465) \
		--username $(SMTP_USER) \
		--password $(SMTP_PASS) \
		--to $(TO) \
		--report $(REPORT)

# 一键运行 (参数: dataloader/backtest/server, 见 scripts/start.sh)
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
	go build -o bin/optimizer ./cmd/optimizer
	go build -o bin/jingzhe-server ./cmd/server
	@echo "编译完成"
