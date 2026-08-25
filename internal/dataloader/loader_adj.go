package dataloader

import (
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// mergeAdjFactors 拉取当日全市场复权因子并填入 bars
func (l *Loader) mergeAdjFactors(bars []model.Bar, calDate string) {
	factors, err := l.ts.AdjFactor("", calDate)
	if err != nil {
		logger.L().Warnf("获取 %s 复权因子失败(将依赖回填): %v", calDate, err)
		return
	}
	factorMap := make(map[string]float64, len(factors))
	for _, f := range factors {
		factorMap[f.TsCode] = f.AdjFactor
	}
	for i := range bars {
		if factor, ok := factorMap[bars[i].TsCode]; ok {
			bars[i].AdjFactor = factor
		}
	}
}

// BackfillAdjFactors 对外暴露的复权因子回填入口 (CLI -adj / 运维手动触发用)
func (l *Loader) BackfillAdjFactors() {
	l.backfillAdjFactors()
}

// backfillAdjFactors 回填历史缺失的复权因子
// daily 接口不返回因子, 历史数据的 adj_factor 为 0; 按股票逐只全量拉取回填
// 指数/ETF 不在 stock_basic 中, 自动跳过 (ETF 分红少, 暂不处理)
func (l *Loader) backfillAdjFactors() {
	codes, err := l.barRepo.GetZeroAdjFactorCodes()
	if err != nil {
		logger.L().Errorf("查询缺失复权因子失败: %v", err)
		return
	}
	if len(codes) == 0 {
		return
	}
	// 只回填真实股票 (在 stock_basic 中存在的代码)
	stockSet := make(map[string]bool)
	if rows, err := l.db.Queryx("SELECT ts_code FROM stock_basic"); err == nil {
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err == nil {
				stockSet[code] = true
			}
		}
		rows.Close()
	}
	logger.L().Infof("=== 回填复权因子 (%d 个代码待检查) ===", len(codes))
	totalUpdated := 0
	for _, code := range codes {
		if !stockSet[code] {
			continue
		}
		factors, err := l.ts.AdjFactor(code, "")
		if err != nil {
			logger.L().Warnf("获取 %s 复权因子失败: %v", code, err)
			continue
		}
		factorMap := make(map[string]float64, len(factors))
		for _, f := range factors {
			factorMap[f.TradeDate] = f.AdjFactor
		}
		updated, err := l.barRepo.UpdateAdjFactors(code, factorMap)
		if err != nil {
			logger.L().Errorf("回填 %s 复权因子失败: %v", code, err)
			continue
		}
		totalUpdated += updated
		logger.L().Infof("  %s: 回填 %d 条复权因子", code, updated)
	}
	logger.L().Infof("复权因子回填完成, 共更新 %d 条", totalUpdated)
}
