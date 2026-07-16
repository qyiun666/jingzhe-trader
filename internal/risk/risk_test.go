package risk

import (
	"testing"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
)

// ====== 仓位限制测试 ======

// TestCalcMaxBuyQty_SingleLimit 测试单票仓位限制
func TestCalcMaxBuyQty_SingleLimit(t *testing.T) {
	pl := NewPositionLimiter(0.1, 1.0, 1.0) // 单票10%，总仓100%，板块100%
	totalAsset := 1000000.0
	price := 10.0
	tsCode := "000001.SZ"

	positions := make(map[string]*model.Position)
	stocks := make(map[string]*model.Stock)

	maxQty := pl.CalcMaxBuyQty(tsCode, positions, totalAsset, stocks, price)

	// 单票最大仓位: 1000000 * 0.1 = 100000 元
	// 最大可买数量: 100000 / 10 = 10000 股，手数取整后为 10000
	expected := 10000
	if maxQty != expected {
		t.Errorf("单票仓位限制计算错误: 期望 %d, 实际 %d", expected, maxQty)
	}
}

// TestCalcMaxBuyQty_TotalLimit 测试总仓位限制
func TestCalcMaxBuyQty_TotalLimit(t *testing.T) {
	pl := NewPositionLimiter(0.5, 0.3, 1.0) // 单票50%，总仓30%，板块100%
	totalAsset := 1000000.0
	price := 10.0

	positions := make(map[string]*model.Position)
	stocks := make(map[string]*model.Stock)

	// 已有持仓 10000 股，市值 100000
	positions["000002.SZ"] = &model.Position{
		TsCode:      "000002.SZ",
		TotalQty:    10000,
		MarketPrice: 10.0,
		MarketValue: 100000.0,
		CostPrice:   10.0,
	}

	maxQty := pl.CalcMaxBuyQty("000001.SZ", positions, totalAsset, stocks, price)

	// 总仓位上限: 1000000 * 0.3 = 300000 元
	// 已有市值: 100000 元
	// 剩余可买: 200000 元 / 10 = 20000 股
	expected := 20000
	if maxQty != expected {
		t.Errorf("总仓位限制计算错误: 期望 %d, 实际 %d", expected, maxQty)
	}
}

// TestCalcMaxBuyQty_SectorLimit 测试板块仓位限制
func TestCalcMaxBuyQty_SectorLimit(t *testing.T) {
	pl := NewPositionLimiter(0.5, 1.0, 0.2) // 单票50%，总仓100%，板块20%
	totalAsset := 1000000.0
	price := 10.0

	positions := make(map[string]*model.Position)
	stocks := make(map[string]*model.Stock)

	// 已有深市主板持仓 000002.SZ
	positions["000002.SZ"] = &model.Position{
		TsCode:      "000002.SZ",
		TotalQty:    10000,
		MarketPrice: 10.0,
		MarketValue: 100000.0,
		CostPrice:   10.0,
	}

	// 000001.SZ 也是深市主板
	stocks["000001.SZ"] = &model.Stock{
		TsCode: "000001.SZ",
		Name:   "平安银行",
	}

	maxQty := pl.CalcMaxBuyQty("000001.SZ", positions, totalAsset, stocks, price)

	// 板块上限: 1000000 * 0.2 = 200000 元
	// 已有板块市值: 100000 元
	// 剩余可买: 100000 元 / 10 = 10000 股
	expected := 10000
	if maxQty != expected {
		t.Errorf("板块仓位限制计算错误: 期望 %d, 实际 %d", expected, maxQty)
	}
}

