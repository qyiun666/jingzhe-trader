# 惊蛰（Jingzhe Trader）

> A 股量化交易系统：每日自动选股、出买卖信号、生成指令单并邮件推送；季度目标驱动的动态风控档位；NAS 7×24 常驻，通过 **MCP 接口**供外部 AI Agent 读写。

## 它能做什么

- **每日盯盘流水线**：数据同步 → 新鲜度门禁 → 选股漏斗 → 信号 → 风控 → 指令单 → 邮件通知，全自动。
- **选股是一条流水线**：板块强弱 → 可用资金 → 流动性 → 估值 → 因子排名，每级进出数写日志与
  `run_trace(subject="job:evening_pipeline", outcome)`，候选与买卖决策只在内存里传，落库的结果只有待买卖表；0 候选必落 `alert:SCREEN_EMPTY` 一条轨迹，写清卡在哪一级、板块前三是什么——根治"永远 0 候选"的黑盒问题。
- **季度目标三档位状态机**：G1 标准 / G2 收紧 / G3 防守，落后时完全放开敞口 + 物理熔断兜底最坏情况。
- **LLM 决定买什么、买多少**（DeepSeek，4 条证据 prompt + 1 条决策 prompt），风控只做硬截断
  （单票/总仓/持仓数/一手价/金额下限/置信度）；均线与量比等规则信号降为喂进 prompt 的证据列。
  五条 prompt 都是**整批一次问**：筛选后的候选一次性发过去，所以一天的调用数恒为 5，与候选数无关。
  消息面一条走 Responses API 的 web_search 实测查当期风险公告（其余四条刻意不联网）；
  检索档配置为 high 且一档到底（早先"问不出就降到 low 再问一次"是误诊：真因是请求没传
  `max_output_tokens`，默认输出预算被 reasoning 吃满，整批 JSON 写到一半就停了）。
  所有 prompt 都禁止模型引用训练记忆里的新闻。模型：deepseek-v4-flash。
- **MCP 对外接口**（12 工具）：外部 Agent 负责「初始化当日流程 → 读买卖指令 → 人工下单后回执成交 → 校准账本 → 每天查日志」。
- **进程由外部 Agent 托管**：`nohup` 后台拉起，`/healthz` 探活，挂了重启（不再有 systemd/launchd 单元与独立看门狗）。

## 关键设计决策

| 决策点 | 结论 |
|--------|------|
| 季度目标落后时 | 完全放开敞口（允许自主放大仓位追赶），但由物理熔断框定底线 |
| 资金 / 季度目标 | 本金由 `jingzhe init` 写入（本机测试账户 2 万），季度 10%~20%（均可配置） |
| 现金口径 | 不建账户表：本金 − Σ成交推算；但持仓可以是券商校准进来的（无成交单支撑），此时按 `sync_portfolio`/`init` 给的可用资金落一个"现金锚点"，避免持仓成本被双算成可用资金 |
| LLM 角色 | 买入标的与数量的决策者（风控只截断）；卖出仍按规则执行，不让模型参与止损 |
| 数据 / 配置 | 全放 SQLite（`modernc.org/sqlite`，纯 Go 无 CGO） |
| 买卖执行 | 系统只出指令单，由人工在券商 App 执行后回执（不自动下单） |
| 通知投递 | 一封邮件一次机会：不建发件箱表，发不出去就是任务失败并落 `mail:<类型>` 的 fail 轨迹。当日一封的类型（M1 待买卖 / M2 计划 / M5 日报）与同一告警码的 M6 在重复触发时**只刷新轨迹行、不再重复投递**——`run_trace` 按 (交易日, subject) 覆盖成一行，补跑导致的多发邮件在日志里本来看不出来，收件箱却是实打实被刷爆 |

## 架构（流水线）

```
日历 → 日线+估值截面+复权+停牌+指数日线 → 新鲜度门禁 → 选股漏斗 → 目标档位/风控
        → LLM 决策(买) + 规则信号(卖) → 待买卖表 → 邮件 → 回执成交(人工)
                                        ↘ MCP 接口(外部 Agent 读写)
```

