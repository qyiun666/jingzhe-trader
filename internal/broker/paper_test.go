package broker

import (
	"testing"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// 测试用费率: 5元最低佣金档 (小资金敏感场景)
func testCostModel() *market.CostModel {
	return market.NewCostModel(config.CostConfig{
		CommissionRate:  0.00025,
		MinCommission:   5.0,
		StampTaxRate:    0.0005,
		TransferFeeRate: 0.00001,
	})
}

// TestExecuteBuy_FeeAwareSizing 买入 sizing 含费边界:
// 现金恰好只够股票本金时, 应自动降档到含手续费也买得起的数量
func TestExecuteBuy_FeeAwareSizing(t *testing.T) {
	pb := NewPaperBroker("test", 10000, testCostModel())

	// 10000 现金买 10 元 x 1000 股: 本金正好 10000, 但佣金 5 元不够
	_, err := pb.PlaceOrder(OrderRequest{
		TsCode: "000001.SZ", Side: model.SideBuy, Qty: 1000, Price: 10.0,
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	positions, _ := pb.QueryPositions()
	pos := positions["000001.SZ"]
	if pos == nil {
		t.Fatal("买入后无持仓")
	}
	// 期望降档到 900 股 (900*10 + 5佣金 + 过户费 <= 10000)
	if pos.TotalQty != 900 {
		t.Errorf("期望降档买入 900 股, 实际 %d 股", pos.TotalQty)
	}

	asset, _ := pb.QueryAsset()
	if asset.Cash < 0 {
		t.Errorf("买入后现金为负: %.2f (含费检查失效)", asset.Cash)
	}
}

// TestExecuteBuy_CashJustShortOfFee 现金恰好不够一手的手续费时应拒单而非透支
func TestExecuteBuy_CashJustShortOfFee(t *testing.T) {
	// 1003 元买 10 元 x 100 股: 本金 1000 + 佣金 5 = 1005.01 > 1003
	pb := NewPaperBroker("test", 1003, testCostModel())

	_, err := pb.PlaceOrder(OrderRequest{
		TsCode: "000001.SZ", Side: model.SideBuy, Qty: 100, Price: 10.0,
	})
	if err == nil {
		t.Fatal("现金不足含费成本时应拒单")
	}

	asset, _ := pb.QueryAsset()
	if asset.Cash != 1003 {
		t.Errorf("拒单后现金应不变, 实际 %.2f", asset.Cash)
	}
}

// TestQueryAsset_DeepCopy QueryAsset 返回的持仓必须是深拷贝
func TestQueryAsset_DeepCopy(t *testing.T) {
	pb := NewPaperBroker("test", 100000, testCostModel())
	if _, err := pb.PlaceOrder(OrderRequest{
		TsCode: "600519.SH", Side: model.SideBuy, Qty: 100, Price: 100.0,
	}); err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	asset, _ := pb.QueryAsset()
	asset.Positions["600519.SH"].TotalQty = 999999 // 篡改外部副本

	fresh, _ := pb.QueryAsset()
	if fresh.Positions["600519.SH"].TotalQty == 999999 {
		t.Error("QueryAsset 未深拷贝持仓, 外部修改污染了内部状态")
	}
}

// TestPendingFill_T1Settlement 次日成交模型:
// T日下单(FillDate=T+1) → 下单日不入账 → T+1结算时入账且当日不可卖 → T+2起可卖
func TestPendingFill_T1Settlement(t *testing.T) {
	pb := NewPaperBroker("test", 10000, testCostModel())
	pb.SetTradeDate("20240102", "20240103")

	// T日下单, FillDate=T+1: 应立即返回成功但不入账
	if _, err := pb.PlaceOrder(OrderRequest{
		TsCode: "000001.SZ", Side: model.SideBuy, Qty: 900, Price: 10.0, FillDate: "20240103",
	}); err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	asset, _ := pb.QueryAsset()
	if len(asset.Positions) != 0 {
		t.Fatal("T日下单不应立即入账持仓")
	}
	if asset.Cash != 10000 {
		t.Fatalf("T日现金不应变动, 实际 %.2f", asset.Cash)
	}

	// T+1 开盘结算: 成交入账, 当日买入不可卖 (TodayBought)
	pb.SettleT1("20240103")
	asset, _ = pb.QueryAsset()
	pos := asset.Positions["000001.SZ"]
	if pos == nil || pos.TotalQty != 900 {
		t.Fatalf("T+1结算后应有900股持仓, 实际 %+v", pos)
	}
	if pos.AvailableQty != 0 {
		t.Fatalf("T+1当日买入不可卖, 可卖应为0, 实际 %d", pos.AvailableQty)
	}
	if asset.Cash >= 10000 {
		t.Fatalf("T+1成交后现金应已扣减, 实际 %.2f", asset.Cash)
	}

	// T+1 当日卖出应被 T+1 规则拒绝
	pb.SetTradeDate("20240103", "20240104")
	if _, err := pb.PlaceOrder(OrderRequest{
		TsCode: "000001.SZ", Side: model.SideSell, Qty: 900, Price: 10.5, FillDate: "20240103",
	}); err == nil {
		t.Fatal("T+1当日卖出当日买入的持仓应被拒绝")
	}

	// T+2 结算后可卖
	pb.SettleT1("20240104")
	pb.SetTradeDate("20240104", "20240107")
	if _, err := pb.PlaceOrder(OrderRequest{
		TsCode: "000001.SZ", Side: model.SideSell, Qty: 900, Price: 10.5, FillDate: "20240104",
	}); err != nil {
		t.Fatalf("T+2卖出应成功: %v", err)
	}
	asset, _ = pb.QueryAsset()
	if len(asset.Positions) != 0 {
		t.Fatal("卖出后应无持仓")
	}
}

// TestPendingFill_InsufficientCashAtFill 待成交单入账日资金不足时应拒单
func TestPendingFill_InsufficientCashAtFill(t *testing.T) {
	pb := NewPaperBroker("test", 5000, testCostModel())
	pb.SetTradeDate("20240102", "20240103")
	if _, err := pb.PlaceOrder(OrderRequest{
		TsCode: "000001.SZ", Side: model.SideBuy, Qty: 400, Price: 10.0, FillDate: "20240103",
	}); err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	// 下单后(成交前)现金被其他方式占用: 模拟当天另一笔即日买入
	if _, err := pb.PlaceOrder(OrderRequest{
		TsCode: "600519.SH", Side: model.SideBuy, Qty: 300, Price: 10.0, FillDate: "20240102",
	}); err != nil {
		t.Fatalf("即日买入失败: %v", err)
	}
	// T+1 结算: 现金只剩约 2000, 买 400 股(约4005元)不够 → 降档或拒单, 不得透支
	pb.SettleT1("20240103")
	asset, _ := pb.QueryAsset()
	if asset.Cash < 0 {
		t.Fatalf("待成交单入账后现金不得为负, 实际 %.2f", asset.Cash)
	}
	pos := asset.Positions["000001.SZ"]
	if pos != nil && pos.TotalQty > 200 {
		t.Fatalf("资金不足时应降档到可承受数量, 实际 %d 股", pos.TotalQty)
	}
}