// TestCalcMaxBuyQty_WithExistingPosition 测试已有同票持仓时的最大可买
func TestCalcMaxBuyQty_WithExistingPosition(t *testing.T) {
	pl := NewPositionLimiter(0.1, 1.0, 1.0) // 单票10%
	totalAsset := 1000000.0
	price := 10.0
	tsCode := "000001.SZ"

	positions := make(map[string]*model.Position)
	stocks := make(map[string]*model.Stock)

	// 已有 5000 股持仓
	positions[tsCode] = &model.Position{
		TsCode:      tsCode,
		TotalQty:    5000,
		MarketPrice: 10.0,
		MarketValue: 50000.0,
		CostPrice:   10.0,
	}

	maxQty := pl.CalcMaxBuyQty(tsCode, positions, totalAsset, stocks, price)

	// 单票上限: 1000000 * 0.1 = 100000 元
	// 已有市值: 50000 元
	// 还可买: 50000 / 10 = 5000 股
	expected := 5000
	if maxQty != expected {
		t.Errorf("已有持仓时计算错误: 期望 %d, 实际 %d", expected, maxQty)
	}
}

// TestCheckPosition_BuyWithinLimit 测试买入信号在限制内
func TestCheckPosition_BuyWithinLimit(t *testing.T) {
	pl := NewPositionLimiter(0.1, 1.0, 1.0)
	totalAsset := 1000000.0
	price := 10.0

	positions := make(map[string]*model.Position)
	stocks := make(map[string]*model.Stock)

	signal := model.Signal{
		TsCode:    "000001.SZ",
		Direction: model.DirBuy,
		TargetQty: 1000,
	}

	result, err := pl.CheckPosition(signal, positions, totalAsset, stocks, price)
	if err != nil {
		t.Errorf("正常买入不应报错: %v", err)
	}
	if result.TargetQty != 1000 {
		t.Errorf("买入数量不应变化: 期望 1000, 实际 %d", result.TargetQty)
	}
}

// TestCheckPosition_BuyOverLimit 测试买入信号超过限制（应调整）
func TestCheckPosition_BuyOverLimit(t *testing.T) {
	pl := NewPositionLimiter(0.01, 1.0, 1.0) // 单票仅1%
	totalAsset := 1000000.0
	price := 10.0

	positions := make(map[string]*model.Position)
	stocks := make(map[string]*model.Stock)

	signal := model.Signal{
		TsCode:    "000001.SZ",
		Direction: model.DirBuy,
		TargetQty: 5000, // 超过限制
	}

	result, err := pl.CheckPosition(signal, positions, totalAsset, stocks, price)

	// 单票上限: 1000000 * 0.01 = 10000 元 / 10 = 1000 股
	expectedQty := 1000
	if result.TargetQty != expectedQty {
		t.Errorf("超额买入应调整: 期望 %d, 实际 %d", expectedQty, result.TargetQty)
	}
	if err == nil {
		t.Error("超额买入应返回错误信息")
	}
}

// TestCheckPosition_SellNoLimit 测试卖出信号不受仓位限制
func TestCheckPosition_SellNoLimit(t *testing.T) {
	pl := NewPositionLimiter(0.1, 1.0, 1.0)
	totalAsset := 1000000.0
	price := 10.0

	positions := make(map[string]*model.Position)
	stocks := make(map[string]*model.Stock)

	signal := model.Signal{
		TsCode:    "000001.SZ",
		Direction: model.DirSell,
		TargetQty: 100000, // 大量卖出
	}

	result, err := pl.CheckPosition(signal, positions, totalAsset, stocks, price)
	if err != nil {
		t.Errorf("卖出信号不应受仓位限制: %v", err)
	}
	if result.TargetQty != 100000 {
		t.Errorf("卖出数量不应变化")
	}
}

// ====== 止损止盈测试 ======

// TestCheckSingle_StopLoss 测试止损触发
func TestCheckSingle_StopLoss(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15) // 止损5%，止盈15%

	pos := &model.Position{
		TsCode:    "000001.SZ",
		TotalQty:  1000,
		CostPrice: 10.0,
	}

	// 下跌 6%，触发止损
	currentPrice := 9.4
	triggered, reason := sl.CheckSingle(pos, currentPrice)
	if !triggered {
		t.Error("下跌6%应触发止损")
	}
	if reason == "" {
		t.Error("应返回止损原因")
	}
}

