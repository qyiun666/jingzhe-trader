# 惊蛰 (Jingzhe Trader)

> 蛰伏待击 — A 股量化交易系统，专为小资金设计

[![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

## 简介

**惊蛰 (Jingzhe)** 是一个基于 Go 语言的 A 股量化交易系统。名字取自二十四节气"惊蛰"——春雷惊醒蛰伏的昆虫，寓意**长时间观望等待，时机到了果断出手**。

专为**小资金**（1 万本金起）优化，强调**低频、集中、精准**的交易风格，用冷酷的规则代替冲动的人性。

## 架构

`cmd/server` 是唯一常驻进程，内置调度器在交易日自动完成全链路，结果全部落 SQLite。
AI Agent（如 Hermes）只需定时 GET 只读 API 拿现成结果，POST 确认执行。

```
                      ┌──────────────────────────────────────────────┐
                      │              cmd/server (常驻)                │
                      │                                              │
  Tushare ──────────▶ │  调度器 (交易日自动执行)                       │
  腾讯免费行情 ──────▶ │   15:10 数据更新 (进程内 dataloader)          │
  QMT sidecar ◀─────▶ │   15:30 EOD信号 → trade_plan 表              │
                      │   15:35 对账 (QMT模式)                        │
                      │   15:45 日报生成 + 飞书推送                    │
                      │   盘中每5分钟 实时价止损监控 → 紧急计划+告警     │
                      │   16:30 数据保留清理 + WAL checkpoint          │
                      │                                              │
                      │  统一执行管道 engine.Pipeline                  │
                      │   信号 → 风控 → Broker下单 → 落库              │
                      │   (回测/模拟/实盘共用, 只换 Broker 实现)        │
                      └───────────────┬──────────────────────────────┘
                                      │ SQLite (WAL)
                      ┌───────────────┴──────────────────────────────┐
                      │              HTTP API (Bearer 鉴权)           │
                      └───────────────┬──────────────────────────────┘
                                      │
                 Agent/人: GET /api/agent/brief → POST /api/plan/confirm
```

### Agent 对接流程

1. `GET /api/agent/brief` — 一次拿到全量上下文：待处理交易计划、持仓诊断、账户、市场概况、数据新鲜度、任务健康度
2. Agent/人审阅计划后 `POST /api/plan/confirm {"id": 123}` 确认
3. `trading.auto_execute=true` 且 `broker.type=qmt` 时确认即真实下单，否则仅标记 confirmed 由人工执行后 `POST /api/trade/confirm` 反馈成交

详细用法见下方 [Agent 接入指南](#agent-接入指南hermes--任意-ai-agent)。

## 核心特点

- **小资金友好** — 1 万本金即可运行；按资金量级自适应持仓数与最小交易额（5 元最低佣金下默认单笔 ≥5000 元保证费率 ≤0.1%）
- **回测即实盘** — 回测/模拟/实盘共用同一条 `信号 → 风控 → 下单 → 落库` 管道，回测结果不虚高
- **风控内建** — 止损/止盈信号优先执行、单票/总仓位/板块敞口限制、含手续费的买入资金检查
- **全自动闭环** — 内置调度器：数据更新 → EOD 信号 → 对账 → 日报飞书推送 → 盘中止损监控 → 数据自动清理
- **常驻稳定** — 任务 panic 隔离、job_run 防重复/启动补跑、优雅关机、WAL checkpoint、goroutine 纪律
- **多策略支持** — 均线交叉 / MACD / 布林带突破 / 多因子选股，动态策略选择器按市况切换
- **LLM 辅助** — 集成 DeepSeek 等大模型深度分析新闻舆情（可选）

## 快速开始

### 1. 编译

```bash
git clone https://github.com/qyiun666/jingzhe-trader.git
cd jingzhe-trader
go build -o bin/server ./cmd/server
go build -o bin/backtest ./cmd/backtest
go build -o bin/dataloader ./cmd/dataloader
go build -o bin/optimizer ./cmd/optimizer
```

### 2. 配置

```bash
cp config/config.example.yaml config/config.yaml
```

**密钥一律走环境变量，不要写进配置文件**（配置文件中的同名项会被环境变量覆盖）：

```bash
export TUSHARE_TOKEN=你的tushare token       # 必需, 行情数据源
export JZ_API_TOKEN=随机长字符串              # 推荐, API写接口鉴权
export FEISHU_WEBHOOK=飞书机器人webhook       # 可选, 日报/告警推送
export LLM_API_KEY=deepseek密钥              # 可选, 新闻分析
export QMT_SIDECAR_TOKEN=随机长字符串         # QMT实盘时, sidecar鉴权
```

> ⚠️ 如果你的 token 曾经写在配置文件里提交过 git，请立即到对应平台**轮换**。

### 3. 拉取数据 & 回测

```bash
bin/dataloader -config config/config.yaml          # 首次约拉3年数据
bin/backtest -config config/config.yaml            # 回测并生成HTML报告
```

### 4. 启动常驻服务

```bash
bin/server -config config/config.yaml
# 默认监听 127.0.0.1:8080, 调度器自动运行
curl http://127.0.0.1:8080/api/health              # 健康检查
curl http://127.0.0.1:8080/api/agent/brief         # Agent 全量上下文
```

## 小资金配置指南（1 万元档）

`config.example.yaml` 默认即为 1 万资金调好的参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `risk.max_position_pct` | 0.6 | 单票上限 60%（小资金必须集中） |
| `risk.max_total_position_pct` | 1.0 | 总仓位可满仓 |
| `risk.stop_loss_pct` | 0.05 | 止损 -5% |
| `risk.take_profit_pct` | 0.10 | 止盈 +10% |
| `trading.min_trade_amount` | 0 | 0=自适应：最低佣金/0.1%（5元→5000元），低于该额的买入直接拒绝 |
| `trading.max_positions` | 0 | 0=自适应：<5万→2只，<20万→4只，否则6只 |
| `trading.auto_execute` | false | 默认只生成计划，人/Agent 确认后执行 |
| `cost.min_commission` | 5.0 | 按你的券商实际佣金档修改 |

资金量变大后无需改代码：把 `backtest.initial_capital` 改成实际资金，自适应参数自动放宽；也可以手动指定 `trading.*` 覆盖自适应。

## API 一览

只读接口（GET，无需 token）：

| 端点 | 说明 |
|---|---|
| `/api/agent/brief` | **Agent 首选**：计划+持仓+市场+健康度一次拿全 |
| `/api/plan?date=` | 交易计划列表（不传 date 返回全部待处理） |
| `/api/daily?date=` | 每日操盘报告（汇总） |
| `/api/positions` | 持仓诊断 |
| `/api/market` / `/api/news` / `/api/strategy` | 市场概况 / 新闻舆情 / 策略建议 |
| `/api/reconcile?date=` | 本地 vs 券商对账 |
| `/api/health` | uptime / goroutine数 / db大小 / 各任务最近成功时间 |
| `/api/system/status` | 数据新鲜度 / 持仓数 / 下一交易日 |

写接口（POST，配置 `server.api_token` 后需 `Authorization: Bearer <token>`）：

| 端点 | 说明 |
|---|---|
| `/api/plan/confirm` | 确认交易计划 `{"id": 123}` |
| `/api/trade/confirm` | 人工成交后反馈 `{"ts_code","side","qty","price"}` |
| `/api/portfolio/sync` | 同步真实持仓（`overwrite: false` 为增量 Upsert） |
| `/api/system/update-data` | 手动触发数据更新 |

## Agent 接入指南（Hermes / 任意 AI Agent）

本系统是 Agent 的“数据 + 执行后端”：调度器每个交易日自动完成数据更新→信号生成→盘中监控，结果全部落库；Agent 只需定时读取现成结果、审批计划、反馈成交，不需要自己算任何指标。

### 鉴权

```bash
export JZ_API_TOKEN="your-random-token"    # 服务端: config 留空则从环境变量 JZ_API_TOKEN 读取
# Agent 侧所有 POST 请求带头: Authorization: Bearer $JZ_API_TOKEN  (GET 无需 token)
```

### 推荐轮询节奏（交易日）

| 时间 | Agent 动作 | 说明 |
|---|---|---|
| 15:35 后 | `GET /api/agent/brief` | 调度器 15:10 更新数据、15:30 生成计划，此时计划已就绪 |
| 审阅后 | `POST /api/plan/confirm` | 逐条确认要执行的计划 |
| 成交后 | `POST /api/trade/confirm` | 人工/券商成交后反馈，保持持仓同步 |
| 盘中（可选） | `GET /api/plan` | 盘中止损监控产生的 urgent 计划会实时出现在这里（同时飞书告警） |
| 任意 | `GET /api/health` | 存活与任务健康度巡检 |

### 1. 读取全量上下文

```bash
curl http://NAS_IP:8080/api/agent/brief
```

响应字段（`data`）：

```jsonc
{
  "date": "20260726",           // 数据基准日
  "data_last_date": "20260724", // 库内最新行情日
  "data_fresh": true,            // false 时应提醒用户数据滞后, 不宜盲信计划
  "open_plans": [{               // 待处理交易计划 (pending/confirmed)
    "id": 12, "trade_date": "20260726",
    "ts_code": "000001.SZ", "name": "平安银行",
    "direction": "buy", "qty": 500, "ref_price": 11.13,
    "reason": "均线金叉: MA3=11.05上穿MA25=10.98",
    "strategy": "ma_cross", "urgency": "normal",  // urgent=止损类, 优先处理
    "status": "pending"
  }],
  "portfolio": { /* 持仓明细+健康分+集中度+盈亏/风险指标 */ },
  "market":    { /* 指数涨跌/市场情绪概况 */ },
  "jobs":      { "signal": "2026-07-26 15:30:02", "data_update": "..." },
  "warnings":  ["..."]           // 数据滞后/任务失败等异常, 非空时优先告知用户
}
```

### 2. 确认执行计划

```bash
curl -X POST http://NAS_IP:8080/api/plan/confirm \
  -H "Authorization: Bearer $JZ_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"id": 12}'
```

- `broker.type=paper`（默认）：模拟盘立即成交并更新持仓，飞书推送成交回执
- `broker.type=qmt` + `trading.auto_execute=true`：直接真实下单
- 否则仅标记 `confirmed`，等人工在券商 App 成交后走第 3 步反馈

不想执行的计划无需操作，次日自动过期（`expired`）。

### 3. 人工成交后反馈（保持账本一致）

```bash
curl -X POST http://NAS_IP:8080/api/trade/confirm \
  -H "Authorization: Bearer $JZ_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"ts_code":"000001.SZ","side":"buy","qty":500,"price":11.15}'
```

### 4. 首次接入：同步真实持仓

```bash
curl -X POST http://NAS_IP:8080/api/portfolio/sync \
  -H "Authorization: Bearer $JZ_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"cash": 10392.74, "overwrite": true, "positions": [
        {"ts_code":"510050.SH","total_qty":500,"available_qty":500,"cost_price":3.059}
      ]}'
```

### Agent 提示词建议（可直接复制进 Agent 的定时任务）

```text
每个交易日 15:35 调用 GET /api/agent/brief：
1. 若 warnings 非空或 data_fresh=false，先向我报告异常，不要确认任何计划；
2. 逐条分析 open_plans：结合 reason、portfolio 风险指标、market 情绪，
   给出 执行/跳过 建议及理由，urgent（止损）计划优先；
3. 经我同意后用 POST /api/plan/confirm 确认；未同意的不操作；
4. 汇报今日持仓盈亏与健康分变化。
```

## 数据自动清理

调度器每日 16:30 自动执行（`retention` 配置段），防止数据库无限膨胀：

- 行情保留 3 年、新闻 30 天、交易计划/任务记录 90 天
- 回测记录只保留最近 20 个 run，**实盘记录（`live_*`）永久保留**
- 日志保留 30 天，HTML 报告保留最近 30 个
- 每日 `wal_checkpoint(TRUNCATE)`，每周日增量 vacuum 回收空间

## 部署（开机自启 + 崩溃自动拉起）

- **Linux (systemd)**：`scripts/jingzhe.service`，`systemctl enable --now jingzhe`
- **macOS (launchd)**：`scripts/com.jingzhe.trader.plist`，`launchctl load`

进程意外退出由系统自动拉起，重启后调度器依据 `job_run` 表自动补跑当天漏掉的任务。

## QMT 实盘（可选，Windows）

1. Windows 机器上运行 miniQMT + `scripts/qmt_sidecar.py`（设置 `QMT_SIDECAR_TOKEN`，仅监听 127.0.0.1）
2. 配置 `broker.type: qmt`、`broker.qmt.url`
3. `trading.auto_execute: true` 后，确认计划即真实下单；每日 15:35 自动对账

## 项目结构

```
cmd/            server(常驻) / backtest / optimizer / dataloader
internal/
  engine/       统一执行管道 Pipeline (回测=实盘)
  broker/       Broker接口: PaperBroker(模拟撮合) / QMTBridge
  risk/         风控: 止损止盈 / 仓位限制 / 小资金自适应 sizing
  scheduler/    内置调度器 (job_run 防重/补跑/panic隔离)
  strategy/     策略与动态选择器
  store/        SQLite 仓储 + retention 数据清理
  quote/        盘中实时行情 (腾讯免费源 / QMT)
  notify/       飞书通知
  api/          HTTP API (鉴权/CORS/recover 中间件)
  dataloader/   Tushare 数据同步 (库化, CLI与调度器共用)
```

## 免责声明

本项目仅供学习研究，不构成投资建议。股市有风险，实盘需谨慎。

## License

MIT
