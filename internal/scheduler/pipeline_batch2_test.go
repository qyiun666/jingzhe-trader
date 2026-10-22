package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// TestShouldSkipBuyReview 满仓短路判定：无新仓名额且候选全部已持有才跳过；
// 任一候选未持有、还有空位、候选为空都不跳过。
func TestShouldSkipBuyReview(t *testing.T) {
	held := []model.Position{
		{TsCode: "a", TotalQty: 100},
		{TsCode: "b", TotalQty: 100},
		{TsCode: "cleared", TotalQty: 0}, // 清仓行不占名额
	}
	allHeld := []model.Candidate{{TsCode: "a"}, {TsCode: "b"}}
	if !shouldSkipBuyReview(allHeld, held, 2) {
		t.Error("满仓且候选均已持有应跳过评审")
	}
	if shouldSkipBuyReview([]model.Candidate{{TsCode: "a"}, {TsCode: "c"}}, held, 2) {
		t.Error("有未持有候选（可能是换仓对象）不得跳过")
	}
	if shouldSkipBuyReview([]model.Candidate{{TsCode: "a"}}, held, 3) {
		t.Error("还有新仓名额时不得跳过")
	}
	if shouldSkipBuyReview(nil, held, 2) {
		t.Error("无候选时不需要触发满仓短路")
	}
	if shouldSkipBuyReview(allHeld, held, 0) {
		t.Error("maxPositions<=0 时不判定")
	}
}

// TestCandidateIndustryLine 日报候选行业分布：取快照前 20 只，按行业只数降序，
// ETF 单独归类；缺快照时明说"无快照"。
func TestCandidateIndustryLine(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/day.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if got := candidateIndustryLine(ctx, st, "20261023"); !strings.Contains(got, "无候选快照") {
		t.Errorf("缺快照应明说无快照，实际 %q", got)
	}

	snap := model.ScreenSnapshot{TradeDate: "20261023"}
	for i := 0; i < 10; i++ {
		snap.Candidates = append(snap.Candidates, model.CandidateSnapshot{TsCode: "e" + itoa(i), Industry: "电子"})
	}
	for i := 0; i < 5; i++ {
		snap.Candidates = append(snap.Candidates, model.CandidateSnapshot{TsCode: "b" + itoa(i), Industry: "银行"})
	}
	for i := 0; i < 5; i++ {
		snap.Candidates = append(snap.Candidates, model.CandidateSnapshot{
			TsCode: "588000.SH", Industry: "半导体", Kind: model.KindETF})
	}
	// 第 21 只不参与 Top20 统计。
	snap.Candidates = append(snap.Candidates, model.CandidateSnapshot{TsCode: "z", Industry: "医药"})
	raw, _ := json.Marshal(snap)
	if err := st.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: "20261023", Subject: model.TraceScreenTopN(), Outcome: model.TraceOK,
		Detail: string(raw),
	}); err != nil {
		t.Fatalf("写候选快照失败: %v", err)
	}

	got := candidateIndustryLine(ctx, st, "20261023")
	for _, want := range []string{"前 20 只", "电子10", "银行5", "ETF5"} {
		if !strings.Contains(got, want) {
			t.Errorf("行业分布缺 %q: %q", want, got)
		}
	}
	if strings.Contains(got, "医药") {
		t.Errorf("第 21 只不应计入 Top20 分布: %q", got)
	}
}