> 财务指标 / 利润表不在这条链路里：`fina_indicator` 与 `income` 只能按标的逐只查（全市场 5500+ 次调用），
> 历史版本为此建过慢路径与进度表，本轮重写已整体移除（`internal/tushare/` 只剩行情类接口）。
> 当前选股的价值因子只由每日估值截面的 PE(TTM) / PB 取倒数合成，不依赖财务报表。

## 目录结构

```
cmd/
  jingzhe/      单一二进制：serve（调度器 + MCP 接口）/ jobs / config / run
internal/
  store/        SQLite 存储与迁移（schema/仓储）
  config/       config_kv 配置（默认值/环境变量/凭据掩码）
  dataloader/   数据同步与新鲜度门禁
  market/       交易日历/成本/季度划分
  quote/        行情（只有 gotdx 一个源；取不到价即失败，不降级、不缓存旧价）
  screener/     选股漏斗
  signal/       买卖信号
  goal/         季度目标档位状态机
  risk/         风控与仓位
  ticket/       指令单/回执/资产实算
  notify/       邮件构建/折行/告警
  scheduler/    调度器（5 个触发点，每个是一个大方法顺序组装小方法）
  llm/          买入决策：4 条证据 prompt + 1 条决策 prompt（DeepSeek）
  mcp/          MCP 接口（12 工具：5 读 + 7 写）
  observability/ 日志（zap）
  model/        领域模型与枚举
  app/          组合根：全进程唯一的依赖装配
deploy/         Makefile、env.example（无服务托管单元，进程由外部 Agent 起）
docs/           设计文档（本地保留，不推 GitHub）
MCP-AGENT-GUIDE.md  外部 Agent 接入手册（根目录）
```

## 快速开始

前置：Go 1.27+（`modernc.org/sqlite` 纯 Go，无需 CGO）。

```bash
# 1) 配置凭据（复制模板并填入真实值）
cp deploy/env.example .env && chmod 600 .env
#   .env 需由启动方注入进程环境（本系统不自动加载 .env）

# 2) 构建
make -f deploy/Makefile build     # 产物 bin/jingzhe

# 3) 查看生效配置（凭据默认掩码）
./bin/jingzhe -db data/jingzhe.db config dump

# 4) 账户基线：本金 / 当前持仓 / 可用资金（首次必做，本金 write-once）
./bin/jingzhe -db data/jingzhe.db init -capital 20000 -hold "601233.SH:200:26.48"
#   省略 -cash 时可用资金按 本金 − 持仓成本 推算；给了 -hold 就必须给得出可用资金，
#   它落成"现金锚点"，否则校准进来的持仓成本会被双算成可用资金（见 sync_portfolio 同一条路径）

# 5) 手动跑一次当日闭环（任务名 = 调度器注册名，与 serve 到点触发同一条路径）
./bin/jingzhe -db data/jingzhe.db run task evening_pipeline --date 20260903
#   日历补齐 → 行情同步 → 新鲜度门禁 → 档位评估 → 选股漏斗 → LLM 决策 → 落待买卖表
./bin/jingzhe -db data/jingzhe.db run task mail_pending  --date 20260903
./bin/jingzhe -db data/jingzhe.db run task daily_report  --date 20260903
#   另有 4 个只有 CLI 提供的数据面任务：calendar / daily / freshness / screen
#   （screen 是只试跑漏斗不落单；freshness 不新鲜时非零退出，便于脚本判读）

# 6) 启动常驻服务（调度器 + MCP 接口，供外部 Agent 接入）
./bin/jingzhe -db data/jingzhe.db serve -addr :8080
```

## 单一二进制的五个子命令

| 子命令 | 作用 |
|--------|------|
| `serve`（默认） | 常驻服务：调度器跑 5 个触发点 + MCP 对外接口。09:00 当日计划邮件 / 09:30-11:30·13:00-15:00 每 5 分钟盘中扫描 / 16:30 选股流水线 / 17:00 待买卖表邮件（空表不发）/ 18:00 日报 + 保留清理。`/healthz` 免鉴权、`/mcp` 需 Bearer 令牌；令牌为空拒绝启动。装配期还拒绝两类"跑起来也是假绿"的状态：本金未 `init`、邮件配置不完整 |
| `jobs` | 演练指定交易日的时间线（dry-run，不执行任务），用于核对 `scheduler.*` 配置 |
| `config` | `dump` / `get KEY` / `set KEY VALUE`（凭据默认掩码，`--show-secrets` 才显示明文） |
| `init` | 写账户基线：`-capital` 本金（= 期初总资产，write-once）、`-hold 代码:股数:成本` 当前持仓、`-cash` 可用资金（省略则按本金−持仓成本推算）；顺带补齐 config_kv 默认值 |
| `run task` | 手工执行单个任务。接受两类名字：**调度器注册名** `morning_plan`/`intraday_scan`/`evening_pipeline`/`mail_pending`/`daily_report`（与 `serve` 同一份注册表、同一条 `runJob`，落库口径一致），以及**只有 CLI 提供的数据面任务** `calendar`/`daily`/`freshness`/`screen`（接入与排查用，没有到点触发） |

