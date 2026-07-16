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

> 如果你是 AI Agent（如 TRAE / HermesAgent / Cursor Agent 等），下载本仓库后按以下步骤操作即可。
>
> **核心定位**：Agent 是指挥官，负责分析数据、生成交易指令、推送给用户。用户是执行者，按指令操作后反馈结果，Agent 调接口更新持仓。

### 1. 部署启动

```bash
# 克隆仓库
git clone https://github.com/qyiun666/jingzhe-trader.git
cd jingzhe-trader

# 安装依赖
go mod tidy

# 复制配置模板并填入密钥
cp config/config.example.yaml config/config.yaml
# 编辑 config.yaml，填入 Tushare Token 和 LLM API Key

# 编译
make build-small

# 启动 API 服务（后台常驻）
./bin/jingzhe-server -config config/config.yaml &
```

服务启动后监听 `http://localhost:11270`，Agent 所有操作通过 HTTP API 完成。

### 2. 数据管理

#### 2.1 每日数据采集

```bash
# 增量同步（每个交易日收盘后执行）
./bin/dataloader -config config/config.yaml

# 含新闻+资金流向+龙虎榜
./bin/dataloader -config config/config.yaml -news -moneyflow -toplist

# 同步财务指标（每季度一次）
./bin/dataloader -config config/config.yaml -fina
```

#### 2.2 筛选模式（节省空间）

在 `config.yaml` 中开启筛选模式，只拉取股票池+持仓的行情数据，数据量减少 99%：

```yaml
dataloader:
  filter_mode: false        # true=只拉关注股票, false=全量
  watchlist: []             # 额外关注代码，如: ["600519.SH"]
  enable_limit: true        # 涨跌停价（风控需要，建议开）
  enable_basic: true        # 每日基本面（多因子策略需要，建议开）
  enable_fund: true         # ETF/基金日线
```

#### 2.3 清理多余数据

切换到筛选模式后，清理已积累的无用数据（删除不在股票池和持仓中的股票）：

```bash
./bin/dataloader -config config/config.yaml -cleanup
```

执行后自动 VACUUM 回收磁盘空间。**注意**：cleanup 会直接删除数据，确认股票池配置正确后再执行。

### 3. 每日调度计划

| 时间 | 操作 | 命令/API | 说明 |
|---|---|---|---|
| **15:30** | 数据采集 | `./bin/dataloader -config config/config.yaml` | 收盘后同步当日行情 |
| **15:35** | 生成操盘报告 | `make captain date=YYYYMMDD` | 生成每日操盘报告 |
| **15:40** | 获取交易指令 | `GET /api/rebalance?date=YYYYMMDD` | 获取买卖建议 |
| **15:45** | 推送指令 | 飞书 Webhook | 把买卖指令推送给用户 |
| **按需** | 交易反馈 | `POST /api/trade/confirm` | 用户操作完成后更新持仓 |
| **16:00** | 持仓诊断 | `make captain-diagnose` | 检查持仓风控状态 |

### 4. 交易指令闭环（核心流程）

**角色分工**：
- **Agent** = 指挥官：分析数据，生成具体交易指令（买/卖/设条件单/撤条件单）
- **用户** = 执行者：按指令在交易软件操作，完成后反馈结果
- **系统** = 账本：通过 API 记录交易，更新持仓和现金

**流程**：

```
1. Agent 跑 captain daily → 生成买卖建议
2. Agent 从 /api/rebalance 获取结构化指令
3. Agent 推送指令给用户（飞书/通知），格式：
   "买入 510050.SH 500股 限价3.05"
   "卖出 510300.SH 200股 市价"
   "510050.SH 设条件单：跌到2.90自动买入500股"
   "撤掉 510050.SH 的条件单"
4. 用户操作完成后反馈："510050买完了，500股，3.048成交"
5. Agent 调 POST /api/trade/confirm 更新持仓
6. 下次报告基于最新持仓
```

**条件单说明**：条件单在用户的交易软件上设置和管理，系统不跟踪条件单状态。条件单触发成交后，用户反馈成交结果，Agent 调 `/api/trade/confirm` 更新即可。

### 5. API 接口