// TestCheckSingle_NoStopLoss 测试未触发止损
func TestCheckSingle_NoStopLoss(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15)

	pos := &model.Position{
		TsCode:    "000001.SZ",
		TotalQty:  1000,
		CostPrice: 10.0,
	}

	// 下跌 3%，未触发止损
	currentPrice := 9.7
	triggered, reason := sl.CheckSingle(pos, currentPrice)
	if triggered {
		t.Error("下跌3%不应触发止损")
	}
	if reason != "" {
		t.Error("不应返回原因")
	}
}

// TestCheckSingle_TakeProfit 测试止盈触发
func TestCheckSingle_TakeProfit(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15) // 止盈15%

	pos := &model.Position{
		TsCode:    "000001.SZ",
		TotalQty:  1000,
		CostPrice: 10.0,
	}

	// 上涨 20%，触发止盈
	currentPrice := 12.0
	triggered, reason := sl.CheckSingle(pos, currentPrice)
	if !triggered {
		t.Error("上涨20%应触发止盈")
	}
	if reason == "" {
		t.Error("应返回止盈原因")
	}
}

// TestCheckSingle_NoTakeProfit 测试未触发止盈
func TestCheckSingle_NoTakeProfit(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15)

	pos := &model.Position{
		TsCode:    "000001.SZ",
		TotalQty:  1000,
		CostPrice: 10.0,
	}

	// 上涨 10%，未触发止盈
	currentPrice := 11.0
	triggered, _ := sl.CheckSingle(pos, currentPrice)
	if triggered {
		t.Error("上涨10%不应触发止盈(止盈线15%)")
	}
}

// TestCheckSingle_EdgeStopLoss 测试止损边界
func TestCheckSingle_EdgeStopLoss(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15)

	pos := &model.Position{
		TsCode:    "000001.SZ",
		TotalQty:  1000,
		CostPrice: 10.0,
	}

	// 刚好下跌 5%，应触发止损
	currentPrice := 9.5
	triggered, _ := sl.CheckSingle(pos, currentPrice)
	if !triggered {
		t.Error("刚好下跌5%应触发止损")
	}
}

// TestCheckStopLoss_MultiplePositions 测试多持仓止损检查
func TestCheckStopLoss_MultiplePositions(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15)

	positions := make(map[string]*model.Position)
	bars := make(map[string]*model.Bar)

	// 股票A: 亏损 10%，应触发止损
	positions["000001.SZ"] = &model.Position{
		TsCode:       "000001.SZ",
		TotalQty:     1000,
		AvailableQty: 1000,
		CostPrice:    10.0,
	}
	bars["000001.SZ"] = &model.Bar{
		TsCode: "000001.SZ",
		Close:  9.0, // 亏损10%
	}

	// 股票B: 盈利 5%，不触发
	positions["000002.SZ"] = &model.Position{
		TsCode:       "000002.SZ",
		TotalQty:     2000,
		AvailableQty: 2000,
		CostPrice:    10.0,
	}
	bars["000002.SZ"] = &model.Bar{
		TsCode: "000002.SZ",
		Close:  10.5, // 盈利5%
	}

	// 股票C: 盈利 20%，应触发止盈
	positions["600001.SH"] = &model.Position{
		TsCode:       "600001.SH",
		TotalQty:     500,
		AvailableQty: 500,
		CostPrice:    10.0,
	}
	bars["600001.SH"] = &model.Bar{
		TsCode: "600001.SH",
		Close:  12.0, // 盈利20%
	}

	signals := sl.CheckStopLoss(positions, bars)

	// 应返回 2 个卖出信号（1个止损 + 1个止盈）
	if len(signals) != 2 {
		t.Errorf("应返回2个卖出信号，实际 %d 个", len(signals))
	}

	// 验证信号都是卖出方向
	for _, sig := range signals {
		if sig.Direction != model.DirSell {
			t.Error("止损止盈信号应为卖出方向")
		}
	}
}

