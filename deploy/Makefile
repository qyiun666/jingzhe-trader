# 惊蛰 (Jingzhe Trader) 构建脚本（Batch 1: 基础设施与存储地基）
# 使用: make build / make test / make jingzhed / make jingzhectl / make db-check

BINARY_DIR := bin
DB ?= data/jingzhe.db

.PHONY: all build test vet fmt jingzhed jingzhectl db-check clean

all: build

build: jingzhed jingzhectl

jingzhed:
	go build -o $(BINARY_DIR)/jingzhed ./cmd/jingzhed

jingzhectl:
	go build -o $(BINARY_DIR)/jingzhectl ./cmd/jingzhectl

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -w .

# 自检：打开库并打印全部配置（凭据默认掩码）
db-check: jingzhectl
	./$(BINARY_DIR)/jingzhectl -db $(DB) config dump

clean:
	rm -rf $(BINARY_DIR)