#### 5.1 接口总览

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/health` | GET | 健康检查 |
| `/api/daily` | GET | 每日操盘报告（汇总：持仓+调仓+市场+策略） |
| `/api/positions` | GET | 持仓列表+诊断（盈亏、风控、集中度） |
| `/api/rebalance` | GET | 调仓建议（买入/卖出/持有列表） |
| `/api/strategy` | GET | 策略建议（推荐操作方向） |
| `/api/strategy/status` | GET | 动态策略状态（当前策略+参数） |
| `/api/market` | GET | 市场概况（指数+涨跌+板块） |
| `/api/kline` | GET | K线数据（`?code=510050.SH&start=20260101&end=20260716`） |
| `/api/snapshots` | GET | 账户快照历史（`?limit=30`） |
| `/api/news` | GET | 新闻列表 |
| `/api/news/llm` | GET | LLM 深度新闻分析（`?limit=5`） |
| `/api/portfolio` | GET | 获取持仓列表（原始格式） |
| `/api/portfolio/sync` | POST | 批量同步持仓（覆盖式） |
| `/api/trade/confirm` | POST | 交易反馈（单笔成交确认） |
| `/api/system/status` | GET | 系统状态（数据新鲜度+健康度） |
| `/api/system/update-data` | POST | 手动触发数据更新 |

#### 5.2 交易反馈接口

用户操作完成后，Agent 调用此接口更新持仓：

```bash
# 买入反馈
curl -s -X POST "http://localhost:11270/api/trade/confirm" \
  -H "Content-Type: application/json" \
  -d '{"ts_code": "510050.SH", "side": "buy", "qty": 500, "price": 3.048}'
```

```bash
# 卖出反馈
curl -s -X POST "http://localhost:11270/api/trade/confirm" \
  -H "Content-Type: application/json" \
  -d '{"ts_code": "510300.SH", "side": "sell", "qty": 200, "price": 4.75}'
```

**参数说明**：
- `ts_code`：股票代码（如 `510050.SH`）
- `side`：`buy` 或 `sell`
- `qty`：成交数量（必须 100 的整数倍）
- `price`：实际成交价格

**响应**：
```json
{
  "code": 0,
  "msg": "ok",
  "data": {
    "ts_code": "510050.SH",
    "side": "buy",
    "qty": 500,
    "price": 3.048,
    "amount": 1524.0,
    "cash": 8476.0,
    "total_asset": 10000.0
  }
}
```

系统自动处理：加权平均成本、现金增减、持仓数量更新。

#### 5.3 批量同步持仓

当需要从外部系统一次性导入完整持仓时使用（覆盖式，会清空旧持仓）：

```bash
curl -s -X POST "http://localhost:11270/api/portfolio/sync" \
  -H "Content-Type: application/json" \
  -d '{
    "positions": [
      {"ts_code": "510050.SH", "total_qty": 500, "available_qty": 500, "cost_price": 3.059},
      {"ts_code": "510300.SH", "total_qty": 200, "available_qty": 200, "cost_price": 4.936}
    ],
    "cash": 5000.0
  }'
```

#### 5.4 获取调仓建议

```bash
curl -s "http://localhost:11270/api/rebalance?date=20260716"
```

**响应结构**：
```json
{
  "code": 0,
  "data": {
    "sell_list": [
      {"ts_code": "510300.SH", "qty": 200, "price": 4.75, "reason": "死叉卖出"}
    ],
    "buy_list": [
      {"ts_code": "510050.SH", "qty": 500, "price": 3.05, "reason": "金叉买入"}
    ],
    "hold_list": [
      {"ts_code": "510050.SH", "qty": 500, "suggestion": "继续持有"}
    ],
    "cash_pct": 0.15,
    "reason": "调仓2只"
  }
}
```

### 6. 飞书推送配置

在 `config/config.yaml` 中填入飞书 Webhook URL：

```yaml
feishu:
  webhook_url: "https://open.feishu.cn/open-apis/bot/v2/hook/xxxxxx"
  push_daily: true
  push_time: "15:30"
```

获取方式：飞书群设置 → 添加机器人 → 自定义机器人 → 复制 Webhook 地址。

**推送格式建议**（Agent 组装后发送）：

```
【交易指令 2026-07-16】

卖出：
  510300.SH  200股  市价  （死叉卖出）

买入：
  510050.SH  500股  限价3.05  （金叉买入）

持有：
  510050.SH  500股  继续持有

现金占比：15%
操作完成后请回复成交结果。
```

### 7. Captain 命令

Captain 是操盘手工具，提供三种模式：

```bash
# 每日操盘报告（生成 HTML 报告 + 保存账户快照）
make captain date=20260716

# 持仓诊断（检查止损/止盈/集中度）
make captain-diagnose

# 调仓建议（生成买卖列表）
make captain-rebalance
```

每个模式执行后会自动保存 `account_snapshot`（账户快照），用于绘制资产曲线。

### 8. 回测验证（部署前建议跑一遍）

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