// TestCheckSingle_ZeroCost 测试零成本持仓（不触发）
func TestCheckSingle_ZeroCost(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15)

	pos := &model.Position{
		TsCode:    "000001.SZ",
		TotalQty:  1000,
		CostPrice: 0.0, // 成本为0
	}

	triggered, _ := sl.CheckSingle(pos, 5.0)
	if triggered {
		t.Error("零成本持仓不应触发止损止盈")
	}
}

// TestCheckSingle_NoPosition 测试空持仓
func TestCheckSingle_NoPosition(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.15)

	pos := &model.Position{
		TsCode:   "000001.SZ",
		TotalQty: 0, // 空仓
	}

	triggered, _ := sl.CheckSingle(pos, 10.0)
	if triggered {
		t.Error("空持仓不应触发止损止盈")
	}
}

// TestTrailingStop 测试移动止盈
func TestTrailingStop(t *testing.T) {
	sl := NewStopLossManager(0.05, 0.10) // 止盈10%
	sl.SetTrailingStop(0.05)              // 移动止盈回撤5%

	pos := &model.Position{
		TsCode:      "000001.SZ",
		TotalQty:    1000,
		CostPrice:   10.0,
		MarketPrice: 12.0, // 最高价 12 元（盈利20%）
	}

	// 从高点回撤 6%，应触发移动止盈
	currentPrice := 11.28 // 12 * 0.94 = 11.28，回撤约6%
	triggered, reason := sl.CheckSingle(pos, currentPrice)
	if !triggered {
		t.Error("回撤超过阈值应触发移动止盈")
	}
	if reason == "" {
		t.Error("应返回移动止盈原因")
	}
}

// ====== 黑名单测试 ======

// TestBlacklist_ST 测试 ST 股过滤
func TestBlacklist_ST(t *testing.T) {
	bl := NewBlacklist(true, 0)

	stock := &model.Stock{
		TsCode:     "000001.SZ",
		IsST:       true,
		ListStatus: "L",
	}

	if !bl.Check(stock, "20250101") {
		t.Error("ST股应被过滤")
	}
}

// TestBlacklist_NoST 测试非 ST 股不被过滤
func TestBlacklist_NoST(t *testing.T) {
	bl := NewBlacklist(true, 0)

	stock := &model.Stock{
		TsCode:     "000001.SZ",
		IsST:       false,
		ListStatus: "L",
		ListDate:   "20200101",
	}

	if bl.Check(stock, "20250101") {
		t.Error("非ST股不应被过滤")
	}
}

// TestBlacklist_MinListDays 测试最小上市天数
func TestBlacklist_MinListDays(t *testing.T) {
	bl := NewBlacklist(false, 30) // 最小上市30天

	stock := &model.Stock{
		TsCode:     "000001.SZ",
		IsST:       false,
		ListStatus: "L",
		ListDate:   "20250101",
	}

	// 上市仅 10 天，应被过滤
	if !bl.Check(stock, "20250111") {
		t.Error("上市天数不足应被过滤")
	}

	// 上市 40 天，不应被过滤
	if bl.Check(stock, "20250210") {
		t.Error("上市天数足够不应被过滤")
	}
}

// TestBlacklist_Custom 测试自定义黑名单
func TestBlacklist_Custom(t *testing.T) {
	bl := NewBlacklist(false, 0)

	stock := &model.Stock{
		TsCode:     "000001.SZ",
		IsST:       false,
		ListStatus: "L",
		ListDate:   "20200101",
	}

	// 添加到黑名单
	bl.Add("000001.SZ")
	if !bl.Check(stock, "20250101") {
		t.Error("自定义黑名单股票应被过滤")
	}

	// 从黑名单移除
	bl.Remove("000001.SZ")
	if bl.Check(stock, "20250101") {
		t.Error("移除后不应再被过滤")
	}
}

// ====== 风控管理器集成测试 ======

