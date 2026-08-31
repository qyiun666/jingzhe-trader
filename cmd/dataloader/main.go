package main

import (
	"flag"
	"fmt"
	"time"

	"jingzhe-trader/internal/appcfg"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/pkg/logger"
)

func main() {
	dbPath := flag.String("db", "", "数据库路径 (配置即存于此库, 默认取 "+appcfg.EnvDBPath+" 或 "+config.DefaultDBPath()+")")
	startDate := flag.String("start", "", "起始日期 YYYYMMDD (留空则从上次同步位置继续)")
	endDate := flag.String("end", time.Now().Format("20060102"), "结束日期 YYYYMMDD")

	// 可选数据同步开关
	syncNews := flag.Bool("news", false, "同步新闻快讯")
	syncMoneyFlow := flag.Bool("moneyflow", false, "同步个股资金流向")
	syncTopList := flag.Bool("toplist", false, "同步龙虎榜数据")
	syncFina := flag.Bool("fina", false, "同步财务指标数据(按报告期获取, 每季度采集一次)")
	adjOnly := flag.Bool("adj", false, "仅回填复权因子(首次升级后跑一次, 修复历史adj_factor为0的数据)")
	cleanup := flag.Bool("cleanup", false, "清理不在股票池和持仓中的股票数据(危险)")
	confirmCleanup := flag.Bool("confirm-cleanup", false, "确认执行清理(与 -cleanup 配合, 双重保护)")
	flag.Parse()

	// 先开库再读配置 (配置本体存在 config_kv 里)
	db, err := appcfg.Open(appcfg.ResolveDBPath(*dbPath))
	if err != nil {
		fmt.Printf("打开数据库失败: %v\n", err)
		return
	}
	defer db.Close()

	cfg, err := appcfg.Load(db)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		return
	}

	// 初始化日志
	logger.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.Output, cfg.Log.FilePath, cfg.Retention.LogDays)
	defer logger.Sync()

	loader := dataloader.New(cfg, db)

	// 复权因子回填模式: 回填后退出
	if *adjOnly {
		loader.BackfillAdjFactors()
		return
	}

	// 清理模式: 清理数据后退出 (需 enable_cleanup 配置 + --confirm-cleanup 双重确认)
	if *cleanup {
		if err := loader.Cleanup(*confirmCleanup); err != nil {
			logger.L().Errorf("清理失败: %v", err)
		}
		return
	}

	// 数据同步
	opts := dataloader.Options{
		StartDate:     *startDate,
		EndDate:       *endDate,
		SyncNews:      *syncNews,
		SyncMoneyFlow: *syncMoneyFlow,
		SyncTopList:   *syncTopList,
		SyncFina:      *syncFina,
	}
	report, err := loader.Run(opts)
	if err != nil {
		logger.L().Fatalf("数据同步失败: %v", err)
	}
	// 辅助数据源失败不阻断核心同步, 但必须让操作者与脚本看到
	if len(report.OptionalFailures) > 0 {
		for _, f := range report.OptionalFailures {
			logger.L().Errorf("辅助数据同步失败: %s", f)
		}
		logger.L().Fatalf("核心数据已同步, 但 %d 个辅助数据源失败 (见上方日志)", len(report.OptionalFailures))
	}
}