## MCP 对外接口（给外部 Agent）

系统通过 `serve` 暴露 **12 个 MCP 工具**（5 读 + 7 写），外部 AI Agent 据此完成：

1. **初始化**：`init_day` / `trigger_task`
2. **读指令**：`get_brief` → `get_tickets`（候选与信号不落库，漏斗计数在 `get_logs`）
3. **回执与校准**：`report_fill`（券商 App 人工下单后回报）/ `sync_portfolio`（存量持仓 + 本金 + 可用资金；
   校准进来的持仓没有成交单支撑，所以必须同时给 `available_cash_yuan`，否则持仓成本会被双算成可用资金）
4. **干预**：`skip_ticket`（作废指令单）/ `set_gear`（人工改档）/ `confirm_pace`（激进策略续期）
5. **查日志**：`get_logs`（失败与降级都在返回的 `trace` 数组里，含 `llm:*` 每只票每条 prompt 的结果）

完整接入手册（启动方式、协议、工具参数、每日工作流、错误处理、安全红线）见根目录 **[`MCP-AGENT-GUIDE.md`](./MCP-AGENT-GUIDE.md)**；接口速查见 `docs/API.md`。

## 配置

- 全部配置存 SQLite 的 `config_kv` 表（单一数据源），键目录见 `internal/config/keys.go`。
- 生效优先级：环境变量 `JZ_*` > 库内值 > 默认值。
- 键目录只保留**确实有消费方**的键：仓位/止损/持仓数由 `risk.GearTable` 按档位给出，不做配置项暴露。
- 环境变量模板：`deploy/env.example`（含 Tushare / LLM / 邮件 / MCP 令牌 / 告警收件人）。
- 注意：根目录 `.env`（真实凭据）被 `.gitignore` 忽略；模板之所以放在 `deploy/` 而非根目录 `.env.example`，是因为 `.gitignore` 的 `.env.*` 规则会连带忽略后者。

## 部署（NAS 7×24）

不提供 systemd / launchd 单元：进程由外部 Agent 以 `nohup ... serve &` 拉起，用 `curl -sf /healthz` 探活、`pgrep` 判存活，异常退出（非零码）后负责重启。关停走 SIGTERM，先停止接单再等在途任务写完，最后关库。

## 测试

```bash
go test ./...    # 单元：外部依赖全部打桩
go vet ./...

# 集成：真的打 Tushare / DeepSeek / SMTP / gotdx / MCP 接口
set -a; . ./.env; set +a
JZ_ITEST=1 go test ./itest -timeout 25m
```

`itest/` 只在 `JZ_ITEST=1` 时跑，凭据缺失一律 skip 而不是假通过。它证明的是"这些接口
今天真的能用"：真拉一次全市场日线、真发一封邮件到 `watch.mail_to`、真问一次 DeepSeek、
真取一次实时价，并按时间顺序把一个完整交易日的闭环（同步 → 门禁 → 选股 → LLM 决策 →
指令单 → 四类邮件 + 盘中止损告警 → 组合同步 → 改档 → MCP 读接口）跑通。

## 文档

- 根目录 `MCP-AGENT-GUIDE.md`：外部 Agent 接入手册（会随代码推送）。
- `docs/`（**本地保留，不推 GitHub**）：`PRD.md`、`ARCHITECTURE.md`、`tech-constraints.md`、`API.md`。

## 安全

- 凭据只存 SQLite 与 `.env`（均被 `.gitignore` 忽略），仓库不含明文密钥。
- MCP 鉴权用 Bearer 令牌（常量时间比较，防时序侧信道）；令牌为空服务拒绝启动。
