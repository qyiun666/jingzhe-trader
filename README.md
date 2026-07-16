# 惊蛰 (Jingzhe Trader)

> 蛰伏待击 — A 股量化交易系统，专为小资金设计

[![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

## 简介

**惊蛰 (Jingzhe)** 是一个基于 Go 语言的 A 股量化交易系统。名字取自二十四节气"惊蛰"——春雷惊醒蛰伏的昆虫，寓意**长时间观望等待，时机到了果断出手**。

专为**小资金**（1 万本金起）优化，强调**低频、集中、精准**的交易风格，用冷酷的规则代替冲动的人性。

## 核心特点

- **小资金友好** — 1 万本金即可运行，手续费优化，集中持仓
- **多策略支持** — 均线交叉 / MACD / 布林带突破 / 多因子选股 / 日内做T
- **动态策略选择** — 根据市场环境自动切换最优策略
- **自适应参数** — 根据 ATR 波动率自动调整均线周期、止损止盈、仓位
- **LLM 辅助** — 集成 DeepSeek 等大模型，深度分析新闻舆情
- **完整链路** — 数据采集 → 回测验证 → 模拟盘 → 实盘(QMT)
- **飞书推送** — 每日操盘报告自动推送到飞书

## 回测表现

| 策略 | 区间 | 总收益 | 年化 | 夏普 | 最大回撤 |
|---|---|---|---|---|---|
| **均线交叉 (3/25)** | 2024.01 ~ 2026.07 | **+67.25%** | **23.59%** | **1.11** | 12.72% |
| 多因子 | 2024.01 ~ 2026.07 | +25.66% | 9.86% | 0.50 | 17.51% |
| 均线交叉 | 2026.04 ~ 2026.07 | +17.37% | 76.56% | 1.74 | 15.65% |

> 数据基于 13 只低价活跃股回测，手续费按真实万 0.85 佣金 + 万 5 印花税计算。

## 快速开始

### 1. 克隆仓库

```bash
git clone https://github.com/qyiun666/jingzhe-trader.git
cd jingzhe-trader
```

### 2. 安装依赖

```bash
go mod tidy
```

### 3. 配置

```bash
cp config/config.example.yaml config/config.yaml
# 编辑 config.yaml，填入你的 Tushare Token 和 LLM API Key
```

- **Tushare Token**: 从 [tushare.pro](https://tushare.pro/register.html) 注册获取（500 元/年档）
- **LLM API Key** (可选): 支持 DeepSeek / 通义千问 / 智谱等 OpenAI 兼容接口

### 4. 采集数据

```bash
# 下载行情数据 (首次运行需要较长时间)
make datasync

# 下载财务指标数据
make datasync-full
```

### 5. 回测验证

```bash
make backtest-small
```

### 6. 启动服务

```bash
make server-small
```

服务启动后访问 `http://localhost:11270`。

## 项目架构

```
jingzhe-trader/
├── cmd/                  # 命令行入口
│   ├── server/           # HTTP API 服务
│   ├── dataloader/       # 数据采集
│   ├── backtest/         # 回测引擎
│   ├── trader/           # 模拟/实盘交易
│   ├── signal/           # 信号计算
│   └── optimizer/        # 参数优化
├── internal/
│   ├── api/              # HTTP API 路由和处理器
│   ├── backtest/         # 回测引擎核心
│   ├── broker/           # 券商接口 (Paper / QMT)
│   ├── config/           # 配置管理
│   ├── factor/           # 选股因子 (价值/质量/成长/动量/情绪)
│   ├── indicator/        # 技术指标 (MA/MACD/ATR/RSI/BOLL)
│   ├── llm/              # LLM 客户端 (新闻分析/选股建议)
│   ├── market/           # 市场数据和交易成本
│   ├── model/            # 数据模型
│   ├── risk/             # 风控引擎
│   ├── signal/           # 信号系统
│   ├── store/            # 数据持久化 (SQLite)
│   ├── strategy/         # 策略引擎
│   ├── tushare/          # Tushare 数据接口
│   └── analysis/         # 策略分析和建议
├── config/               # 配置文件
├── data/                 # SQLite 数据库 (gitignored)
├── logs/                 # 日志文件 (gitignored)
├── reports/              # 回测报告 (gitignored)
└── Makefile              # 快捷命令
```

## AI Agent 使用指南

> 如果你是 AI Agent（如 HermesAgent / Cursor Agent 等），下载本仓库后按以下步骤操作即可。

### 1. 启动服务

```bash
# 克隆仓库
git clone https://github.com/qyiun666/jingzhe-trader.git
cd jingzhe-trader

# 安装依赖
go mod tidy

# 复制配置模板并填入密钥
cp config/config.example.yaml config/config.yaml
# 编辑 config.yaml，填入 Tushare Token 和 LLM API Key

# 启动服务
make server-small
```

服务启动后监听 `http://localhost:11270`。

### 2. 每日调度计划

| 时间 | 操作 | API | 说明 |
|---|---|---|---|
| **08:50** | 晨报 | `GET /api/daily?date=YYYYMMDD` | 获取今日操盘建议 |
| **08:55** | 飞书推送 | 调用飞书 Webhook | 将报告内容推送 |
| **11:35** | 午盘 | `GET /api/market?date=YYYYMMDD` | 市场概况 + 持仓诊断 |
| **15:30** | 收盘报告 | `GET /api/daily?date=YYYYMMDD` | 收盘后最终建议 |
| **15:35** | 飞书推送 | 调用飞书 Webhook | 推送收盘报告 |
| **按需** | 交易反馈 | `POST /api/trade/confirm` | 用户操作后反馈 |

### 3. API 调用示例

**获取每日操盘报告：**
```bash
curl -s "http://localhost:11270/api/daily?date=20260716"
```

**同步用户持仓：**
```bash
curl -s -X POST "http://localhost:11270/api/portfolio/sync" \
  -H "Content-Type: application/json" \
  -d '{
    "positions": [
      {"ts_code": "600519.SH", "total_qty": 100, "available_qty": 100, "cost_price": 1800},
      {"ts_code": "000858.SZ", "total_qty": 300, "available_qty": 300, "cost_price": 150.5}
    ],
    "cash": 500000
  }'
```

**交易反馈（买入）：**
```bash
curl -s -X POST "http://localhost:11270/api/trade/confirm" \
  -H "Content-Type: application/json" \
  -d '{"ts_code": "002049.SZ", "side": "buy", "qty": 200, "price": 68.50}'
```

**LLM 深度新闻分析：**
```bash
curl -s "http://localhost:11270/api/news/llm?limit=5"
```

### 4. 飞书推送配置

在 `config/config.yaml` 中填入你的飞书 Webhook URL：

```yaml
feishu:
  webhook_url: "https://open.feishu.cn/open-apis/bot/v2/hook/xxxxxx"
  push_daily: true
  push_time: "15:30"
```

获取方式：飞书群设置 → 添加机器人 → 自定义机器人 → 复制 Webhook 地址。

### 5. 完整工作流

```
1. 每天早上 08:50，Agent 调用 /api/daily 获取晨报
2. 解析报告中的 action_items（买入/卖出建议）
3. 通过飞书 Webhook 推送 formatted 报告给用户
4. 用户看到报告后决定是否操作
5. 用户操作后，Agent 调用 /api/trade/confirm 反馈成交信息
6. 系统更新持仓，下次报告基于真实持仓
7. 收盘后 15:30 再次推送收盘报告
```

### 6. 回测验证（部署前建议跑一遍）

```bash
make backtest-small    # 跑回测验证策略有效性
make optimize          # 参数网格搜索，找最优参数
```

## 策略说明

### 均线交叉 (ma_cross) — 推荐策略

- **短均线**: 3 日，**长均线**: 25 日（经网格搜索优化）
- 金叉买入，死叉卖出
- 含 4 重信号过滤：成交量确认、趋势强度、大盘环境、冷却期
- 自适应参数：根据波动率动态调整均线周期和仓位

### 多因子选股 (multi_factor)

- 5 大类因子：价值(PE/PB) + 质量(ROE/毛利率) + 成长(净利润同比) + 动量(60日涨幅) + 情绪(换手率/量比/涨跌停)
- 周度调仓，每次只选 top 3
- 适合震荡市和弱趋势市场

### 日内做T (intraday_t)

- 利用底仓做日内高抛低吸
- 自动评估：波动率够不够、做T划不划算
- 震荡市自动推荐

## API 接口

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/health` | GET | 健康检查 |
| `/api/daily` | GET | 每日操盘报告 |
| `/api/portfolio` | GET | 获取持仓列表 |
| `/api/portfolio/sync` | POST | 同步真实持仓 |
| `/api/trade/confirm` | POST | 交易反馈确认 |
| `/api/strategy/status` | GET | 动态策略状态 |
| `/api/news/llm` | GET | LLM 深度新闻分析 |
| `/api/system/status` | GET | 系统全面状态 |

## Makefile 快捷命令

```bash
make build-small        # 编译所有二进制
make server-small       # 启动服务
make backtest-small     # 小资金回测
make trader-small       # 小资金模拟盘
make datasync           # 数据采集
make datasync-full      # 全量数据采集(含新闻/财务)
make optimize           # 策略参数网格搜索
```

## 免责声明

本项目仅供学习和研究使用，不构成任何投资建议。股市有风险，投资需谨慎。使用本系统进行实盘交易产生的任何盈亏由使用者自行承担。

## License

[MIT](LICENSE)
