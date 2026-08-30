// Command watchdog 是进程外的"截止时间看门狗": 每个交易日盘后独立检查
// 信号链路 (数据更新 → EOD信号 → 日报推送) 是否全部成功, 缺项则用独立邮件通道告警。
//
// 与调度器内置告警的分工: 调度器只在自己活着时能报错; 进程被杀/机器重启未拉起/
// 调度循环卡死等"整体沉默"场景, 只能由外部 cron 定时运行的本程序兜底。
//
// 用法:
//
//	watchdog -config config/config.yaml          # 检查今天
//	watchdog -config config/config.yaml -date 20260831
//	watchdog -config config/config.yaml -mail-ok  # 全通过也发心跳邮件
//
// 退出码: 0=全部通过或非交易日, 1=有缺失(已尝试发告警), 2=自身无法完成检查
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/store"
)

// requiredJobs "准确时机告诉我买卖"的核心链路, 缺任何一环当日信号都不可信
var requiredJobs = []struct {
	name string
	desc string
}{
	{store.JobDataUpdate, "行情数据更新"},
	{store.JobSignal, "EOD买卖信号"},
	{store.JobReport, "日报推送"},
}

func main() {
	log.SetPrefix("[watchdog] ")
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	date := flag.String("date", "", "检查的交易日 YYYYMMDD (默认今天)")
	mailOK := flag.Bool("mail-ok", false, "全部通过时也发送心跳邮件")
	flag.Parse()

	code := run(*configPath, *date, *mailOK)
	os.Exit(code)
}

func run(configPath, date string, mailOK bool) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("加载配置失败: %v", err)
		return 2
	}
	db, err := store.NewDB(cfg.Database.Path)
	if err != nil {
		log.Printf("打开数据库失败: %v", err)
		return 2
	}
	defer db.Close()

	today := time.Now().Format("20060102")
	if date != "" {
		today = date
	}

	open, err := isTradingDay(db, today)
	if err != nil {
		log.Printf("交易日判断失败: %v", err)
		return 2
	}
	if !open {
		log.Printf("%s 非交易日, 跳过检查", today)
		return 0
	}

	missing, attempted, err := checkJobs(db, today)
	if err != nil {
		log.Printf("任务状态检查失败: %v", err)
		return 2
	}

	if len(missing) == 0 {
		log.Printf("%s 核心链路全部成功", today)
		if mailOK {
			if err := sendAlert(cfg, fmt.Sprintf("✅ 惊蛰信号链路心跳 %s", today),
				fmt.Sprintf("%s 数据更新/EOD信号/日报推送 全部成功。", today)); err != nil {
				log.Printf("心跳邮件发送失败: %v", err)
			}
		}
		return 0
	}

	body := ""
	if attempted == 0 {
		body = "⚠️ 今日没有任何任务运行记录: server 进程很可能未运行 (机器重启未拉起 / OOM / 误停)。\n\n"
	}
	body += "缺失明细:\n" + joinLines(missing) +
		"\n\n排查: 检查 jingzhe server 进程与日志, 确认后可手动补跑对应任务。"
	title := fmt.Sprintf("🚨 惊蛰看门狗: %s 信号链路有 %d 环未完成", today, len(missing))
	log.Printf("发现 %d 个缺失任务, 发送告警", len(missing))
	if err := sendAlert(cfg, title, body); err != nil {
		log.Printf("告警邮件发送失败: %v", err)
		return 2
	}
	return 1
}

// checkJobs 返回缺失任务描述与"当日有尝试记录"的任务数
func checkJobs(db *sqlx.DB, today string) (missing []string, attempted int, err error) {
	jobRepo := store.NewJobRepo(db)
	for _, j := range requiredJobs {
		ok, qerr := jobRepo.HasSucceeded(j.name, today)
		if qerr != nil {
			return nil, 0, fmt.Errorf("查询任务 %s 失败: %w", j.name, qerr)
		}
		last, qerr := jobRepo.LastAttemptStartedAt(j.name, today)
		if qerr != nil {
			return nil, 0, fmt.Errorf("查询任务 %s 尝试时间失败: %w", j.name, qerr)
		}
		if !last.IsZero() {
			attempted++
		}
		if ok {
			log.Printf("✅ %-12s %s success", j.name, j.desc)
			continue
		}
		status := "无成功记录"
		if !last.IsZero() {
			status = fmt.Sprintf("失败 (最后尝试 %s)", last.Format("15:04:05"))
		}
		log.Printf("❌ %-12s %s: %s", j.name, j.desc, status)
		missing = append(missing, fmt.Sprintf("- %s (%s): %s", j.desc, j.name, status))
	}
	return missing, attempted, nil
}

// isTradingDay 查交易日历; 日历缺行时保守按周一至周五处理,
// 避免"日历没更新"导致看门狗也静默失效 (这正是它要防的失效模式)
func isTradingDay(db *sqlx.DB, date string) (bool, error) {
	var isOpen int
	q := `SELECT is_open FROM trade_cal WHERE cal_date = ?`
	err := db.QueryRow(q, date).Scan(&isOpen)
	if err == nil {
		return isOpen == 1, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	t, perr := time.Parse("20060102", date)
	if perr != nil {
		return false, fmt.Errorf("日期格式无效: %s", date)
	}
	wd := t.Weekday()
	return wd >= time.Monday && wd <= time.Friday, nil
}

// sendAlert 强制走邮件通道: 不受 cfg.Mail.Enabled 开关影响,
// 否则调度器事故场景可能同时关掉了唯一的对外告警口
func sendAlert(cfg *config.Config, title, body string) error {
	mailer := notify.NewMailNotifier(true, cfg.Mail.From, cfg.Mail.Password)
	if !mailer.Enabled() {
		return errors.New("邮件未配置 (需 mail.from 非空且环境变量 JZ_MAIL_PASSWORD 已注入)")
	}
	return mailer.Send(title, body)
}

func joinLines(lines []string) string {
	s := ""
	for _, l := range lines {
		s += l + "\n"
	}
	return s
}
