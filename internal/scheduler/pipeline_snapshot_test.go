package scheduler

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
)

// TestWriteScreenSnapshot 候选快照落 run_trace：score/factors/行业/资金口径必须完整，
// 重跑覆盖同一行而不是追加；空候选也要写一行 0 条，归因才能区分"没选出来"与"没留痕"。
func TestWriteScreenSnapshot(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/snap.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	d := Deps{Store: st}
	rp := risk.DefaultParams(model.FromFloat(10000)) // 单票上限 40% = 4000 元

	cands := []model.Candidate{{
		TsCode: "600000.SH", Name: "甲", Industry: "银行", Rank: 1, PoolSize: 20,
		Score: 88.5, Factors: model.FactorScore{Momentum: 90, Value: 80, LowVol: 70, Liquidity: 60},
		Close: 1234, Mom: 0.08, SectorMom: 0.05,
	}}
	if err := writeScreenSnapshot(ctx, d, "20261009", cands, rp); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}
	snaps, err := st.TraceRepo().ListScreenTopN(ctx, "20261009", "20261009")
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	snap, ok := snaps["20261009"]
	if !ok {
		t.Fatal("缺少当日快照")
	}
	if snap.TotalAssetFen != int64(model.FromFloat(10000)) || snap.SlotFen != int64(model.FromFloat(4000)) {
		t.Errorf("资金口径=%d/%d，期望 10000/4000 元", snap.TotalAssetFen, snap.SlotFen)
	}
	if len(snap.Candidates) != 1 {
		t.Fatalf("候选数=%d，期望 1", len(snap.Candidates))
	}
	c := snap.Candidates[0]
	if c.TsCode != "600000.SH" || c.Score != 88.5 || c.Industry != "银行" ||
		c.Momentum != 90 || c.Liquidity != 60 || c.CloseFen != 1234 {
		t.Errorf("候选快照字段有误: %+v", c)
	}

	// 重跑覆盖：同一天仍只有一行；空候选写出 0 条而不是残留上一轮。
	if err := writeScreenSnapshot(ctx, d, "20261009", nil, rp); err != nil {
		t.Fatalf("写空快照失败: %v", err)
	}
	snaps, err = st.TraceRepo().ListScreenTopN(ctx, "20261009", "20261009")
	if err != nil {
		t.Fatalf("重读快照失败: %v", err)
	}
	if len(snaps) != 1 || len(snaps["20261009"].Candidates) != 0 {
		t.Errorf("重跑应覆盖为空候选，实际 %+v", snaps)
	}
}
