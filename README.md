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
| `trading.min_trade_amount` | 3000 | 小资金降低门槛（0=自适应5000元太高，1万资金×40%仓位=4000<5000会被风控全部拦截）；设 3000 确保能成交 |
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
FEISHU_WEBHOOK=https://open.feishu.cn/open-apis/bot/v2/hook/xxx
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
curl http://127.0.0.1:11270/api/health      # 返回 JSON 即正常
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
| API 可访问 (`/api/health`) | 不可达则告警 |
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
# 1. 服务存活
sudo systemctl is-active jingzhe              # → active

# 2. API 正常
curl -s http://127.0.0.1:11270/api/health | head

# 3. 数据新鲜 (交易日当天 15:10 后应有当日数据)
sqlite3 /opt/jingzhe-trader/data/jingzhe.db "SELECT MAX(trade_date) FROM daily_bar;"

# 4. 交易日历完整 (今天应在日历中)
sqlite3 /opt/jingzhe-trader/data/jingzhe.db "SELECT * FROM trade_cal WHERE cal_date='$(date +%Y%m%d)';"

# 5. 指数数据存在 (大盘过滤策略需要)
sqlite3 /opt/jingzhe-trader/data/jingzhe.db "SELECT COUNT(*) FROM daily_bar WHERE ts_code='000300.SH';"

# 6. 健康检查 cron 已生效
grep jingzhe /var/log/syslog 2>/dev/null || tail -5 /opt/jingzhe-trader/logs/health_check.log
```

### 日常更新（NAS 拉取新代码 → 重新编译 → 重启）

当开发机推送了新代码（如策略调参、Bug 修复、新增指数同步），NAS 需执行以下更新流程：

```bash
cd /opt/jingzhe-trader

# 1. 拉取最新代码
git pull origin main

# 2. 重新编译 (停服前编译, 避免编译报错导致无可用二进制)
go build -o bin/server     ./cmd/server
go build -o bin/dataloader ./cmd/dataloader

# 3. 重启服务 (systemd 会优雅关机: SIGTERM 等 15s → WAL checkpoint → 拉起新进程)
sudo systemctl restart jingzhe

# 4. 验证
sleep 3
sudo systemctl status jingzhe
curl -s http://127.0.0.1:11270/api/health
```

> **更新 config.yaml 时**：直接编辑后 `sudo systemctl restart jingzhe` 即可，无需重新编译。

### 常见问题排查

| 现象 | 排查 |
|---|---|
| 服务启动后立即退出 | `journalctl -u jingzhe -n 50` 看日志；多为 config.yaml 路径或 `.env` 缺失 |
| 数据一直不更新 | 检查 Tushare token 是否过期；`./bin/dataloader -config config/config.yaml` 手动跑看报错 |
| 今天不在交易日历 | 健康检查脚本会自动补；也可手动 `./bin/dataloader -config config/config.yaml` |
| 周末调度器不工作 | 正常，调度器用系统时间判断周末，非交易日不执行数据/信号任务 |
| API 返回 401 | GET 接口无需 token；POST 接口需 `Authorization: Bearer $JZ_API_TOKEN` |
| 磁盘空间告警 | 调度器每日 16:30 自动清理；紧急可 `sqlite3 data/jingzhe.db "VACUUM;"` |

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
