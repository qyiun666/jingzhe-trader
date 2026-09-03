# 惊蛰（Jingzhe Trader）MCP 对外接口 · 外部 Agent 使用指南

> 本文档面向**接入本系统的外部 AI Agent**（以下简称「你」）。
> 读完本文，你应当能够：连上服务、读懂每日总览、按指令在券商 App 人工下单后回执成交、每天核查日志并处理告警。
> 代码已通过冒烟测试（`internal/mcp/smoke_test.go` 中的 `TestMCPSmoke` 验证 healthz / 鉴权 / initialize / tools.list(13) / get_brief / get_logs 全通）。

---

## 0. 一句话定位

惊蛰是一个 **A 股量化交易系统（Go）**，核心引擎（选股、信号、风控、目标档位、邮件通知、调度）跑在本地/NAS 上。
你（外部 Agent）**不负责交易决策与下单执行**，你负责**出入接口层**：

| 你的职责 | 对应工具 |
|---------|---------|
| 每日初始化当日流程 | `init_day` / `trigger_task` |
| 读取系统给出的买卖指令 | `get_brief` → `get_tickets` / `get_candidates` / `get_signals` |
| 人工下单后回执成交（更新买卖信息） | `report_fill` |
| 每天查日志、处理告警 | `get_logs` / `ack_alert` |
| 必要时人工覆盖档位 / 留痕备注 | `set_gear` / `note` |

**关键约束（务必记住）**：系统产出「指令单（order_ticket）」，你在券商 App 里**人工执行**买卖，执行完用 `report_fill` 把真实成交回报给系统。系统**不会**自动连券商下单。这是有意为之的人机协作闭环，避免自动交易风险。

---

## 1. 服务如何启动（你需要连的端点）

惊蛰的常驻守护进程叫 `jingzhed`（不是一次性 CLI）。它由 systemd / launchd 在 NAS 上 7×24 拉起。

```bash
# 在部署机上启动（通常由守护进程负责，你无需手动跑）
jingzhed -db data/jingzhe.db [-addr :8080]
```

- `-db`  SQLite 数据库路径（默认 `data/jingzhe.db`）
- `-addr` 监听地址（默认 `:8080`）

**API 令牌（Bearer Token）优先级**：
1. 环境变量 `JZ_SERVER_API_TOKEN`
2. 配置表 `server.api_token`

> 令牌为空 → 服务**拒绝启动**（安全设计，杜绝无令牌接入）。

你拿到的连接信息通常形如：

