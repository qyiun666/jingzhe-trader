# 惊蛰（Jingzhe Trader）MCP 对外接口 · 外部 Agent 使用指南

> 本文档面向**接入本系统的外部 AI Agent**（以下简称「你」）。
> 读完本文，你应当能够：连上服务、读懂每日总览、按指令在券商 App 人工下单后回执成交、每天核查日志并处理告警。
> 代码已通过冒烟测试（`internal/mcp/smoke_test.go` 中的 `TestMCPSmoke` 验证 healthz / 鉴权 / initialize / tools.list(12 个工具名逐个比对) / 读工具 / 写工具幂等语义全通）。

---

## 0. 一句话定位

惊蛰是一个 **A 股量化交易系统（Go）**，核心引擎（选股、信号、风控、目标档位、邮件通知、调度）跑在本地/NAS 上。
你（外部 Agent）**不负责交易决策与下单执行**，你负责**出入接口层**：

| 你的职责 | 对应工具 |
|---------|---------|
| 启动 / 探活 / 重启服务进程 | `GET /healthz` + shell（见第 1 节） |
| 每日初始化当日流程 | `init_day` / `trigger_task` |
| 读取系统给出的买卖指令 | `get_brief` → `get_tickets` |
| 人工下单后回执成交（更新买卖信息） | `report_fill` |
| 首次接入与纠错：用券商实际持仓校准账本 | `sync_portfolio` |
| 认为某张指令单不该执行时作废它 | `skip_ticket` |
| 每天查日志、看什么成了什么砸了 | `get_logs` |
| 必要时人工覆盖档位 / 确认激进节奏策略 | `set_gear` / `confirm_pace` |

**关键约束（务必记住）**：系统产出「指令单（order_ticket）」，你在券商 App 里**人工执行**买卖，执行完用 `report_fill` 把真实成交回报给系统。系统**不会**自动连券商下单。这是有意为之的人机协作闭环，避免自动交易风险。

---

## 1. 服务如何启动（你需要连的端点）

惊蛰是**单一二进制 `jingzhe`**：一个进程内同时跑「定时任务调度 + MCP 对外接口」。
没有 systemd / launchd 托管——**启动、探活、重启都是你的职责**：

```bash
# 构建（注意：Makefile 在 deploy/ 下，只能在仓库根用 -f 调用；
#       make -C deploy 会因为找不到 ./cmd/jingzhe 而失败）
make -f deploy/Makefile build          # 产物 bin/jingzhe

# 启动并挂后台（日志落 logs/）
mkdir -p logs
nohup ./bin/jingzhe -db data/jingzhe.db serve -addr :8080 >> logs/jingzhe.log 2>&1 &
```

- `-db`　SQLite 数据库路径（默认 `data/jingzhe.db`）
- `serve`　常驻子命令；省略子命令时默认就是 serve
- `-addr`　监听地址（默认 `:8080`，即全部网卡）。只在可信内网暴露，令牌必须非空

### 你要负责的探活与重启

进程**不会**被任何守护器拉起。判断是否要重启，看这三条任一成立：

```bash
# 1. 探活：-f 让 503 也算失败
curl -sf http://<nas-ip>:8080/healthz

# 2. 进程是否还在
pgrep -f 'jingzhe .*serve'

# 3. 上次是怎么退出的：日志末行出现"子系统停摆，服务退出（非零码，需外部重启）"即需重启
tail -n 5 logs/jingzhe.log
```

> 后台进程由 `nohup` 拉起、拉起它的那个 shell 早就退出了，**它的退出码无处可取**——
> 所以判"要不要重启"只能靠上面这三条：端口/进程不在，或日志末行写着子系统停摆。

`/healthz` 反映的是**真实状态**，不只是"端口通"：

```json
{
  "status": "ok",                    // ok | unhealthy
  "service": "jingzhe",
  "time": "2026-09-03T05:13:46Z",
  "uptime_s": 3,
  "scheduler_running": true,         // 调度主循环是否在跑
  "last_tick_ago_s": 3               // 距上一轮调度判定多少秒
}
```

