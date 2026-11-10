package scheduler

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
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
	if err := writeScreenSnapshot(ctx, d, "20261009", cands, rp, ""); err != nil {
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
	if err := writeScreenSnapshot(ctx, d, "20261009", nil, rp, ""); err != nil {
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

// legacyScreenSnapshot 是 sector_shadow_brief 出现**之前**的 screen:topn 载荷形状：
// 四个既有字段，声明序照抄 model.ScreenSnapshot（JSON 键序跟随结构体字段序）。
// 拿它做往返比对，就把"开关关闭 ⇒ detail 逐字节不变"钉在字节层面 —— 将来有人把新字段
// 插进结构体中间（键序漂移）或漏掉 omitempty（空串也写出键），这里立刻红。
type legacyScreenSnapshot struct {
	TradeDate     string                    `json:"trade_date"`
	TotalAssetFen int64                     `json:"total_asset_fen"`
	SlotFen       int64                     `json:"slot_fen"`
	Candidates    []model.CandidateSnapshot `json:"candidates"`
}

// topNDetail 读当日 screen:topn 行的原始 detail（ListScreenTopN 已经解析过，
// 字节级断言必须拿到未解析的原文）。
func topNDetail(t *testing.T, st *store.Store, date string) string {
	t.Helper()
	rows, err := st.TraceRepo().List(context.Background(), date)
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	for _, r := range rows {
		if r.Subject == model.TraceScreenTopN() {
			return r.Detail
		}
	}
	t.Fatalf("缺少 %s 轨迹行", model.TraceScreenTopN())
	return ""
}

// TestWriteScreenSnapshotSectorShadowField B2 输出侧接线的两条硬约束：
// 摘要为空串（= screen.sector_shadow=false，或关闸日）时载荷逐字节不变；
// 摘要非空时只**追加**一个独立键，candidates 与既有四键一字不改。
func TestWriteScreenSnapshotSectorShadowField(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/shadow_snap.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	d := Deps{Store: st}
	rp := risk.DefaultParams(model.FromFloat(10000))
	cands := []model.Candidate{{
		TsCode: "600000.SH", Name: "甲", Industry: "银行", Rank: 1, PoolSize: 20,
		Score: 88.5, Factors: model.FactorScore{Momentum: 90, Value: 80, LowVol: 70, Liquidity: 60},
		Close: 1234, Mom: 0.08, SectorMom: 0.05,
	}}
	const date = "20261009"

	// ① 关闭观测（brief 为空串）：新键不得出现，且整段 detail 与旧载荷形状往返一致。
	if err := writeScreenSnapshot(ctx, d, date, cands, rp, ""); err != nil {
		t.Fatalf("写关闭态快照失败: %v", err)
	}
	off := topNDetail(t, st, date)
	if strings.Contains(off, "sector_shadow") {
		t.Errorf("开关关闭时载荷不得写出影子字段，实际 detail=%s", off)
	}
	var legacy legacyScreenSnapshot
	if err := json.Unmarshal([]byte(off), &legacy); err != nil {
		t.Fatalf("关闭态载荷按旧形状解析失败: %v", err)
	}
	remarshaled, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatalf("旧形状重新序列化失败: %v", err)
	}
	if string(remarshaled) != off {
		t.Errorf("关闭态 detail 与旧载荷不逐字节相同（键序/键集漂移）:\n实际=%s\n旧形状=%s", off, remarshaled)
	}
	// 旧行在新结构上仍解析得通（影子字段回读为空，归因侧不会读到脏值）。
	var snapOff model.ScreenSnapshot
	if err := json.Unmarshal([]byte(off), &snapOff); err != nil {
		t.Fatalf("新结构解析关闭态载荷失败: %v", err)
	}
	if snapOff.SectorShadowBrief != "" {
		t.Errorf("关闭态影子字段应为空，实际=%q", snapOff.SectorShadowBrief)
	}

	// ② 开启观测：只追加 sector_shadow_brief 一个键，既有内容一字不改。
	brief := "板块影子 12 只（sector 级淘汰，未参与打分，漏斗输入序，列前 10）：600000.SH 测试0/银行 10.10元。"
	if err := writeScreenSnapshot(ctx, d, date, cands, rp, brief); err != nil {
		t.Fatalf("写开启态快照失败: %v", err)
	}
	on := topNDetail(t, st, date)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(on), &keys); err != nil {
		t.Fatalf("开启态载荷解析失败: %v", err)
	}
	if len(keys) != 5 {
		t.Errorf("开启态应只多一个键（共 5 个），实际 %d 个: %s", len(keys), on)
	}
	var got string
	if err := json.Unmarshal(keys["sector_shadow_brief"], &got); err != nil {
		t.Fatalf("解析 sector_shadow_brief 失败: %v", err)
	}
	if got != brief {
		t.Errorf("影子摘要写入失真:\n期望=%s\n实际=%s", brief, got)
	}
	var snapOn model.ScreenSnapshot
	if err := json.Unmarshal([]byte(on), &snapOn); err != nil {
		t.Fatalf("新结构解析开启态载荷失败: %v", err)
	}
	if !reflect.DeepEqual(snapOn.Candidates, snapOff.Candidates) {
		t.Errorf("开启观测改动了 candidates，违反「只追加」：\n开=%+v\n关=%+v", snapOn.Candidates, snapOff.Candidates)
	}
	if snapOn.TradeDate != snapOff.TradeDate || snapOn.TotalAssetFen != snapOff.TotalAssetFen ||
		snapOn.SlotFen != snapOff.SlotFen {
		t.Errorf("开启观测改动了既有资金/日期字段: %+v vs %+v", snapOn, snapOff)
	}
	// 键序：影子键必须排在最后（插在中间会让逐日 diff 从第二个键起全部错位）。
	if idx := strings.Index(on, `"sector_shadow_brief"`); idx < strings.Index(on, `"candidates"`) {
		t.Errorf("影子字段不应排在 candidates 之前: %s", on)
	}
}
