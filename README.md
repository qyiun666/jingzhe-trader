# 惊蛰（Jingzhe Trader）

> A 股量化交易系统：每日自动选股、出买卖信号、生成指令单并邮件推送；季度目标驱动的动态风控档位；NAS 7×24 常驻，通过 **MCP 接口**供外部 AI Agent 读写。

## 它能做什么

- **每日盯盘流水线**：数据同步 → 新鲜度门禁 → 选股漏斗 → 信号 → 风控 → 指令单 → 邮件通知，全自动。
- **选股漏斗可诊断**：每级淘汰都计数（`screen_funnel`），0 候选自动降级到观察名单（`screen_watchlist`）——根治"永远 0 候选"的黑盒问题。
- **季度目标三档位状态机**：G1 标准 / G2 收紧 / G3 防守，落后时完全放开敞口 + 物理熔断兜底最坏情况。
- **LLM 只做买入候选终审**（DeepSeek），不参与决策投票。
- **MCP 对外接口**（13 工具）：外部 Agent 负责「初始化当日流程 → 读买卖指令 → 人工下单后回执成交 → 每天查日志」。
- **NAS 7×24 常驻**（systemd / launchd）+ 独立看门狗邮件告警。

## 关键设计决策

| 决策点 | 结论 |
|--------|------|
| 季度目标落后时 | 完全放开敞口（允许自主放大仓位追赶），但由物理熔断框定底线 |
| 资金 / 季度目标 | 1 万本金、季度 10%~20%（均可配置） |
| LLM 角色 | 只做买入候选终审（砍掉多角色辩论） |
| 数据 / 配置 | 全放 SQLite（`modernc.org/sqlite`，纯 Go 无 CGO） |
| 买卖执行 | 系统只出指令单，由人工在券商 App 执行后回执（不自动下单） |

## 架构（流水线）

```
日历 → 日线 → 财务 → 新鲜度门禁 → 选股漏斗 → 信号 → 目标档位/风控 → 指令单 → 邮件
                                                                    ↘ 回执成交(人工)
                                                                    ↘ MCP 接口(外部 Agent)
```

## 目录结构

```
cmd/
  jingzhectl/   运维 CLI（config dump/set、run task）
  jingzhed/     MCP 常驻服务（对外接口）
  jingzhew/     看门狗（健康探测 + 邮件告警）
internal/
  store/        SQLite 存储与迁移（schema/仓储）
  config/       config_kv 配置（默认值/环境变量/凭据掩码）
  dataloader/   数据同步与新鲜度门禁
  market/       交易日历/成本/季度划分
  quote/        行情（gotdx 为主、腾讯降级）
  screener/     选股漏斗
  signal/       买卖信号
  goal/         季度目标档位状态机
  risk/         风控与仓位
  ticket/       指令单/回执/快照
  notify/       邮件构建/折行/告警
  scheduler/    调度器（14 个 JobSpec 时间线）
  llm/          DeepSeek 终审
  mcp/          MCP 接口（13 工具）
  observability/ 日志（zap）
  model/        领域模型与枚举
  app/          依赖装配
deploy/         systemd / launchd 单元、Makefile
docs/           设计文档（本地保留，不推 GitHub）
MCP-AGENT-GUIDE.md  外部 Agent 接入手册（根目录）
```

## 快速开始

前置：Go 1.27+（`modernc.org/sqlite` 纯 Go，无需 CGO）。

```bash
# 1) 配置凭据（复制模板并填入真实值）
cp deploy/env.example .env && chmod 600 .env
#   .env 需注入进程环境（本系统不自动加载 .env，由 systemd/launchd 注入）

# 2) 构建
make build          # 或 go build ./...

# 3) 首次初始化：写 config_kv 与建表（由 store 自动迁移）
./bin/jingzhectl -db data/jingzhe.db config dump --show-secrets

# 4) 手动跑一遍当日流水线（按依赖顺序）
./bin/jingzhectl -db data/jingzhe.db run task calendar
./bin/jingzhectl -db data/jingzhe.db run task daily --date 20260901
./bin/jingzhectl -db data/jingzhe.db run task fina
./bin/jingzhectl -db data/jingzhe.db run task freshness
./bin/jingzhectl -db data/jingzhe.db run task screener
./bin/jingzhectl -db data/jingzhe.db run task signal

# 5) 启动 MCP 常驻服务（供外部 Agent 接入）
./bin/jingzhed -db data/jingzhe.db -addr :8080
```

## 三个二进制

| 二进制 | 作用 |
|--------|------|
| `jingzhectl` | 运维 CLI：`config dump/get/set`、`run task <calendar\|daily\|fina\|freshness\|screener\|signal>` |
| `jingzhed` | MCP 常驻服务：`-db <sqlite>` `-addr :8080`；`/healthz` 免鉴权、`/mcp` 需 Bearer 令牌；令牌为空拒绝启动 |
| `jingzhew` | 看门狗：轮询 `/healthz` + `/mcp`，连续失败达阈值发 SMTP 告警 |

## MCP 对外接口（给外部 Agent）

系统通过 `jingzhed` 暴露 **13 个 MCP 工具**（7 读 + 6 写），外部 AI Agent 据此完成：

1. **初始化**：`init_day` / `trigger_task`
2. **读指令**：`get_brief` → `get_tickets` / `get_candidates` / `get_signals`
3. **回执成交**：`report_fill`（人工在券商 App 下单后回报）
4. **查日志**：`get_logs` / `ack_alert`

完整接入手册（启动方式、协议、工具参数、每日工作流、错误处理、安全红线）见根目录 **[`MCP-AGENT-GUIDE.md`](./MCP-AGENT-GUIDE.md)**；接口速查见 `docs/API.md`。

## 配置

- 全部配置存 SQLite 的 `config_kv` 表（单一数据源），键目录见 `internal/config/keys.go`。
- 生效优先级：环境变量 `JZ_*` > 库内值 > 默认值。
- 环境变量模板：`deploy/env.example`（含 Tushare / LLM / 邮件 / MCP 令牌 / 看门狗）。
- 注意：根目录 `.env`（真实凭据）被 `.gitignore` 忽略；模板之所以放在 `deploy/` 而非根目录 `.env.example`，是因为 `.gitignore` 的 `.env.*` 规则会连带忽略后者。

## 部署（NAS 7×24）

- systemd：`deploy/systemd/jingzhed.service`、`deploy/systemd/jingzhew.service`
- launchd（macOS）：`deploy/launchd/com.jingzhe.trader.daemon.plist`
- 看门狗独立进程，`jingzhed` 挂掉会发邮件告警。

## 测试

```bash
go test ./...    # 全量
go vet ./...
```

## 文档

- 根目录 `MCP-AGENT-GUIDE.md`：外部 Agent 接入手册（会随代码推送）。
- `docs/`（**本地保留，不推 GitHub**）：`PRD.md`、`ARCHITECTURE.md`、`tech-constraints.md`、`API.md`。

## 安全

- 凭据只存 SQLite 与 `.env`（均被 `.gitignore` 忽略），仓库不含明文密钥。
- MCP 鉴权用 Bearer 令牌（常量时间比较，防时序侧信道）；令牌为空服务拒绝启动。
