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
