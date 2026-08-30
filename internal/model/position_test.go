package model

import "testing"

// TestAccountSnapshotFillPnL 收敛后的唯一盈亏算法: 实盘/回测/展示三条路径共用,
// 行为必须与收敛前三处保持一致
func TestAccountSnapshotFillPnL(t *testing.T) {
	snap := AccountSnapshot{TotalAsset: 110000}
	prev := &AccountSnapshot{TotalAsset: 100000}
	snap.FillPnL(prev, 90000)
	if snap.PnL != 10000 || snap.PnLPct != 0.1 {
		t.Fatalf("当日盈亏错误: pnl=%.2f pct=%.4f", snap.PnL, snap.PnLPct)
	}
	if snap.TotalPnL != 20000 || snap.TotalPnLPct != 20000.0/90000.0 {
		t.Fatalf("累计盈亏错误: total=%.2f pct=%.6f", snap.TotalPnL, snap.TotalPnLPct)
	}

	// 无上一快照 (首日): 当日盈亏保持零值
	first := AccountSnapshot{TotalAsset: 100000}
	first.FillPnL(nil, 100000)
	if first.PnL != 0 || first.PnLPct != 0 {
		t.Fatalf("首日当日盈亏应为 0, 实际 %.2f", first.PnL)
	}
	if first.TotalPnL != 0 || first.TotalPnLPct != 0 {
		t.Fatalf("首日累计盈亏应为 0, 实际 %.2f", first.TotalPnL)
	}

	// 基准非法 (<=0): 不得污染累计字段, 也不得除零
	bad := AccountSnapshot{TotalAsset: 50000}
	bad.FillPnL(&AccountSnapshot{TotalAsset: 0}, 0)
	if bad.PnL != 0 || bad.TotalPnL != 0 || bad.PnLPct != 0 || bad.TotalPnLPct != 0 {
		t.Fatalf("零基准应跳过计算: %+v", bad)
	}
}