- `scheduler_running=false` 或 `last_tick_ago_s > 120` → **HTTP 503**，说明任务已经停摆，需要重启
- 任一子系统（调度器 / MCP 接口）停摆时，进程会**自行退出并返回码 1**，不会出现"进程活着但没服务"

重启前先停旧的，避免端口占用导致新进程立即退出：

```bash
pkill -f 'jingzhe .*serve'
```

收到 `SIGTERM` 后进程会先停止接单、等在途任务写完库再退出（正常退出码 0），可以直接 kill。

进程自己按 09:00→18:00 的 5 个触发点跑完一个交易日（可用 `jingzhe jobs -date YYYYMMDD`
先演练当日时间线核对，dry-run 不执行任何任务）。

**API 令牌（Bearer Token）优先级**：
1. 环境变量 `JZ_SERVER_API_TOKEN`
2. 配置表 `server.api_token`

> 令牌为空 → 服务**拒绝启动**（安全设计，杜绝无令牌接入）。

你拿到的连接信息通常形如：

```
端点   ： http://<nas-ip>:8080/mcp
鉴权   ： Authorization: Bearer <JZ_SERVER_API_TOKEN>
健康   ： http://<nas-ip>:8080/healthz   （免鉴权；调度停摆返 503，用 curl -sf 判活）
```

---

## 2. 传输协议（JSON-RPC 2.0 over HTTP）