// TestRiskManager_BuyFlow 测试买入信号完整风控流程
func TestRiskManager_BuyFlow(t *testing.T) {
	cfg := config.RiskConfig{
		MaxPositionPct:      0.1,
		MaxTotalPositionPct: 0.8,
		MaxSectorPct:        0.3,
		StopLossPct:         0.05,
		TakeProfitPct:       0.15,
		ExcludeST:           true,
		MinListDays:         0,
	}

	rm := NewRiskManager(cfg)

	signals := []model.Signal{
		{
			TsCode:    "000001.SZ",
			Direction: model.DirBuy,
			TargetQty: 5000,
			Reason:    "测试买入",
		},
	}

	positions := make(map[string]*model.Position)
	stocks := map[string]*model.Stock{
		"000001.SZ": {
			TsCode:     "000001.SZ",
			Name:       "平安银行",
			IsST:       false,
			ListStatus: "L",
			ListDate:   "20200101",
		},
	}
	bars := map[string]*model.Bar{
		"000001.SZ": {
			TsCode:    "000001.SZ",
			Close:     10.0,
			PreClose:  10.0,
			TradeDate: "20250101",
		},
	}

	totalAsset := 1000000.0

	passed, rejected := rm.Check(signals, positions, totalAsset, stocks, "20250101", bars)

	if len(passed) != 1 {
		t.Errorf("正常买入信号应通过，通过数量: %d", len(passed))
	}
	if len(rejected) != 0 {
		t.Errorf("正常买入不应被拒绝，拒绝数量: %d", len(rejected))
		for _, r := range rejected {
			t.Logf("拒绝原因: %s - %s", r.Rule, r.Reason)
		}
	}
}

// TestRiskManager_STBuyRejected 测试 ST 股买入被拒绝
func TestRiskManager_STBuyRejected(t *testing.T) {
	cfg := config.RiskConfig{
		MaxPositionPct:      0.1,
		MaxTotalPositionPct: 0.8,
		MaxSectorPct:        0.3,
		StopLossPct:         0.05,
		TakeProfitPct:       0.15,
		ExcludeST:           true,
		MinListDays:         0,
	}

	rm := NewRiskManager(cfg)

	signals := []model.Signal{
		{
			TsCode:    "000001.SZ",
			Direction: model.DirBuy,
			TargetQty: 1000,
		},
	}

	positions := make(map[string]*model.Position)
	stocks := map[string]*model.Stock{
		"000001.SZ": {
			TsCode:     "000001.SZ",
			Name:       "ST平安",
			IsST:       true, // ST 股
			ListStatus: "L",
		},
	}
	bars := make(map[string]*model.Bar)

	passed, rejected := rm.Check(signals, positions, 1000000.0, stocks, "20250101", bars)

	if len(passed) != 0 {
		t.Error("ST股买入应被拒绝")
	}
	if len(rejected) != 1 {
		t.Errorf("应有1个拒绝原因，实际 %d", len(rejected))
	}
	if len(rejected) > 0 && rejected[0].Rule != "blacklist_st" {
		t.Errorf("拒绝原因应为 blacklist_st，实际 %s", rejected[0].Rule)
	}
}