```
端点   ： http://<nas-ip>:8080/mcp
鉴权   ： Authorization: Bearer <JZ_SERVER_API_TOKEN>
健康   ： http://<nas-ip>:8080/healthz   （免鉴权）
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
| `tools/list` | 列出全部 13 个工具及其 inputSchema | 需要 Bearer |
| `tools/call` | 调用具体工具（`params.name` + `params.arguments`） | 需要 Bearer |

`/healthz` 走 GET，免鉴权，返回 `{"status":"ok","time":"...","service":"jingzhe-trader-mcp"}`。

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

## 3. 工具清单（共 13 个）

读类（7）：`get_brief` `get_candidates` `get_signals` `get_tickets` `get_positions` `get_portfolio` `get_logs`
写类（6）：`report_fill` `init_day` `set_gear` `note` `ack_alert` `trigger_task`

> 日期参数统一为 `YYYYMMDD`（如 `20260901`），缺省为「今天」。金额在 `report_fill` 接口用**元**（如 `price: 12.35`），系统内部换算成分存储。

### 3.1 get_brief（每日第一入口）
读取当日总览：数据新鲜度、阻断项、候选/信号/持仓计数、账户快照、目标进度。
- 参数：`date?`（string）
- 关注返回里的：
  - `data_fresh`（bool）——**为 false 时今天不应做任何交易动作**
  - `blockers`（[]string）—— 若有 `DATA_STALE` 等，说明当日不宜交易
  - `candidates` / `signals` / `positions`（计数）
  - `portfolio`（cash_yuan / market_value / total_asset / position_count / gear）
  - `goal`（季度目标进度、当前档位）
- 典型调用：`tools/call` `get_brief` `{"date":"20260901"}`

### 3.2 get_candidates
读取当日选股候选（含五因子分项与理由）与漏斗诊断。
- 参数：`date?`、`limit?`（前 N 条，0=全部）
- 返回：`{trade_date, candidates:[...], funnel:[...]}`

### 3.3 get_signals
读取当日信号（买入/卖出、规则、状态）。
- 参数：`date?`
- 返回：`{trade_date, signals:[...]}`

### 3.4 get_tickets
读取指令单（order_ticket）——**你在券商 App 执行的依据**。
- 参数：`date?`、`status?`（`drafted`/`issued`/`filled`/`closed`/`expired`/`rejected`，空=全部）
- 返回：`{trade_date, tickets:[...]}`
- `status` 为 `issued`/`drafted` 且未 `filled` 的，才是当天待执行单。

### 3.5 get_positions
读取当前持仓快照。
- 参数：无
- 返回：`{positions:[...]}`

### 3.6 get_portfolio
读取账户快照（现金、市值、总资产、持仓数、当前档位）。
- 参数：`date?`
- 返回：最新 `account_snapshot` 记录。

### 3.7 get_logs（每天查日志）
读取当日任务执行记录（job_run，含 degraded/failed）与全部告警（agent_alert）。
- 参数：`date?`
- 返回：`{trade_date, jobs:[...], alerts:[...]}`
- 这是你「每天查日志」的主工具：检查是否有 `failed`/`degraded` 任务、是否有未处理告警。

### 3.8 report_fill（回执成交 / 更新买卖信息）
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

### 3.9 init_day（每日初始化）
初始化当日流程：检查数据新鲜度，若当日尚未选股/出信号则补跑（假定数据已就绪）。**你每日开工第一步。**
- 参数：`date?`
- 返回：`{date, fresh:bool, steps:[...]}`（`steps` 记录了实际补跑了哪些阶段，如 `["screener","signal"]`）
- 若 `fresh:false` → 提示「数据不新鲜，请先由调度器完成数据同步」，此时不要继续交易动作。

### 3.10 set_gear
人工覆盖档位（G1/G2/G3）。覆盖会解除锁利；`until` 空则默认当日。
- 参数：`gear`（G1/G2/G3）、`reason`（必填）、`until?`、`actor?`
- 会写 `goal_gear_log` 与 `action_log`。

### 3.11 note
追加一条操作日志到 `action_log`（审计留痕，不改动业务数据）。记录人工决策/备注用。
- 参数：`object_type?`、`object_id?`、`action?`（缺省 `note`）、`reason`（必填）、`actor?`

### 3.12 ack_alert
标记某条告警已读（你处理完毕后调用）。
- 参数：`alert_id`（number，必填）
- 返回：`{alert_id, ok:true}`

### 3.13 trigger_task
手动触发一个命名任务（补跑/调试）：`freshness` / `screener` / `signal` / `t1_settle`。
- 参数：`task`（必填）、`date?`
- 用于 init_day 之外的手动补跑，或排查数据。

---

## 4. 每日标准工作流（照着做）

> 以下用「你（Agent）」的视角，按时间顺序。括号内是对应工具。

### 上午 / 开盘前
1. **`init_day`** —— 初始化当日。看返回：
   - `fresh:false` → 停止，转人工：「数据未就绪，等待调度器同步」。
   - `fresh:true` → 继续。
2. **`get_brief`** —— 读总览。重点看：
   - `blockers` 是否为空；`data_fresh` 是否为 true。
   - `goal.gear`（当前档位）、`portfolio`（现金/市值）。
   - 若有阻断项 → 不要继续，先处理或转人工。
3. **`get_tickets` `{"status":"issued"}`**（或 `drafted`）—— 取出当日**待执行指令单**。这就是你要在券商 App 里操作的清单。
4. （可选）**`get_candidates` / `get_signals`** —— 了解候选与信号明细，便于向用户解释「为什么买/卖」。

### 交易时段
5. 在**券商 App 人工执行**第 3 步取到的买卖指令（买入/卖出）。
6. 每完成一笔，立刻 **`report_fill`** 回报真实成交：
   - 已知 `ticket_id`：`{"ticket_id":<id>, "qty":<股>, "price":<元>}`
   - 只有代码：`{"ts_code":"600000.SH", "qty":1000, "price":12.35}`；若返回 `need_confirm:true`，从 `candidates` 取 `ticket_id` 重试。
   - 重复回报会被 `duplicate:true` 安全去重，放心重试。
7. （可选）对人工干预/决策用 **`note`** 留痕；若需强制调档位用 **`set_gear`**。

### 收盘后 / 每日收尾
8. **`get_logs`** —— 查当日任务执行与告警：
   - `jobs` 里有没有 `failed` / `degraded`？如有 → 记入并视情况 `trigger_task` 补跑或转人工。
   - `alerts` 里有没有未处理项（如 `SCREEN_EMPTY`、`DATA_STALE`）？
9. 对每个已处理的告警 **`ack_alert`** `{"alert_id":<id>}`。
10. 向用户/系统汇报当日执行与日志结论（可借助 `get_portfolio` `get_positions` 给出持仓与资产概览）。

---

## 5. 错误与边界处理（你必须会的判断）

| 现象 | 含义 | 你的动作 |
|------|------|---------|
| HTTP 401 / `未授权` | Bearer 令牌缺失或错误 | 检查 `JZ_SERVER_API_TOKEN`；不要反复乱试（可能被限流） |
| `result.isError:true` 且 text 以 `错误:` 开头 | 工具业务报错（参数错/记录不存在等） | 解析 message，修正参数重试 |
| `report_fill` 返回 `need_confirm:true` | `ts_code` 命中多张单，无法唯一确定 | 从 `candidates` 取 `ticket_id`，补 `ticket_id` 重试 |
| `get_brief` 中 `data_fresh:false` 或 `blockers` 含 `DATA_STALE` | 当日数据不新鲜 | **停止交易动作**，等待调度器同步或转人工 |
| `init_day` 返回 `fresh:false` | 数据未就绪 | 停止，提示「等待数据同步」 |
| `report_fill` 返回 `duplicate:true` | 该笔成交已回报过（幂等去重） | 正常，忽略即可 |
| `get_logs` 中 `jobs` 有 `failed`/`degraded` | 某任务执行异常 | 视情况 `trigger_task` 补跑或转人工 |
| 配置缺失类报错（如 `server.api_token` 为空） | 服务端启动自检未过 | 这是运维问题，联系部署方，不是你能在接口层修的 |

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

1. **你不替系统做买卖决策**：买/卖由引擎依据信号+风控+目标档位生成指令单，你只负责读取与执行回报。
2. **你不直连券商自动下单**：所有成交都来自你在券商 App 的人工操作 + `report_fill` 回报。
3. **数据不新鲜就停手**：任何 `DATA_STALE` / `data_fresh:false` 出现时，停止交易动作，转人工。
4. **可安全重试**：`report_fill` / `init_day` 均为幂等设计，重复调用不会重复记账。
5. **留痕**：人工干预（覆盖档位、备注、告警处理）用 `note` / `set_gear` / `ack_alert` 写进审计日志。

---

## 8. 相关文件（供工程师参考，非 Agent 必读）

- `internal/mcp/server.go` —— HTTP 服务、鉴权、JSON-RPC 分发
- `internal/mcp/tools.go` —— 13 个工具的注册与 handler（权威 schema 来源）
- `internal/mcp/brief.go` —— `get_brief` 组装逻辑
- `internal/mcp/deps.go` —— 依赖装配（与 `run_cmd.go` 同款）
- `internal/mcp/smoke_test.go` —— 冒烟测试（healthz/鉴权/13 工具/get_brief/get_logs）
- `cmd/jingzhed/main.go` —— 常驻守护进程入口（token 优先级、flag）
