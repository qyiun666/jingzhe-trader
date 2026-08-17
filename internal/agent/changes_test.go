package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"jingzhe-trader/internal/store"
)

// setupChangeTest 准备含历史辩论记录的临时库
func setupChangeTest(t *testing.T) (*DebateOrchestrator, *store.DebateRepo) {
	t.Helper()
	db, err := store.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	repo := store.NewDebateRepo(db)
	return NewDebateOrchestrator(nil, nil, nil, nil, nil, repo), repo
}

// TestDetectDecisionChangesNewSymbol 新增标的应单独标记为 new_symbol, 与决策变更区分
func TestDetectDecisionChangesNewSymbol(t *testing.T) {
	o, repo := setupChangeTest(t)
	// 历史记录: 贵州茅台 hold, 平安银行 buy
	if _, err := repo.Insert(&store.DebateResult{
		TradeDate: "20251231", TsCode: "600519.SH", Name: "贵州茅台",
		Decision: "hold", Confidence: 0.5, RiskLevel: "medium",
	}); err != nil {
		t.Fatalf("插入历史辩论失败: %v", err)
	}
	if _, err := repo.Insert(&store.DebateResult{
		TradeDate: "20251231", TsCode: "000001.SZ", Name: "平安银行",
		Decision: "buy", Confidence: 0.7, RiskLevel: "low",
	}); err != nil {
		t.Fatalf("插入历史辩论失败: %v", err)
	}

	today := []store.DebateResult{
		// 决策真正变化: hold → buy
		{TradeDate: "20260102", TsCode: "600519.SH", Name: "贵州茅台", Decision: "buy", Confidence: 0.8, RiskLevel: "medium"},
		// 无显著变化
		{TradeDate: "20260102", TsCode: "000001.SZ", Name: "平安银行", Decision: "buy", Confidence: 0.75, RiskLevel: "low"},
		// 新增标的
		{TradeDate: "20260102", TsCode: "300750.SZ", Name: "宁德时代", Decision: "buy", Confidence: 0.6},
	}

	changes := o.DetectDecisionChanges(today)
	if len(changes) != 2 {
		t.Fatalf("期望 2 条变更(1 决策变更 + 1 新增标的), 实际 %d: %+v", len(changes), changes)
	}
	byType := map[string]DecisionChange{}
	for _, c := range changes {
		byType[c.Type] = c
	}
	decision, ok := byType[ChangeTypeDecision]
	if !ok {
		t.Fatalf("缺少 decision 类型变更: %+v", changes)
	}
	if decision.TsCode != "600519.SH" || !strings.Contains(decision.Detail, "决策变更") {
		t.Errorf("decision 变更内容异常: %+v", decision)
	}
	newSym, ok := byType[ChangeTypeNewSymbol]
	if !ok {
		t.Fatalf("缺少 new_symbol 类型变更: %+v", changes)
	}
	if newSym.TsCode != "300750.SZ" || !strings.Contains(newSym.Detail, "新增标的") {
		t.Errorf("new_symbol 变更内容异常: %+v", newSym)
	}
}

// TestFormatChangesForNotify 通知文案应分组展示新增标的与决策变更
func TestFormatChangesForNotify(t *testing.T) {
	if got := FormatChangesForNotify(nil); got != "无决策变更" {
		t.Errorf("空列表应返回'无决策变更', 实际 %q", got)
	}
	changes := []DecisionChange{
		{Type: ChangeTypeNewSymbol, Name: "宁德时代", Detail: "新增标的: 宁德时代 → 买入 (置信度 60%)"},
		{Type: ChangeTypeDecision, Name: "贵州茅台", Detail: "决策变更: 持有 → 买入"},
	}
	text := FormatChangesForNotify(changes)
	if !strings.Contains(text, "【新增标的】") {
		t.Errorf("通知应包含新增标的分组, 实际:\n%s", text)
	}
	if !strings.Contains(text, "【决策变更】") {
		t.Errorf("通知应包含决策变更分组, 实际:\n%s", text)
	}
	// 决策变更分组应排在新增标的之前
	if strings.Index(text, "【决策变更】") > strings.Index(text, "【新增标的】") {
		t.Errorf("决策变更应排在新增标的前面, 实际:\n%s", text)
	}
}

// TestFormatChangesForNotifyOnlyNewSymbol 只有新增标的时不应出现决策变更分组
func TestFormatChangesForNotifyOnlyNewSymbol(t *testing.T) {
	text := FormatChangesForNotify([]DecisionChange{
		{Type: ChangeTypeNewSymbol, Name: "宁德时代", Detail: "新增标的"},
	})
	if strings.Contains(text, "【决策变更】") {
		t.Errorf("无决策变更时不应出现该分组, 实际:\n%s", text)
	}
	if !strings.Contains(text, "【新增标的】") {
		t.Errorf("应包含新增标的分组, 实际:\n%s", text)
	}
}