// TestRiskManager_SellT1 测试卖出信号 T+1 检查
func TestRiskManagerManager_SellT1(t *testing.T) {
	cfg := config.RiskConfig{
		MaxPositionPct:      0.1,
		MaxTotalPositionPct: 0.8,
		MaxSectorPct:        0.3,
		StopLossPct:         0.05,
		TakeProfitPct:       0.15,
		ExcludeST:           false,
		MinListDays:         0,
	}

	rm := NewRiskManager(cfg)

	signals := []model.Signal{
		{
			TsCode:    "000001.SZ",
			Direction: model.DirSell,
			TargetQty: 1000,
		},
	}

	positions := map[string]*model.Position{
		"000001.SZ": {
			TsCode:       "000001.SZ",
			TotalQty:     1000,
			AvailableQty: 500, // 只有500股可卖
			TodayBought:  500, // 500股是今天买的
			CostPrice:    10.0,
			MarketPrice:  10.0,
		},
	}
	stocks := map[string]*model.Stock{
		"000001.SZ": {
			TsCode:     "000001.SZ",
			IsST:       false,
			ListStatus: "L",
		},
	}
	bars := map[string]*model.Bar{
		"000001.SZ": {
			TsCode:   "000001.SZ",
			Close:    10.0,
			PreClose: 10.0,
		},
	}

	passed, rejected := rm.Check(signals, positions, 1000000.0, stocks, "20250101", bars)

	// 卖出数量被调整为可卖数量，信号应通过
	if len(passed) != 1 {
		t.Errorf("T+1调整后应通过，通过数量: %d", len(passed))
		for _, r := range rejected {
			t.Logf("拒绝: %s - %s", r.Rule, r.Reason)
		}
	}
	if len(passed) > 0 && passed[0].TargetQty != 500 {
		t.Errorf("卖出数量应调整为500，实际 %d", passed[0].TargetQty)
	}
}

// ====== 敞口测试 ======

// TestSectorExposure 测试板块敞口计算
func TestSectorExposure(t *testing.T) {
	em := NewExposureManager(0.3)
	totalAsset := 1000000.0

	positions := map[string]*model.Position{
		"000001.SZ": { // 深市主板
			TsCode:      "000001.SZ",
			TotalQty:    1000,
			MarketPrice: 10.0,
			MarketValue: 10000.0,
		},
		"000002.SZ": { // 深市主板
			TsCode:      "000002.SZ",
			TotalQty:    2000,
			MarketPrice: 10.0,
			MarketValue: 20000.0,
		},
		"600001.SH": { // 沪市主板
			TsCode:      "600001.SH",
			TotalQty:    1000,
			MarketPrice: 10.0,
			MarketValue: 10000.0,
		},
	}

	stocks := make(map[string]*model.Stock)
	exposure := em.SectorExposure(positions, stocks, totalAsset)

	// 深市主板: (10000 + 20000) / 1000000 = 3%
	szPct := exposure["深市主板"]
	if szPct < 0.029 || szPct > 0.031 {
		t.Errorf("深市主板敞口计算错误: 期望约 3%%, 实际 %.2f%%", szPct*100)
	}

	// 沪市主板: 10000 / 1000000 = 1%
	shPct := exposure["沪市主板"]
	if shPct < 0.009 || shPct > 0.011 {
		t.Errorf("沪市主板敞口计算错误: 期望约 1%%, 实际 %.2f%%", shPct*100)
	}
}

// TestCheckSectorLimit 测试板块限制检查
func TestCheckSectorLimit(t *testing.T) {
	em := NewExposureManager(0.05) // 板块上限 5%
	totalAsset := 1000000.0
	price := 10.0

	positions := map[string]*model.Position{
		"000002.SZ": { // 深市主板，市值 40000
			TsCode:      "000002.SZ",
			TotalQty:    4000,
			MarketPrice: 10.0,
			MarketValue: 40000.0,
		},
	}

	stocks := map[string]*model.Stock{
		"000001.SZ": {TsCode: "000001.SZ"},
	}

	// 买入 2000 股 (20000 元)，深市主板总市值变为 60000，占比 6% > 5%，应拒绝
	signal := model.Signal{
		TsCode:    "000001.SZ",
		Direction: model.DirBuy,
	}

	err := em.CheckSectorLimit(signal, positions, stocks, totalAsset, price, 2000)
	if err == nil {
		t.Error("突破板块限制应报错")
	}

	// 买入 500 股 (5000 元)，深市主板总市值变为 45000，占比 4.5% < 5%，应通过
	err = em.CheckSectorLimit(signal, positions, stocks, totalAsset, price, 500)
	if err != nil {
		t.Errorf("未突破板块限制不应报错: %v", err)
	}
}