所有业务调用走 `POST /mcp`，请求体为 JSON-RPC 2.0：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": { "name": "get_brief", "arguments": { "date": "20260901" } }
}
```

必须带请求头：
```
Content-Type: application/json
Authorization: Bearer <token>
```

### 三个方法

| method | 用途 | 鉴权 |
|--------|------|------|
| `initialize` | 握手，拿协议版本/能力 | 需要 Bearer |
| `tools/list` | 列出全部 12 个工具及其 inputSchema | 需要 Bearer |
| `tools/call` | 调用具体工具（`params.name` + `params.arguments`） | 需要 Bearer |

`/healthz` 走 GET，免鉴权：健康返回 200，调度停摆返回 **503**（响应体字段见第 1 节）。

### 响应形态

- **鉴权失败**：HTTP `401`，body 为 JSON-RPC error：
  ```json
  {"jsonrpc":"2.0","error":{"code":401,"message":"未授权：缺少或错误的 Bearer 令牌"}}
  ```
- **工具正常返回**：HTTP `200`，`result.content[0].text` 是 JSON 字符串（工具业务结果已 `json.MarshalIndent`）：
  ```json
  {"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{ ...业务结果... }"}],"isError":false}}
  ```
- **工具业务错误**：HTTP `200`，`result.isError:true`，`content[0].text` 以 `"错误: "` 开头。你应当解析它并视情况重试或转人工。
- **协议层错误**（未知工具/参数错）：HTTP `200`，顶层 `error` 对象（code 见 JSON-RPC 标准）。

> **实操建议**：解析响应时，先看 `error`（顶层）是否存在；否则看 `result.isError`；否则取 `result.content[0].text` 当 JSON 解析。

---

## 3. 工具清单（共 12 个）

读类（5）：`get_brief` `get_tickets` `get_positions` `get_portfolio` `get_logs`
写类（7）：`init_day` `report_fill` `sync_portfolio` `skip_ticket` `set_gear` `confirm_pace` `trigger_task`

> 日期参数统一为 `YYYYMMDD`（如 `20260901`），缺省为「今天」——按**交易所时区 Asia/Shanghai** 判定，
> 与承载机器的时区无关（UTC 主机上北京时间 08:00 前机器日期还是昨天）。
> 格式在服务端分发前统一校验（`date` 与 `set_gear.until` 都算），不合式立即返回工具错误并在服务端留一行
> `MCP 调用被拒：日期格式非法`。**读工具不会把坏日期当成「当天没有数据」返空列表**——那会让 agent 把
> 一次拼错的调用读成「今天无事」。
>
> **金额单位（读与写不对称，看清楚再动手）**：
> - 写工具入参用**元**（`report_fill.price`、`sync_portfolio.cost_price`、`initial_capital_yuan`），系统换算成分存储；
> - 读工具直接输出内部模型，金额字段是**分**（如 `get_positions` 里 `CostPrice:1120` = 11.20 元）；
> - 只有 `get_brief` 的 `portfolio` 段已换算为元并带 `_yuan` 后缀（`cash_yuan` / `market_value` / `total_asset`）。
>
> 读工具的字段名是 Go 结构体字段（PascalCase，如 `TsCode` / `TotalQty`）；空结果一律返回 `[]` 而不是 `null`。

### 3.1 get_brief（每日第一入口）
读取当日总览：数据新鲜度、阻断项、当日指令单与待执行数、持仓数、账户资产、目标进度。
- 参数：`date?`（string）
- 关注返回里的：
  - `data_fresh`（bool）——**为 false 时今天不应做任何交易动作**
  - `blockers`（[]string）—— 若有 `DATA_STALE` 等，说明当日不宜交易
  - `tickets_total` / `tickets_pending`（指令单计数，当日唯一落库的决策结果）
  - `positions`（持仓只数）、`pipeline_done`（收盘流水线今日是否已成功跑完）
  - `portfolio`（cash_yuan / market_value / total_asset / position_count / gear）
  - `goal`（季度目标进度、当前档位）
- 典型调用：`tools/call` `get_brief` `{"date":"20260901"}`

### 3.2 get_tickets
读取指令单（order_ticket）——**你在券商 App 执行的依据**。
- 参数：`date?`、`status?`（`drafted`/`issued`/`filled`/`skipped`/`expired`，空=全部）
- 返回：`{trade_date, tickets:[...]}`
- `status` 为 `issued`/`drafted` 且未 `filled` 的，才是当天待执行单。
- 状态名不在状态机内会直接返回业务错误（不再静默给你空列表）。终态只有 `filled`/`skipped`/`expired`。

### 3.3 get_positions
读取当前持仓快照。
- 参数：无
- 返回：`{positions:[...]}`

### 3.4 get_portfolio
读取**当前**账户资产：可用资金、持仓市值、总资产、持仓数 + 当日生效档位（金额字段以元计，键名 `*_yuan`）。
- 参数：无
- 数值全部现场推算：现金 = 本金 − Σ买入总成本 + Σ卖出净到账，市值 = 持仓 × 最新收盘价（停牌取停牌前收盘）。
  系统不再存每日资产快照，所以取不到"历史某一天"的资产；历史序列看每日 18:00 日报。

### 3.5 get_logs（每天查日志）
读取当日全部轨迹行（run_trace）。
- 参数：`date?`
- 返回：`{trade_date, trace:[...]}` —— 注意顶层键是**单数 `trace`**（不像 `tickets`/`positions` 那样带 s），按 `traces` 取值会拿到空。
- `trace[]` 是当日所有 job / alert / mail / LLM 记录的集合，每条含 `subject`（`job:evening_pipeline` / `alert:SCREEN_EMPTY` / `mail:M5` / `llm:600000.SH:news`）与 `outcome`（ok/partial/fail）。
  LLM 每条 prompt 对每只票的结论与理由也在这些行里（`detail` 是 JSON：`v`结论 / `c`置信 / `w`仓位 / `r`理由 / `e`失败原因）。
- 这是你「每天查日志」的主工具：检查是否有 `fail` 轨迹、是否有未处理告警。

### 3.6 report_fill（回执成交 / 更新买卖信息）
你在券商 App 人工下单后，回报一笔真实成交。**这是你「更新买卖信息」的核心动作。**
- 参数（必填 `qty`、`price`；`ticket_id` 与 `ts_code` 至少其一）：
  - `ticket_id?`（number，优先）
  - `ts_code?`（string，无 ticket_id 时按当日活跃单匹配）
  - `qty`（number，股数）
  - `price`（number，成交价·元）
  - `note?`（string）
  - `actor?`（string，缺省 `mcp-agent`）
- 行为：
  - 给 `ticket_id` → 直接记账该单成交；
  - 只给 `ts_code` → 在当日活跃单中精确匹配，**命中 0 张或多张**会返回 `need_confirm:true`（不记账），并附 `candidates` 列表让你补齐 `ticket_id` 后重试。
  - 成功返回 `{need_confirm:false, duplicate:bool, fill:{...}}`。`duplicate:true` 表示幂等去重（重复回报被忽略，安全）。
- 典型调用：`tools/call` `report_fill` `{"ticket_id":42,"qty":1000,"price":12.35}`

### 3.7 init_day（每日初始化）
检查数据新鲜度；当日收盘流水线（`evening_pipeline`）尚未成功跑完就补跑一次。**你每日开工第一步。**
- 参数：`date?`
- 返回三态：`{date, fresh:true, already_ran:bool, ran:"evening_pipeline"}`（当日已跑完 / 已补跑）、
  `{fresh:false, message, detail}`（数据不新鲜，**今日不要做任何交易动作**）、
  `{date, skipped:true, message}`（**非交易日**，当日没有流程可初始化，也不会去补跑流水线）。
- 补跑走的是调度器同一条任务路径（同步→门禁→档位→选股→决策→写单），不是另拼一套逻辑，
  所以手工补跑与到点自动跑的结果一致，同样落一条 `run_trace(subject="job:evening_pipeline", outcome)`。
- 拿 `date` 调这个工具前先确认它是交易日；早先版本非交易日会放行、再在流水线里报
  「数据不新鲜 …… 总体: 跳过（非交易日）」这种自相矛盾的失败，现在不会了。

### 3.8 set_gear
人工覆盖档位（G1/G2/G3）。覆盖会解除锁利；`until` 空则默认当日。
- 参数：`gear`（G1/G2/G3）、`reason`（必填）、`until?`、`actor?`
- 结果直接写在 `config_kv` 的 `goal.state` 这一个 JSON 值里（`override_gear` / `override_reason` / `override_until` 三个字段；当前生效档位就是 `current_gear`）。
  档位状态只有一行、每次整行覆盖，已并入配置表；变更过程只记服务日志，没有"档位变更历史"这张表。

### 3.9 confirm_pace
确认执行激进节奏策略（`pace_policy` 的人工放行出口）；未确认时该策略不生效。
- 参数：`date?`
- 结果写在 `goal.state` 的 `pace_policy` / `pace_confirm_date` 两个字段，返回 `{date, confirmed:true}`。

### 3.10 trigger_task
立即执行一个触发点（补跑/调试），任务名与自动时刻表完全一致：
`morning_plan` / `intraday_scan` / `evening_pipeline` / `mail_pending` / `daily_report`。
- 参数：`task`（必填）、`date?`
- 与到点触发走同一条 `run_trace` 记账路径（outcome 为 ok/fail），返回 `jobs` 字段列出全部可选名。
- 注意：这只是**即时补跑**；每日自动时刻表由 `scheduler.*` 配置键决定（见第 5.2 节）。
- **补跑不会重复发通知**：M1（待买卖）/ M2（计划）/ M5（日报）当日各只投一封，已投递成功过再补跑
  `morning_plan` / `mail_pending` / `daily_report`，只会在服务日志留一行"跳过重复发送"，任务照常算完成。
  `intraday_scan` 每轮最多一封 M3（内容随本轮新单变化），不受此限；urgent 告警（M6）**同一个 code 当天也只发一封**，
  但轨迹行每次都会刷新——所以想知道"这件事今天砸了几次"要看 `get_logs` 的 `detail`，不能数邮件。

### 3.11 sync_portfolio（首次接入与纠错，必读）
以**券商实际持仓**为准校准账本。系统只知道自己指令单回报过的成交——你接管一个已有持仓的账户时，
必须先用它把存量持仓、本金和可用资金灌进账本，否则卖出规则、仓位上限、季度基准全部算错。
命令行侧有等价入口：`jingzhe init -capital … -hold 代码:股数:成本`（同一个实现，只是不用起服务）。
- 参数：
  - `positions`（必填，数组）：每项 `{ts_code, total_qty, available_qty?, today_bought?, cost_price?, high_price?}`
    （数量=股，价格=元。库里只存 `total_qty` 与 `today_bought`，可卖量一律按 `total_qty − today_bought` 现算：
    给了 `today_bought` 就用它；只给 `available_qty` 时按差额反推出 `today_bought`）
    - **`ts_code` 与 `total_qty` 每一项都必须给**，服务端按 schema 的 `required` 强制校验，漏传直接拒绝
      并指明第几项缺哪个字段。以前漏 `total_qty` 会按 0 记账，等于把一次拼错的调用变成"这只票清仓了"，
      响应还是成功。**显式传 `total_qty: 0` 依然合法**（券商那边确实卖光了），要挡的是"没给这个键"。
  - `available_cash_yuan`（**必填**，number）：券商口径的可用资金（元）。校准进来的持仓没有成交单支撑，
    系统把它落成**现金锚点**：锚点当日及之前的成交不再重复扣减现金，之后的成交才动现金。
    不给这个参数直接报错——否则那笔持仓成本会被算两遍（一遍在持仓、一遍在可用资金），账户虚增。
  - `initial_capital_yuan?`（number）：本金 = 期初总资产（**元**，含持仓成本），`0`/省略 = 不动本金。
    它是季度目标的基准，不是可用资金。
  - `date?`、`actor?`
- **本金是 write-once**：首次写入生效；库里已有非零本金时再传不同值，**本金不改**、
  持仓与现金照常同步，返回 `capital_rejected:true`，并在服务日志落一条 warn（拒绝这件事不建表，也不占当日轨迹行）。
  要修正本金必须走人工复核（直接改库），不要指望这个接口。
- 返回 `synced`（同步了几只）、`cash_after_sync`（账本此刻的可用资金）、`capital_rejected?`。
- 典型调用：
  ```json
  {"name":"sync_portfolio","arguments":{
    "date":"20260901","initial_capital_yuan":20000,"available_cash_yuan":14704,
    "positions":[{"ts_code":"601233.SH","total_qty":200,"cost_price":26.48}]}}
  ```

### 3.12 skip_ticket
作废一张指令单（置 `skipped`，终态）。你判断某张单不该执行时用。
- 参数：`ticket_id`（必填）、`reason`（**必填**）、`actor?`
- 只有 `drafted` / `issued` 可跳；`filled` / `skipped` / `expired` 是终态，会返回非法转移错误。
- 与 `report_fill` 的区别：`report_fill` 表示**真的成交了**，`skip_ticket` 表示**没成交、不再执行**。
- 留痕只在被改的那一行上：作废结果写在 `order_ticket`（`status=skipped` + `note` + `reported_by/reported_at`），
  `reason` 进服务日志。**当日轨迹里不会有 `ticket:skip` 这一行**（`run_trace` 只有 `job:` / `alert:` / `mail:` / `llm:`
  四类 subject），别去 `get_logs` 找它，用 `get_tickets` 复核状态即可。

---

## 4. 每日标准工作流（照着做）

> 以下用「你（Agent）」的视角，按时间顺序。括号内是对应工具。

### 第 0 步：首次接入只做一次
如果 `get_positions` 返回空但你已知券商里**有存量持仓**，先跑 **`sync_portfolio`** 把实际持仓与本金灌进账本。
跳过这一步，后面的仓位上限、卖出规则、季度目标进度全是错的。

### 上午 / 开盘前
1. **`init_day`** —— 初始化当日。看返回：
   - `fresh:false` → 停止，转人工：「数据未就绪，等待调度器同步」。
   - `fresh:true` → 继续。
2. **`get_brief`** —— 读总览。重点看：
   - `blockers` 是否为空；`data_fresh` 是否为 true。
   - `goal.Gear`（当前档位，注意读工具输出的是 Go 字段名 `Gear`/`GearLabel`）、`portfolio`（现金/市值）。
   - 若有阻断项 → 不要继续，先处理或转人工。
3. **`get_tickets` `{"status":"issued"}`**（或 `drafted`）—— 取出当日**待执行指令单**。这就是你要在券商 App 里操作的清单。
4. （可选）**`get_logs`** —— 看返回的 `trace[]`：`subject~"job:evening_pipeline"` 的 `outcome="ok"` 表示成功；若有 `outcome="fail"` 则需补跑。

### 交易时段
5. 在**券商 App 人工执行**第 3 步取到的买卖指令（买入/卖出）。
6. 每完成一笔，立刻 **`report_fill`** 回报真实成交：
   - 已知 `ticket_id`：`{"ticket_id":<id>, "qty":<股>, "price":<元>}`
   - 只有代码：`{"ts_code":"600000.SH", "qty":1000, "price":12.35}`；若返回 `need_confirm:true`，从 `candidates` 取 `ticket_id` 重试。
   - 重复回报会被 `duplicate:true` 安全去重，放心重试。
7. 若需强制调整档位，用 **`set_gear`**（结果写在 `goal.state` 的 override_* 三个字段上）。

### 收盘后 / 每日收尾
8. **`get_logs`** —— 查当日轨迹行：
   - 有没有 `outcome="fail"` 的 job/alert？如有 → 记入并视情况 `trigger_task` 补跑或转人工。
   - 有没有 `subject~"alert:"` 且 `outcome="fail"` 的告警？
9. 向用户/系统汇报当日执行与日志结论（可借助 `get_portfolio` `get_positions` 给出持仓与资产概览）。

---

## 5. 错误与边界处理（你必须会的判断）

### 5.1 错误信号对照

| 现象 | 含义 | 你的动作 |
|------|------|---------|
| HTTP 401 / `未授权` | Bearer 令牌缺失或错误 | 检查 `JZ_SERVER_API_TOKEN`；不要反复乱试（可能被限流） |
| `result.isError:true` 且 text 以 `错误:` 开头 | 工具业务报错（参数错/记录不存在等） | 解析 message，修正参数重试 |
| `report_fill` 返回 `need_confirm:true` | `ts_code` 命中多张单，无法唯一确定 | 从 `candidates` 取 `ticket_id`，补 `ticket_id` 重试 |
| `get_brief` 中 `data_fresh:false` 或 `blockers` 含 `DATA_STALE` | 当日数据不新鲜 | **停止交易动作**，等待调度器同步或转人工 |
| `init_day` 返回 `fresh:false` | 数据未就绪 | 停止，提示「等待数据同步」 |
| `report_fill` 返回 `duplicate:true` | 该笔成交已回报过（幂等去重） | 正常，忽略即可 |
| `sync_portfolio` 返回 `capital_rejected:true` | 本金为 write-once 已被拒 | 持仓已同步；本金需走人工复核，别重试 |
| `get_logs` 返回的 `trace[]` 有 `outcome="fail"` | 某任务执行异常 | 视情况 `trigger_task` 补跑或转人工 |
| 告警 `LLM_DISABLED` | 买入决策者未启用（`llm.enabled=false` 或缺 key）——当日**不可能有买单**，不是漏跑 | 转人工确认配置；不要靠补跑来"等出单" |
| 告警 `LLM_FAILED` | 部分候选评审未问出结果，明细在当日轨迹的 `llm:*` 失败行（`get_logs`） | 可 `trigger_task` 补跑（只重试失败的那几条 prompt） |
| `/healthz` 返回 503 | 调度主循环停摆或久无判定 | 重启 `serve` 进程（第 1 节） |
| 配置缺失类报错（如 `server.api_token` 为空） | 服务端启动自检未过 | 这是运维问题，联系部署方，不是你能在接口层修的 |

### 5.2 改配置：MCP 不提供写配置的口子

所有配置项（时刻表、选股阈值、成本费率、保留窗口、季度目标）只能经运维 CLI 改，**改完必须重启进程才生效**：

```bash
./bin/jingzhe -db data/jingzhe.db config dump                 # 看全部键与当前生效值
./bin/jingzhe -db data/jingzhe.db config set scheduler.pipeline 16:45
./bin/jingzhe -db data/jingzhe.db jobs -date 20260903          # 重启前先演练核对时间线
pkill -f 'jingzhe .*serve' && nohup ./bin/jingzhe -db data/jingzhe.db serve -addr :8080 >> logs/jingzhe.log 2>&1 &
```

- `scheduler.*`：4 个键 —— `scheduler.morning`(09:00) / `scheduler.pipeline`(16:30，整链一条顺序
  流水线) / `scheduler.mail_pending`(17:00 待买卖邮件) / `scheduler.report`(18:00 日报)。
  盘中扫描窗口（09:30–11:30 / 13:00–15:00，每 5 分钟）写在代码里，不占键。
- `screen.*` / `goal.*` / `cost.*` / `retention.*` / `risk.max_sector_pct` / `risk.take_profit_pct`：阈值与窗口。
  **仓位上限、持仓数、止损不在其中**——它们由 `risk.GearTable` 按档位给出，只能 `set_gear` 换档。
- 键目录里列出的每一个键都有真实消费方；不在这个清单里的键，`config set` 会直接报「未知配置键」。
- 凭据键（`tushare.token` / `mail.password` / `llm.api_key` / `server.api_token`）默认掩码，需 `--show-secrets`。

---

## 6. 最小可跑示例（curl）

```bash
export JZ_MCP="http://<nas-ip>:8080/mcp"
export TOKEN="<JZ_SERVER_API_TOKEN>"

