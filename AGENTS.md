# AGENTS.md — 惊蛰交易系统工程手册

> 本文件由工程（数字员工）维护，记录部署现状与工作日志。

---

## 一、系统概况

- **项目路径**: `/home/qodercli/work/jingzhe-trader`
- **数据库**: `/home/qodercli/work/jingzhe-db/jingzhe.db`（WAL 模式）
- **服务端口**: `:11270`（健康检查 `/healthz`）
- **GitHub**: qyiun666/jingzhe-trader

## 二、职责边界

工程负责：store / dataloader / scheduler / notify / mcp / tushare / quote 模块、部署运维、运维脚本。
策略负责：选股 / 信号 / 风控逻辑。
组长负责：交易监控、用户沟通。

## 三、部署流程（标准 8 步）

见 `/home/qodercli/work/scripts/deploy.sh`：测试 → 库备份 → 二进制备份 → 编译 → revision 核对 → 停旧 → 启动 → 验证。

## 四、红线

- 不推送 `data/`、`.env`、数据库文件到 GitHub
- 不用 `cp -a` 备份 WAL 模式数据库（用 sqlite backup API）
- 不用 `pkill -f` / `pgrep -f`（用精确 PID 匹配）
- 部署不在 09:30–15:00 盘中进行

---

## 五、现状覆盖块

### 2026-11-05 18:45 补部署（组长派工·19:30 前收口）

**部署批**（一次上完）：
- main@be877a8 ← strategy-b1b3@be877a8（ff 合入）
- 含 4 commits：072e206（db backup 入口）、8f1c394（README）、5cea9e9（B1 整手窗口 + B3 基金费用档）、be877a8（整手窗口筛光降级 + SCREEN_EMPTY 三分）

**前置验证**：
- go vet ./... ✅
- go test ./internal/... -count=1 ✅（18 包全过）
- diff 核验：18 文件、全 .go、+708/-39 ✅

**部署结果**：
- 库备份：`/home/qodercli/work/jingzhe-db/backup/jingzhe.db.pre-deploy-20261105-184513`（203MB, integrity=ok）
- 二进制备份：`bin/jingzhe.bak-20261105-184513`
- 旧 PID：1110602（5d6efdd）→ 新 PID：1357467（be877a8）

**逐项自验**：

| 检查项 | 结果 |
|--------|------|
| vcs.revision | be877a81e3da930b9ede742ec77f0a1766c45ac0 ✅ |
| vcs.modified | false ✅ |
| 新 PID | 1357467 ✅ |
| /healthz scheduler_running | true ✅ |
| /proc/1357467/fd (deleted) | 0 个 ✅ |
| fd 指向 jingzhe-db | fd 4/5/6/7/8 → jingzhe.db/wal/shm ✅ |
| db audit | exit 0，"库结构与现役 schema 一致" ✅ |
| get_brief cash | 11,737.83 ✅ |
| get_brief position_count | 0 ✅ |
| get_brief total_asset | 11,737.83 ✅ |
| get_brief blockers | [] ✅ |
| get_brief tickets_pending | 0 ✅ |
| screen.gate_enabled | false ✅ |
| signal.weak_buy_enabled | false ✅ |
| signal.expected_excess_pct | 0 ✅ |
| risk.trailing_arm_pct | 0.08 ✅ |
| risk.trailing_arm_safety_pct | 0.01 ✅ |
| signal.rank_out_weak_enabled | false ✅ |
| screen.gate_confirm_days | 3 ✅ |
| screen.etf_enabled | false ✅ |
| retention.bar_days | 850 ✅ |
| llm.review_batch_size | 5 ✅ |
| db backup 子命令 | exit 0, integrity=ok, 7 表 ✅ |

**GitHub 推送**：
- 出站恢复：5d6efdd..be877a8 main → main ✅
- ahead 4 commits 全部推上

---

## 六、工作日志

| 时间 | 事项 | 操作人 | 结果 |
|------|------|--------|------|
| 2026-11-05 18:45 | 补部署（catch-up 11:14 漏窗） | 工程 | be877a8 上线，PID=1357467，全项自验通过，push 成功 |
| 2026-11-04 19:15 | 常规部署 | 工程 | 5d6efdd 上线 |

---

## 七、运维脚本

- `*/10` 健康检查+自愈：`/home/qodercli/work/scripts/agent_health_check.sh`
- `0 9 1-5` 晨检：`/home/qodercli/work/scripts/agent_morning_check.sh`
- `15 18 1-5` 日报：`/home/qodercli/work/scripts/agent_daily_report.sh`
- `30 19 1-5` 链路核查：`/home/qodercli/work/scripts/agent_chain_check.sh`
