# 惊蛰 (Jingzhe Trader)

> 蛰伏待击 — A 股量化交易系统，专为小资金设计

[![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

## 简介

**惊蛰 (Jingzhe)** 是一个基于 Go 语言的 A 股量化交易系统。名字取自二十四节气"惊蛰"——春雷惊醒蛰伏的昆虫，寓意**长时间观望等待，时机到了果断出手**。

专为**小资金**（1 万本金起）优化，强调**低频、集中、精准**的交易风格，用冷酷的规则代替冲动的人性。

---

## NAS AI 快速参考

> 你是惊蛰交易系统的 AI 助手。系统 24h 常驻 NAS，自动完成数据→选股→信号→日报→监控全链路。你只需通过 MCP 调用工具读结果、审批计划、反馈成交。

**部署**: `git clone` → 环境变量(`.env`) → `go build` → `systemd` 自启 → cron 健康检查

**密钥**: 全部走环境变量（`TUSHARE_TOKEN` / `LLM_API_KEY` / `JZ_API_TOKEN`），config.yaml 已 gitignore 不提交

**核心 MCP 工具**（`get_*` 为读工具，其余为写工具）：

| 工具 | 用途 |
|---|---|
| `get_agent_brief` | 全量上下文：计划+持仓+辩论+变更+任务状态 |
| `get_agent_dashboard` | 一次拿全：通知+计划+辩论+变更+任务 |
| `get_agent_alerts` | 未读通知（盘中止损告警等） |
| `confirm_plan` | 确认计划 `{"id": 123}` |
| `confirm_trade` | 反馈成交 `{"ts_code","side","qty","price"}` |
| `get_health` / `GET /health` | 存活检查（无需鉴权） |

**每日节奏**: 15:35 后拉 `get_agent_dashboard` → 审阅计划 → `confirm_plan` → 成交后 `confirm_trade`

**异常处理**: `warnings` 非空或 `data_fresh=false` → 先报告异常，不确认计划；LLM 不可用时辩论自动跳过，不影响信号生成

**资金口径**（字段都存在 `config_kv` 表，勿手工臆改）：

- `initial_capital` 本金：首次持仓同步时写入 = 现金 + Σ(数量×成本价)，**仅写一次永不更新**（累计盈亏/季初基准都以它为锚，本金变动走人工 SQL 修正）
- `cash` 可用资金：每笔成交确认后自动更新；总资产不落库，实时 = cash + Σ持仓市值，每日收盘后记入 `account_snapshot`
- 季度目标基准自动逐季滚动：取季前最后一张实盘快照，快照缺失才回退 `initial_capital`；快照在 15:10 行情未出时由 18:00 信号任务幂等补记
- `goal_risk_mode` 风险模式（normal/tightened/defensive/locked）：模式切换时自动改写风控参数（只收紧不放松），删除该 key 可强制重评

**给 NAS Agent 的注意事项**：

- 代码更新后必须重新编译并重启服务，旧 binary 不含邮件折行/快照补记修复（NAS 长行邮件会被 QQ 邮箱拒收："Line too long"）
- 存量库若 `initial_capital == cash` 且持仓非空，属于旧版只记现金的脏基准，需一次性 SQL 校正：
  `UPDATE config_kv SET value='<现金+持仓成本>' WHERE key='initial_capital';` 随后 `DELETE FROM config_kv WHERE key='goal_risk_mode';` 触发重评
- 持仓同步接口的 `initial_capital` 只在首次同步生效，后续同步改不了本金，不存在"同步把本金刷小"的风险

详细操作见下方 [Agent 接入指南](#agent-接入指南hermes--任意-ai-agent)。

---

## 架构

`cmd/server` 是唯一常驻进程，内置调度器在交易日自动完成全链路，结果全部落 SQLite。
AI Agent（如 Hermes / Claude / Cursor）通过 **MCP over Streamable HTTP** 读取结果、审批计划、反馈成交。

```
                      ┌──────────────────────────────────────────────────┐
                      │                cmd/server (24h常驻)               │
                      │                                                  │
  Tushare ──────────▶ │  调度器 (交易日自动执行, 每次任务后邮件通知)         │
  腾讯免费行情 ──────▶ │   09:00 盘前总结邮件 (持仓/今日计划/目标)           │
                      │   09:25 T+1持仓结转 (昨日买入转可卖)                │
                      │   15:10 数据更新 (进程内 dataloader) + 实盘账户快照  │
                      │         + 季度目标评估 (模式切换告警)            │
                      │   15:15 全市场选股 → 候选股票自动入池 + 历史K线同步  │
  QMT sidecar ◀─────▶ │   18:00 EOD信号 → 多智能体辩论 → trade_plan 表    │
                      │   15:35 对账 (QMT模式) + 成交回报轮询落库           │
                      │   18:00 当天总结 + 日报邮件 (含当日告警汇总)        │
                      │   盘中每5分钟 实时价止损监控 → 紧急计划+告警         │
                      │   16:30 数据保留清理 + WAL checkpoint              │
                      │                                                  │
                      │  多智能体辩论 (LLM可用时增强买入信号)               │
                      │   4分析师并行(技术/基本面/新闻/市场)               │
                      │   → 多空研究员辩论 → 风险管理经理裁决              │
                      │   → 决策变更检测(对比历史) → 通知用户              │
                      │                                                  │
                      │  统一执行管道 engine.Pipeline                      │
                      │   信号 → 智能体辩论 → 风控 → Broker下单 → 落库     │
                      │   (回测/模拟/实盘共用, 只换 Broker 实现)            │
                      │                                                  │
                      │  策略实例缓存 (避免重建丢失状态)                    │
                      │  共享Repo (避免重复实例化)                          │
                      │  新闻按股票池过滤 (优先展示相关新闻)                │
                      │  陈旧数据清理 (选股池外的股票自动删除)            │
                      └───────────────────┬──────────────────────────────┘
                                          │ SQLite (WAL)
                      ┌───────────────────┴──────────────────────────────┐
                      │         MCP over Streamable HTTP (鉴权)           │
                      └───────────────────┬──────────────────────────────┘
                                          │
     Agent/人: get_agent_brief → confirm_plan → confirm_trade
               get_agent_changes (决策变更检测)
```

## 季度目标跟踪（核心决策约束）

按**日历季度**跟踪收益目标与回撤预算，超预算时**自动收紧风险敞口**（只收紧不放松）：

| 配置项 | 默认 | 说明 |
|---|---|---|
| `goal.enabled` | true | 启用目标跟踪 |
| `goal.quarterly_target_pct` | 0.15 | 季度收益目标 15% |
| `goal.max_drawdown_budget` | 0.10 | 季度最大回撤预算 10% |
| `goal.auto_adjust` | true | 自动调节风险敞口 |

**自动调节规则**：回撤预算消耗 ≥70% → 总仓位 ×0.6 收紧；预算耗尽 → 仓位压至 20% + 止损 5%（防守）；目标提前达成 → 仓位 ×0.5 锁利。

每日数据更新后自动评估，**风险模式切换时告警**；状态可查 `get_goal_status`，`get_agent_brief` 返回的 `goal` 字段是 Agent 决策核心约束。数据源为每日实盘账户快照（account_snapshot，run_id=live)，季初基准取季度开始前最后一个快照，无快照时退回初始资金。

## 核心特点

- **小资金友好** — 1 万本金即可运行；按资金量级自适应持仓数与最小交易额（5 元最低佣金下默认单笔 ≥5000 元保证费率 ≤0.1%）
- **回测即实盘** — 回测/模拟/实盘共用同一条 `信号 → 风控 → 下单 → 落库` 管道，回测结果不虚高
- **风控内建** — 止损/止盈信号优先执行、单票/总仓位/板块敞口限制、含手续费的买入资金检查
- **全自动闭环** — 内置调度器：数据更新 → EOD 信号 → 对账 → 日报邮件推送 → 盘中止损监控 → 数据自动清理
- **自动选股** — 每日全市场扫描（4000+股票），多维度筛选 TopN 候选自动入池，无需手动维护股票池
- **常驻稳定** — 任务 panic 隔离、job_run 防重复/启动补跑、优雅关机、WAL checkpoint、共享仓储实例
- **季度目标跟踪** — 日历季度收益目标 + 回撤预算，超预算自动收紧风险敞口（只收紧不放松），模式切换告警
- **数据可信** — 全链路前复权、真实 T+1 交收、涨跌停/滑点/最低佣金建模、近期数据自动重拉吸收更正
- **多策略支持** — 均线交叉 / MACD / 布林带突破 / 多因子选股 / 日内做T，按市况动态切换，成交归因到策略
- **DeepSeek 多智能体辩论** — 仅支持 DeepSeek：4位分析师并行 → 多空辩论 → 风险经理裁决，二次验证买入信号 + 决策变更追踪（LLM 失败时明确告警不降级）
- **新闻智能过滤** — 按股票池关键词优先展示相关新闻，不足时热点补充

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

> **安全模型**: `config/config.yaml` 已在 `.gitignore` 中，**永远不会提交到 Git**。所有密钥（Tushare Token、DeepSeek Key、API Token、SMTP 授权码）一律通过环境变量注入，源码和配置文件均不含任何密钥。部署时创建 `.env` 文件（同样 gitignore），systemd/cron 自动读取。

**密钥一律走环境变量，不要写进配置文件**（配置文件中的同名项会被环境变量覆盖）：

```bash
export TUSHARE_TOKEN=你的tushare token       # 必需, 行情数据源
export JZ_API_TOKEN=随机长字符串              # 推荐, API鉴权token
export LLM_API_KEY=deepseek密钥              # 可选, 新闻分析+多智能体辩论
export QMT_SIDECAR_TOKEN=随机长字符串         # QMT实盘时, sidecar鉴权
```

> ⚠️ 如果你的 token 曾经写在配置文件里提交过 git，请立即到对应平台**轮换**。

### 3. 拉取数据 & 回测

```bash
bin/dataloader -config config/config.yaml          # 首次约拉3年数据
bin/dataloader -config config/config.yaml -adj     # 历史库升级后回填复权因子 (一次性)
bin/backtest -config config/config.yaml            # 回测并生成HTML报告 (前复权+真实T+1+滑点+涨跌停)
bin/optimizer -config config/config.yaml -walkforward -folds 3   # 样本外验证参数寻优
```

> **复权说明**：系统全链路使用前复权价格（回测与实盘同一口径），数据同步自动合并复权因子；
> 历史库首次升级后需跑一次 `-adj` 回填，否则策略信号在除权日会失真。
> 除权除息、T+1 交收、涨跌停、最低佣金均已建模，回测结果可信后再决策。

### 4. 启动常驻服务

```bash
bin/server -config config/config.yaml
# 监听端口取自 config.yaml 的 server.port (example 默认 11270)
curl http://127.0.0.1:11270/health              # 健康检查 (无需鉴权)
# MCP 端点: http://127.0.0.1:11270/mcp
# 在支持 MCP 的客户端（Claude/Cursor/任意 MCP inspector）中配置以上 URL
```

## 小资金配置指南（1 万元档）

`config.example.yaml` 默认即为 1 万资金调好的参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `risk.max_position_pct` | 0.4 | 单票上限 40%（小资金必须集中） |
| `risk.max_total_position_pct` | 0.9 | 总仓位上限 90%（保留10%现金做T） |
| `risk.stop_loss_pct` | 0.08 | 止损 -8% |
| `risk.take_profit_pct` | 0.15 | 止盈 +15% |
| `risk.trailing_stop_pct` | 0.05 | 移动止盈：盈利达止盈线后，从持仓期间最高价回撤 5% 退出（让利润奔跑；0=不启用） |
| `goal.*` | 见目标章节 | 季度目标 + 回撤预算 + 自动收紧风险敞口 |
| `trading.min_trade_amount` | 3000 | 小资金降低门槛（0=自适应5000元太高，1万资金×40%仓位=4000<5000会被风控全部拦截）；设 3000 确保能成交 |
| `trading.max_positions` | 0 | 0=自适应：<5万→2只，<20万→4只，否则6只 |
| `trading.auto_execute` | false | 默认只生成计划，人/Agent 确认后执行 |
| `cost.min_commission` | 5.0 | 按你的券商实际佣金档修改 |

资金量变大后无需改代码：把 `backtest.initial_capital` 改成实际资金，自适应参数自动放宽；也可以手动指定 `trading.*` 覆盖自适应。

## MCP 接口一览

系统对外暴露 **MCP over Streamable HTTP**，端点为 `http://<host>:<port>/mcp`。
配置 `server.api_token` 后，所有 MCP 请求需携带 `Authorization: Bearer <token>` 请求头；仅 `/health` 健康检查端点无需鉴权。

### 连接信息

| 项 | 默认值/说明 |
|---|---|
| 传输 | Streamable HTTP (`POST /mcp`, `GET /mcp`) |
| 地址 | `http://127.0.0.1:11270/mcp`（端口由 `server.port` 控制） |
| 鉴权 | `Authorization: Bearer $JZ_API_TOKEN` |
| 健康检查 | `GET /health`（无需鉴权，供 systemd/cron 使用） |

### 读工具（get_*）

| 工具 | 说明 |
|---|---|
| `get_agent_brief` | **Agent 首选**：计划+持仓+市场+健康度+辩论结果+决策变更+任务状态 一次拿全 |
| `get_agent_dashboard` | **Agent 仪表盘**：未读通知+今日通知+计划+辩论+变更+任务状态 汇总视图 |
| `get_agent_changes` | 决策变更检测：辩论结果对比 + 计划状态变更 + 任务完成状态 |
| `get_agent_alerts` | 通知存储：告警同时落库，Agent 可离线读取/标记已读 |
| `get_plans` | 交易计划列表（不传 date 返回全部待处理） |
| `get_daily_report` | 每日操盘报告（汇总） |
| `get_positions` | 持仓诊断 |
| `get_portfolio` | 原始持仓明细 |
| `get_market` / `get_daily_report` / `get_strategy_status` | 市场概况 / 每日报告 / 策略建议 |
| `get_goal_status` | 季度目标状态（收益/进度/回撤/预算消耗/风险模式） |
| `get_health` | 服务健康状态 |
| `get_system_status` | 数据新鲜度 / 持仓数 / 下一交易日 |
| `get_kline` | K线数据 |
| `get_snapshots` | 账户快照历史 |
| `get_screener_results` | 自动选股结果（最新或按日期查询） |

### 写工具

| 工具 | 说明 |
|---|---|
| `confirm_plan` | 确认交易计划 `{"id": 123}` |
| `confirm_trade` | 人工成交后反馈 `{"ts_code","side","qty","price"}` |
| `sync_portfolio` | 同步真实持仓（`overwrite: false` 为增量 Upsert） |
| `update_data` | 手动触发数据更新 |
| `switch_strategy` | 手动切换策略 `{"strategy": "ma_cross"}` |
| `mark_alerts_read` | 标记通知已读 `{"id": 123}` 或 `{"all": true}` |
| `run_screener` | 手动触发全市场选股（测试用，正常由调度器自动执行） |

### 资源

部分常用数据同时以 MCP Resource 形式暴露：

| 资源 URI | 说明 |
|---|---|
| `jingzhe://health` | 服务健康状态 JSON |
| `jingzhe://agent/brief` | Agent 全量上下文 JSON |

## Agent 接入指南（Hermes / 任意 AI Agent）

本系统是 Agent 的"数据 + 执行后端"：调度器每个交易日自动完成数据更新→信号生成→盘中监控，结果全部落库；Agent 只需定时读取现成结果、审批计划、反馈成交，不需要自己算任何指标。

### 前置配置：启用多智能体辩论

在 `config.yaml` 中启用 LLM，辩论系统自动生效（不启用则仅用策略信号，跳过辩论）。**仅支持 DeepSeek API**（OpenAI 兼容接口）：

```yaml
# config.yaml
llm:
  enabled: true              # 改为 true 启用
  api_key: ""                # 留空，用环境变量 LLM_API_KEY 注入
  base_url: "https://api.deepseek.com/v1"  # DeepSeek API 地址
  model: "deepseek-chat"     # deepseek-chat 或 deepseek-reasoner
  json_mode: true            # DeepSeek 支持 response_format: json_object
```

```bash
# 环境变量注入密钥（写入 NAS 的 .env 文件）
export LLM_API_KEY=sk-your-deepseek-key
```

> LLM 启用后，每个买入信号触发 4 位分析师 → 多空辩论 → 风险经理裁决，耗时约 10-30 秒/标的。**LLM 调用失败时明确告警（ Warn 日志），不会静默降级为虚假分析**；未启用时系统正常运行，仅跳过辩论增强。

### 通知存储机制

调度器每次执行任务后的通知**同时落库 SQLite**（`agent_alert` 表），即使邮件未配置或发送失败，Agent 也能通过 MCP 读取：

```
调度器任务完成 → alert() 方法
  ├─ 1. 落库 agent_alert 表 (始终执行, 不受邮件配置影响)
  └─ 2. 邮件推送 (可选, 失败不影响流程)
         ↓
Agent 调用 get_agent_alerts {"unread_only": true}
  → 读取未读通知 → 通知用户 → mark_alerts_read {"all": true} 标记已读
```

通知级别：`info`（常规） / `warning`（警告） / `urgent`（紧急/止损/崩溃） / `success`（成功）

### 鉴权

```bash
export JZ_API_TOKEN="your-random-token"    # 服务端: config 留空则从环境变量 JZ_API_TOKEN 读取
# MCP 客户端需要在请求头中携带: Authorization: Bearer $JZ_API_TOKEN
# 例外: GET /health 无需鉴权
```

### 系统每日时间线（自动执行，Agent 无需介入）

| 时间 | 任务 |
|---|---|
| 00:00-09:00 | 常驻运行（systemd 保活），调度器每 30 秒检查到点任务 |
| 09:00 | 盘前总结邮件（昨日市场概况 + 当前持仓 + 今日计划 + 季度目标） |
| 09:25 | T+1 持仓结转 + 盘中止损监控就绪 |
| 09:30-15:00 | 盘中监控（每 5 分钟，触发止损 → 紧急计划 + 告警） |
| 15:10 | 数据更新（Tushare 行情入库）+ 实盘账户快照 + 季度目标评估 |
| 15:15 | 全市场自动选股 → 候选入池 + 历史K线同步 |
| 18:00 | EOD 信号生成 → 多智能体辩论 → 交易计划落库 |
| 15:35 | 对账（仅 QMT 实盘） |
| 18:00 | 当天总结 + 日报邮件（含当日告警汇总）+ 操作提醒落库 |
| 16:30 | 数据保留清理 + WAL checkpoint |

所有任务完成后通知**同时落库 + 邮件推送**（无论有无交易计划）；Agent 下次执行时检查 `task_completed` + `plan_status_summary` 确认状态已更新。

### 推荐轮询节奏（交易日）

| 时间 | Agent 动作 | 说明 |
|---|---|---|
| 09:25-15:00 | `get_agent_alerts {"unread_only": true}` | 盘中每 5 分钟轮询未读通知（止损告警等紧急通知） |
| 15:35 后 | `get_agent_dashboard` | 一次性获取：未读通知+计划+辩论+变更+任务状态 |
| 15:35 后 | `get_agent_brief` | 详细上下文（持仓诊断+市场概况+辩论结果） |
| 15:35 后 | `get_agent_changes` | 检查决策变更、计划状态变更 |
| 审阅后 | `confirm_plan {"id": 123}` | 逐条确认要执行的计划 |
| 成交后 | `confirm_trade {...}` | 人工/券商成交后反馈，保持持仓同步 |
| 读取后 | `mark_alerts_read {"all": true}` | 标记通知已读 |
| 任意 | `get_health` / `GET /health` | 存活与任务健康度巡检 |

### 1. 读取全量上下文

调用 MCP 工具：

```
get_agent_brief
```

响应字段（`data`）：

```jsonc
{
  "date": "20260726",             // 数据基准日
  "data_fresh": true,              // false → 数据滞后, 不宜盲信计划
  "open_plans": [{                 // 待处理交易计划 (pending/confirmed)
    "id": 12, "trade_date": "20260726",
    "ts_code": "000001.SZ", "name": "平安银行",
    "direction": "buy", "qty": 500, "ref_price": 11.13,
    "reason": "均线金叉: MA3=11.05上穿MA25=10.98 | LLM辩论: 技术面转强",
    "strategy": "ma_cross", "urgency": "normal", "status": "pending"
  }],
  "portfolio": { /* 持仓明细+健康分+集中度+盈亏 */ },
  "market":    { /* 指数涨跌/市场情绪概况 */ },
  "jobs":      { "signal": "2026-07-26 15:30:02" },
  "warnings":  ["..."],           // 异常, 非空时优先告知用户
  "debates": [{                    // 多智能体辩论结果
    "ts_code": "000001.SZ", "decision": "buy", "confidence": 0.72,
    "position_pct": 0.5, "stop_price": 10.58,
    "risk_level": "medium", "summary": "技术面金叉确认..."
  }],
  "decision_changes": [{           // 与上次辩论对比
    "ts_code": "600036.SZ", "prev_decision": "buy", "curr_decision": "hold",
    "prev_confidence": 0.65, "curr_confidence": 0.40,
    "detail": "决策变更: 买入 → 持有; 置信度下降: 65% → 40%"
  }],
  "plan_status_summary": { "pending": 2, "confirmed": 1, "executed": 0, "total": 3 },
  "task_completed": { "data_update": true, "signal": true, "report": false },
  "goal": {                        // 季度目标, Agent 决策核心约束
    "quarter": "2026Q3", "return_pct": -0.05, "target_pct": 0.15,
    "drawdown_pct": 0.06, "budget_consumed": 0.6, "mode": "normal"
  },
  "action_needed": [               // 需要用户操作的提示
    "📋 有2条待确认的交易计划，请审阅后确认或忽略",
    "🔄 检测到1个标的投资决策发生变化，请关注"
  ]
}
```

### 2. 确认执行计划

```
confirm_plan {"id": 12}
```

- `broker.type=paper`（默认）：模拟盘立即成交并更新持仓，邮件推送成交回执
- `broker.type=qmt` + `trading.auto_execute=true`：直接真实下单
- 否则仅标记 `confirmed`，等人工在券商 App 成交后走第 3 步反馈

不想执行的计划无需操作，次日自动过期（`expired`）。

### 3. 人工成交后反馈（保持账本一致）

```
confirm_trade {"ts_code":"000001.SZ","side":"buy","qty":500,"price":11.15}
```

### 4. 首次接入：同步真实持仓

```
sync_portfolio {"cash": 10392.74, "overwrite": true, "positions": [
  {"ts_code":"510050.SH","total_qty":500,"available_qty":500,"cost_price":3.059}
]}
```

### 5. 手动切换策略

```
switch_strategy {"strategy": "macd"}
get_strategy_status
```

### Agent 提示词建议（可直接复制进 Agent 的定时任务）

```text
每个交易日 15:35 调用 get_agent_brief：
1. warnings 非空或 data_fresh=false → 先报告异常，不确认任何计划；
2. task_completed.signal 非 true → 信号未生成，不确认计划；
3. 逐条分析 open_plans（结合 debates/portfolio/market），urgent 计划优先，给出执行/跳过建议；
4. decision_changes 非空 → 告知用户决策变化及原因；
5. plan_status_summary 有 confirmed → 提醒反馈成交；
6. 经我同意后 confirm_plan {"id": 123} 确认；
7. 汇报今日持仓盈亏与健康分变化。

通知读取（每次执行时检查）：
get_agent_alerts {"unread_only": true} → 按 level 排序（urgent > warning > info > success）通知用户 → 通知后 mark_alerts_read {"all": true} 标记已读

仪表盘（一次拿全）：
get_agent_dashboard → 未读通知 + 今日通知 + 待处理计划 + 辩论结果 + 决策变更 + 任务状态
```

### 6. 读取通知存档（Agent 核心）

调度器所有通知（信号/日报/止损/告警）都会落库，Agent 读取后通知用户：

```
# 获取未读通知 (响应含 alerts[]: id/job_name/level/title/content/status)
get_agent_alerts {"unread_only": true}

# 通知用户后标记已读
mark_alerts_read {"all": true}
```

### 7. Agent 仪表盘（一次拿全）

```
get_agent_dashboard
```

返回未读通知 + 今日通知 + 待处理计划 + 辩论结果 + 决策变更 + 任务状态 + 计划汇总，适合首次拉取。

## 多智能体辩论系统

当 LLM 配置启用时（`llm.enabled: true`），系统对每个买入信号自动启动多智能体辩论，对策略信号做二次验证。**仅支持 DeepSeek API**；LLM 调用失败时明确告警，不会静默降级为虚假分析结果；未启用时跳过辩论，直接用策略信号生成计划。

### 辩论流程

```
策略信号(买入) → 4分析师并行分析 → 多空研究员辩论 → 风险管理经理裁决
                     ↓                    ↓                ↓
              技术分析师           看涨研究员          决策: buy/hold/reject
              基本面分析师         看跌研究员          仓位建议 + 止损价
              新闻分析师                              风险等级 + 摘要
              市场分析师
```

### 智能体角色

| 角色 | 职责 | 输入 |
|---|---|---|
| 技术分析师 | MA/RSI/MACD/布林带/成交量分析 | 90日K线 |
| 基本面分析师 | PE/PB/ROE/营收增长/负债率 | 基本面+财务数据 |
| 新闻分析师 | 个股相关新闻情感分析 | 新闻库（按股票池过滤） |
| 市场分析师 | 大盘趋势/市场情绪/板块轮动 | 指数K线 |
| 看涨研究员 | 从分析师报告中提炼买入理由 | 4份分析师报告 |
| 看跌研究员 | 从分析师报告中提炼风险因素 | 4份分析师报告 |
| 风险管理经理 | 权衡多空论点，做出最终裁决 | 分析师报告+多空论点+持仓 |

### 裁决结果

- **buy** 通过辩论验证建议买入（可能调整仓位） | **hold** 持有/观望 | **reject** 否决买入信号 | **sell** 建议卖出（已持仓时）

### 决策变更检测

每次辩论结果落库后，系统自动与同一股票的上次辩论结果对比，检测以下变化：

- 决策方向变更（如 buy → hold）
- 置信度显著变化（>20%）
- 风险等级变化

变更结果通过 `get_agent_changes` 查询，同时通过通知落库提示。

## 自动选股与新闻过滤

系统**完全自动选股**，无需手动维护股票池：启用 `screener.enabled: true` 后，每日 15:15 自动从全市场（4000+股票）筛选 TopN 候选，同步 6 个月历史K线并自动加入策略股票池（18:00 信号生成时与配置池、持仓一并扫描）。当前持仓自动加入扫描范围；`universe.bluechip` / `universe.tech` 可手动补充关注股票（可选，留空则完全依赖自动选股）；`dataloader.filter_mode: true` 时只拉选股结果+持仓+watchlist 的数据，大幅减少数据量。

### 选股流程

```
15:10 数据更新 (配置池+持仓的当日行情)
15:15 全市场选股:
  1. Tushare 拉取全市场 daily_basic (PE/PB/换手率/市值)
  2. Tushare 拉取全市场 daily (涨跌幅)
  3. 多维度筛选 (价格/换手率/PE/PB/市值/ST/新股)
  4. 评分排序 (活跃度+资金关注度+估值吸引力)
  5. Top N 候选 → 同步6个月历史K线 → 结果落库
  6. 通知落库 + 邮件推送
18:00 信号生成 (策略扫描: 配置池 + 持仓 + 选股候选)
```

### 筛选条件

| 条件 | 默认值 | 说明 |
|---|---|---|
| `exclude_st` | true | 排除ST股 |
| `min_list_days` | 60 | 排除上市不足60天的新股 |
| `min_price` / `max_price` | 2 / 50 | 价格区间（小资金避免高价股） |
| `min_turnover_rate` | 1.0% | 最低换手率（排除僵尸股） |
| `max_pe` | 50 | 最大PE_TTM（排除负PE和过高估值） |
| `max_pb` | 5.0 | 最大PB |
| `min_circ_mv` | 50亿 | 最小流通市值（排除小盘垃圾股） |

### 评分算法

评分 = 换手率(30%) + 量比(30%) + 估值合理性(25%) + 涨跌幅(15%)

- 换手率 1-10% 最佳（过高可能是炒作）
- 量比 >1 表示放量（资金关注）
- PE_TTM 10-30 最佳（估值合理）
- 温和上涨 0-5% 最佳（避免接飞刀）

### 新闻过滤

新闻按股票池优先过滤：关键词匹配（代码/6位代码/名称）→ 相关新闻排前 → 不足 20 条时热点补充；新闻分析师只分析与当前标的相关的新闻。

### 查看选股结果

```
# 获取最新选股结果
get_screener_results

# 按日期查询 / 手动触发选股 (测试用, 正常由调度器自动执行)
get_screener_results {"date": "20260805"}
run_screener
```

## 数据口径（AI 决策前必读）

所有行情与回测数据遵循同一口径，理解这些约定才能正确解读信号与回测：

| 口径 | 约定 |
|---|---|
| 价格 | **前复权**（以最新复权因子为基准）。回测与实盘策略信号同一口径；涨跌停价比较时自动换算回原始价 |
| 成交 | 默认**次日开盘价**成交（`backtest.fill_price: next_open`），含双向滑点（默认 0.02%） |
| 交收 | **真实 T+1**：T 日信号 T+1 开盘成交，T+2 起可卖；回测与实盘台账一致（每日 09:25 结转） |
| 限制 | 涨停禁买、跌停禁卖（无涨跌停数据时按板块规则自算兜底）；停牌自动顺延到复牌日成交 |
| 费用 | 佣金（含最低 5 元）+ 卖出印花税 + 双向过户费；买入资金检查含费 |
| 复权因子 | tushare `adj_factor` 每日自动合并；历史库升级后需跑一次 `bin/dataloader -adj` 回填 |
| 数据更正 | 每个交易日自动重拉最近 ~10 个交易日覆盖刷新，吸收 tushare 修订 |
| 财务因子 | 按公告日（ann_date）point-in-time 过滤，无前视 |

**绩效归因**：`trades.strategy` 记录每笔成交的来源策略（回测 run 与实盘 live 同表区分 run_id）；
动态策略选择器据此给每个策略评分，结合沪深300 近 30 日趋势推荐，连续 3 日同向才切换。

## 数据自动清理

调度器每日 16:30 自动执行（`retention` 配置段），防止数据库无限膨胀：

- 行情保留 3 年、新闻 30 天、交易计划/任务记录 90 天
- 回测记录只保留最近 20 个 run，**实盘记录（`live_*`）永久保留**
- 日志保留 30 天，HTML 报告保留最近 30 个
- 每日 `wal_checkpoint(TRUNCATE)`，每周日增量 vacuum 回收空间
- **陈旧股票清理**：不在活跃股票池（选股结果+持仓+watchlist）中的股票，其 `daily_bar`/`daily_basic`/`stk_limit` 数据自动删除，防止选股轮换导致数据库膨胀

## NAS 部署（开机自启 + 崩溃自愈 + 数据保鲜）

NAS 是长期运行的"数据 + 执行后端"。以下步骤以 Linux NAS（群晖/威联通通用）为例，完成**首次部署**和**日常更新**。

### 首次部署

#### 0. 前置依赖

```bash
# Go 1.21+ (编译用) 和 sqlite3 (健康检查脚本用)
# 群晖: 套件中心装 Go; 威联通: entware-ng 装 golang sqlite3
go version          # 确认 >= 1.21
sqlite3 --version   # 确认已装
```

#### 1. 拉取代码并编译

```bash
cd /opt
git clone https://github.com/qyiun666/jingzhe-trader.git
cd jingzhe-trader

# 编译全部二进制到 bin/
go build -o bin/server     ./cmd/server
go build -o bin/dataloader ./cmd/dataloader
go build -o bin/backtest   ./cmd/backtest
go build -o bin/optimizer  ./cmd/optimizer
```

> NAS 多为 ARM/x86 低功耗 CPU，建议**在性能更好的机器上交叉编译**后把 `bin/` 拷过去：
> ```bash
> # 在 Mac/Linux 开发机上编译 ARM64 NAS 版本
> GOOS=linux GOARCH=arm64 go build -o bin/server ./cmd/server
> scp bin/server  nas:/opt/jingzhe-trader/bin/
> ```

#### 2. 配置文件 + 环境变量

```bash
cp config/config.example.yaml config/config.yaml
# 按需修改 config.yaml (股票池、资金量、策略参数)
```

**密钥一律走环境变量**，写入 `.env` 文件（systemd 会读取）：

```bash
cat > /opt/jingzhe-trader/.env <<'EOF'
TUSHARE_TOKEN=你的tushare_token
JZ_API_TOKEN=随机长字符串_用于API鉴权
LLM_API_KEY=你的deepseek密钥
EOF
chmod 600 /opt/jingzhe-trader/.env      # 仅 owner 可读
```

#### 3. 首次拉取历史数据

```bash
cd /opt/jingzhe-trader
./bin/dataloader -config config/config.yaml   # 首次约 10-20 分钟, 拉取 3 年数据
```

确认数据已入库：

```bash
sqlite3 data/jingzhe.db "SELECT COUNT(*) FROM daily_bar;"          # 应有数万条
sqlite3 data/jingzhe.db "SELECT COUNT(*) FROM trade_cal;"          # 交易日历
sqlite3 data/jingzhe.db "SELECT MAX(trade_date) FROM daily_bar;"   # 最新日期
```

#### 4. 安装 systemd 服务（开机自启 + 崩溃拉起）

```bash
# 修改 scripts/jingzhe.service 中的路径(若非 /opt/jingzhe-trader 需调整)
sudo cp scripts/jingzhe.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now jingzhe        # 开机自启 + 立即启动

# 验证
sudo systemctl status jingzhe
curl http://127.0.0.1:11270/health      # 返回 JSON 即正常
```

进程意外退出由 systemd 自动拉起（`Restart=always`），重启后调度器依据 `job_run` 表自动补跑当天漏掉的任务。

#### 5. 安装健康检查脚本 + cron（数据保鲜 + 二次自愈）

systemd 保证进程常驻，但无法发现**进程活着但数据过期/日历缺失**等问题。健康检查脚本每 10 分钟巡检并自愈：

```bash
# 赋予执行权限
chmod +x /opt/jingzhe-trader/scripts/nas_health_check.sh

# 加入 crontab (每 10 分钟执行)
sudo crontab -e
# 添加以下行:
*/10 * * * * /opt/jingzhe-trader/scripts/nas_health_check.sh >> /opt/jingzhe-trader/logs/health_check.log 2>&1
```

健康检查脚本（`scripts/nas_health_check.sh`）做三件事：

| 检查项 | 异常处理 |
|---|---|
| server 进程存活 | 挂了则自动 `nohup` 重启 |
| API 可访问 (`/health`) | 不可达则告警 |
| 数据新鲜度 + 交易日历 | 数据过期或日历缺失则自动触发 `dataloader` 补数据 |

查看巡检日志：

```bash
tail -50 /opt/jingzhe-trader/logs/health_check.log
```

#### 6. 防火墙放行（远程访问）

```bash
# 仅放行局域网访问 API (11270 为默认端口, 见 config.yaml server.port)
sudo iptables -A INPUT -p tcp --dport 11270 -s 192.168.1.0/24 -j ACCEPT
```

### 首次部署验证清单

```bash
sudo systemctl is-active jingzhe              # → active (服务存活)
curl -s http://127.0.0.1:11270/health | head   # MCP 服务正常
sqlite3 /opt/jingzhe-trader/data/jingzhe.db "SELECT MAX(trade_date) FROM daily_bar;"   # 数据新鲜
sqlite3 /opt/jingzhe-trader/data/jingzhe.db "SELECT COUNT(*) FROM daily_bar WHERE ts_code='000300.SH';"  # 指数数据存在
# 通过 MCP 客户端调用 get_agent_brief 验证鉴权
```

### 日常更新（NAS 拉取新代码 → 重新编译 → 重启）

当开发机推送了新代码（如策略调参、Bug 修复、新增指数同步），NAS 需执行以下更新流程：

```bash
cd /opt/jingzhe-trader

# 一键更新 (拉取 + 编译 + 重启)
git pull origin main && \
go build -o bin/server ./cmd/server && \
sudo systemctl restart jingzhe

# 验证
sleep 3
sudo systemctl status jingzhe
curl -s http://127.0.0.1:11270/health
```

> **更新 config.yaml 时**：直接编辑后 `sudo systemctl restart jingzhe` 即可，无需重新编译。

### 常见问题排查

| 现象 | 排查 |
|---|---|
| 服务启动后立即退出 | `journalctl -u jingzhe -n 50` 看日志；多为 config.yaml 路径或 `.env` 缺失 |
| 数据一直不更新 | 检查 Tushare token 是否过期；`./bin/dataloader -config config/config.yaml` 手动跑看报错 |
| 今天不在交易日历 | 健康检查脚本会自动补；也可手动 `./bin/dataloader -config config/config.yaml` |
| 周末调度器不工作 | 正常，调度器用系统时间判断周末，非交易日不执行数据/信号任务 |
| MCP 返回 401 | MCP 请求需 `Authorization: Bearer $JZ_API_TOKEN`（仅 `/health` 豁免） |
| 策略信号丢失 | 策略实例已缓存，重启服务后会重新初始化；检查 `config.yaml` 股票池是否有当日行情 |
| 磁盘空间告警 | 调度器每日 16:30 自动清理；紧急可 `sqlite3 data/jingzhe.db "VACUUM;"` |
| LLM 辩论无结果 | 检查 `llm.enabled: true` 和 `LLM_API_KEY` 环境变量；仅支持 DeepSeek API；LLM 失败时会输出 Warn 日志而非静默降级 |

### macOS (launchd)

非 NAS 场景可用 launchd：

```bash
cp scripts/com.jingzhe.trader.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.jingzhe.trader.plist
```

## QMT 实盘（可选，Windows）

1. Windows 机器上运行 miniQMT + `scripts/qmt_sidecar.py`（设置 `QMT_SIDECAR_TOKEN`，仅监听 127.0.0.1）
2. 配置 `broker.type: qmt`、`broker.qmt.url`
3. `trading.auto_execute: true` 后，确认计划即真实下单；每日 15:35 自动对账

## 项目结构

```
cmd/            server(常驻) / backtest / optimizer / dataloader
internal/
  agent/        多智能体辩论系统 (分析师/研究员/风险管理/辩论编排/变更检测)
  engine/       统一执行管道 Pipeline (回测=实盘)
  broker/       Broker接口: PaperBroker(模拟撮合) / QMTBridge
  risk/         风控: 止损止盈 / 仓位限制 / 小资金自适应 sizing
  scheduler/    内置调度器 (job_run 防重/补跑/panic隔离/WaitGroup优雅关闭/增强通知)
  strategy/     策略与动态选择器 (实例缓存/SwitchTo手动切换)
  store/        SQLite 仓储 (共享Repo/retention清理/辩论结果/通知存储)
  quote/        盘中实时行情 (腾讯免费源 / QMT)
  notify/       通知（邮件）
  api/          业务服务层 (被 MCP 层复用)
  mcp/          MCP over Streamable HTTP 对外接口
  dataloader/   Tushare 数据同步 (库化, CLI与调度器共用)
  llm/          LLM 客户端 (仅 DeepSeek/新闻分析/带缓存)
  config/       配置管理 (环境变量覆盖敏感项)
```

## 免责声明

本项目仅供学习研究，不构成投资建议。股市有风险，实盘需谨慎。

## License

MIT