# 1) 健康检查（免鉴权）
curl -s $JZ_MCP/../healthz   # 实际： GET http://<nas-ip>:8080/healthz

# 2) 握手
curl -s -X POST $JZ_MCP \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

# 3) 列工具
curl -s -X POST $JZ_MCP \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'

# 4) 每日总览
curl -s -X POST $JZ_MCP \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_brief","arguments":{"date":"20260901"}}}'

# 5) 取待执行单
curl -s -X POST $JZ_MCP \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_tickets","arguments":{"status":"issued"}}}'

# 6) 回执一笔成交（人工在券商 App 下单后）
curl -s -X POST $JZ_MCP \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"report_fill","arguments":{"ticket_id":42,"qty":1000,"price":12.35}}}'

# 7) 收尾查日志
curl -s -X POST $JZ_MCP \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_logs","arguments":{"date":"20260901"}}}'
```

---

## 7. 设计红线（给 Agent 的安全边界）

1. **你不替系统做买卖决策**：买入由引擎的 LLM 评审 + 风控硬截断生成指令单，卖出按持仓规则；你只负责读取与执行回报。
2. **你不直连券商自动下单**：所有成交都来自你在券商 App 的人工操作 + `report_fill` 回报。
3. **账本与券商只有两条对齐通道**：逐笔 `report_fill`（成交）与整体 `sync_portfolio`（校准）。
4. **`skip_ticket` ≠ `report_fill`**：没成交就 `skip_ticket`。把未成交的单回报成成交会凭空造出持仓与现金流出。
5. **数据不新鲜就停手**：任何 `DATA_STALE` / `data_fresh:false` 出现时，停止交易动作，转人工。
6. **可安全重试**：`report_fill` / `init_day` 均为幂等设计，重复调用不会重复记账。
7. **留痕**：人工干预的结果直接落在被改动的表上（作废 → `order_ticket.status=skipped`，改档 → `goal.state` 的 override_*），过程只记服务日志；当日成败汇总一律看 `get_logs` 的 `run_trace`。

---

## 8. 相关文件（供工程师参考，非 Agent 必读）

- `internal/mcp/server.go` —— HTTP 服务、鉴权、JSON-RPC 分发、`/healthz`
- `internal/mcp/tools_read.go` —— 5 个读工具
- `internal/mcp/tools_write.go` —— 7 个写工具（两处 schema 的权威来源）
- `internal/mcp/tools.go` —— 工具类型、schema 与参数解析辅助
- `internal/mcp/brief.go` —— `get_brief` 组装逻辑
- `internal/mcp/deps.go` —— MCP 的依赖契约（只声明字段，实例一律由 `internal/app` 注入）
- `internal/mcp/smoke_test.go` —— 冒烟测试（healthz/鉴权/12 工具在册/读写工具语义）
- `cmd/jingzhe/main.go` —— 单一二进制入口：子命令分派、token 优先级、优雅关机
- `internal/app/deps.go` —— 组合根：调度器与 MCP 共用同一批服务实例
- `internal/scheduler/jobs.go` + `internal/config/keys.go` —— 时间线与 `scheduler.*` 键的对应关系
