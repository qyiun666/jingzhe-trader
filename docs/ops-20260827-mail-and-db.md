# 2026-08-27 部署与处置手册（邮件未送达 / 调度卡死 / 数据库空洞）

> 面向在部署机上执行本次升级的自动化 Agent 或运维人员。
> 本文档不含任何密钥与私密信息；所有凭据一律走环境变量注入。

## 一、事故摘要与根因

### 1. 任务全部成功但收不到任何邮件
- 根因：邮件通知需**同时**满足三个条件——`mail.enabled=true`、`mail.from` 非空、进程环境已注入 `JZ_MAIL_PASSWORD`。任一缺失时，发送调用静默 no-op（旧版既不打日志也不告警），任务照常记成功。
- 改动：本次修复加入可见性（见第二节）。
- 生产动作：必须补齐上述三项配置，否则依旧不会发信。

### 2. 16:30 清理任务启动后，当日 signal/report 连 failed 记录都没有
- 现象特征：`job_run` 中某任务的 `running` 行长期不更新，之后的其他任务完全无行（连插行都做不到）。
- 根因（推断）：数据清理长事务占住 SQLite 单写锁，后续任务的首次落库被无限期阻塞；进程表面存活。
- 处置：重启进程即可恢复。调度器自带当日补跑（`maybeRunDaily` 按当日成功与否判断），过点的 signal/report 会在重启后的第一个调度周期自动补执行并推送日报。

### 3. 库文件约 194MB，但实际数据只有几 MB
- 根因：`auto_vacuum=0` 的历史库。大表删除后页不归还 OS，文件长期不收缩（实测空闲页占比 98%+）。
- 处置：见部署步骤第 5 步一次性 VACUUM；此后系统每周日检查并自动增量回收。

## 二、本次代码变更

| 文件 | 变更 |
|---|---|
| `internal/notify/mail.go` | 未完整配置时的发送调用由纯静默改为先记 `Warnw` 日志再跳过 |
| `internal/scheduler/scheduler.go` | 新增 `warnMailDisabled(scene)`：每日最多往 `agent_alert` 落一条「⚠️ 邮件通知未配置」告警 |
| `internal/scheduler/scheduler_tasks.go` | 盘前总结 / 日报推送 / 止损即时告警三个发信场景接入上述告警 |

行为影响：不发任何推送时当天必有一条告警可查，杜绝“全绿但零邮件”的无声故障；其余语义不变。

## 三、部署步骤（顺序执行）

1. 拉取最新 `main` 分支。
2. 编译：`make build`（产物 `bin/jingzhe-server`）。不要用裸 `go run` 长驻运行。
3. 停止旧进程：先 `kill -TERM`，确认进程退出、监听端口释放。
4. 备份数据目录：`cp -a data data.bak.$(date +%Y%m%d-%H%M)`（连同 `-wal/-shm` 一起）。
5. （推荐）一次性空间回收：`sqlite3 data/jingzhe.db "VACUUM;"` —— 务必在服务停止状态下执行；临时磁盘占用≈当前库大小，耗时数分钟。
6. 邮件配置自检：
   - 配置文件中 `mail.enabled=true`、`mail.from=<发件邮箱>`；
   - 以环境变量 `JZ_MAIL_PASSWORD=<SMTP授权码>` 注入新进程（launchd/systemd 各自的环境注入方式）；
   - 三项缺一不可，缺了会有当日的「⚠️ 邮件通知未配置」告警提示。
7. 用新编译的二进制常驻启动；观察日志出现「调度器启动」。
8. 等待一个调度周期：当日未成功的 signal/report 会自动补跑（signal 可能触发 LLM 辩论，耗时数分钟属正常）。
9. **收到日报邮件 = 全链路验证成功。**

## 四、部署后自检清单

- [ ] `job_run` 当日存在 signal / report 的 `success` 记录；
- [ ] `agent_alert` 无新的「⚠️ 邮件通知未配置」（若出现，按第三节第 6 步补配置后重启）；
- [ ] 收件箱已收到盘前总结 / 日报邮件；
- [ ] 独立通路备用验证（绕开 server，直接测 SMTP）：`make email-report SMTP_SERVER=smtp.qq.com SMTP_USER=xxx SMTP_PASS=xxx TO=xxx REPORT=reports/daily_report_xxx.html`。

## 五、注意事项

- SMTP 授权码只允许存在于环境变量 `JZ_MAIL_PASSWORD`，禁止写入任何 yaml / 代码 / 文档。
- `data/*.db`、`logs/` 已被 `.gitignore` 排除且历史上从未入库，禁止手工 `-f` 强加提交。
- 若再次遇到"某任务 running 后再无任何行"，优先怀疑写锁阻塞而非功能缺陷：备份数据目录后重启即可恢复并自动补跑。
