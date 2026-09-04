# 惊蛰 (Jingzhe Trader) 构建脚本
# 使用: make build / make test / make itest / make vet / make db-check / make jobs

BINARY_DIR := bin
DB ?= data/jingzhe.db

.PHONY: all build test itest vet fmt jingzhe db-check jobs clean

all: build

build: jingzhe

# 单一二进制：常驻服务（调度器 + MCP 接口）与运维 CLI 合一
jingzhe:
	go build -o $(BINARY_DIR)/jingzhe ./cmd/jingzhe

test:
	go test ./... -count=1

# 集成测试：真的打 Tushare / DeepSeek / SMTP / gotdx / MCP 接口。
# 凭据由启动方注入，本目标不替你加载 .env：
#   set -a; . ./.env; set +a
#   make -f deploy/Makefile itest                 # 锚定最近一个已同步交易日
#   make -f deploy/Makefile itest DATE=20260903   # 指定交易日
# 会真发信到 watch.mail_to、真计费问模型 —— 这是它存在的意义。
itest:
	JZ_ITEST=1 $(if $(DATE),JZ_ITEST_DATE=$(DATE)) go test ./itest -count=1 -timeout 25m -v

vet:
	go vet ./...

fmt:
	gofmt -w .

# 自检：打开库并打印全部配置（凭据默认掩码）
db-check: jingzhe
	./$(BINARY_DIR)/jingzhe -db $(DB) config dump

# 演练当日调度时间线（dry-run，不真正执行任务）
jobs: jingzhe
	./$(BINARY_DIR)/jingzhe -db $(DB) jobs

clean:
	rm -rf $(BINARY_DIR)
