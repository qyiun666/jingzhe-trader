package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// newTestNewsAnalyst 构造使用临时库的 NewsAnalyst (LLM 禁用, 验证不依赖 LLM 的分支)
func newTestNewsAnalyst(t *testing.T, newsList []model.News) *NewsAnalyst {
	t.Helper()
	db, err := store.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	repo := store.NewNewsRepo(db)
	if err := repo.BatchInsert(newsList); err != nil {
		t.Fatalf("插入新闻失败: %v", err)
	}
	return NewNewsAnalyst(llm.NewClient(llm.Config{}), repo)
}

// TestNewsAnalystNoRelevantNews 窗口内匹配不到该股新闻时按"该维度无输入"处理, 不计入情绪均值
func TestNewsAnalystNoRelevantNews(t *testing.T) {
	a := newTestNewsAnalyst(t, []model.News{
		{Datetime: "2026-01-02 10:00:00", Title: "央行发布新政", Content: "宏观流动性"},
		{Datetime: "2026-01-02 09:00:00", Title: "国际油价上涨", Content: "能源市场"},
	})
	report, err := a.Analyze(&DebateContext{TradeDate: "20260102", TsCode: "600519.SH", Name: "贵州茅台"})
	if err != nil {
		t.Fatalf("Analyze 失败: %v", err)
	}
	if !report.IsMissingData() {
		t.Fatalf("无该股新闻时应标记为无依据报告, 实际 %v", report.KeyPoints)
	}
	if !strings.Contains(report.KeyPoints[0], "新闻") {
		t.Errorf("应说明是新闻维度缺输入, 实际 %q", report.KeyPoints[0])
	}
}

// TestNewsAnalystRelevantNews 命中该股新闻时必须走到 LLM 调用 (未启用时以错误体现), 而不是判"无新闻"
func TestNewsAnalystRelevantNews(t *testing.T) {
	a := newTestNewsAnalyst(t, []model.News{
		{Datetime: "2026-01-02 10:00:00", Title: "贵州茅台发布年度业绩预告", Content: "业绩增长"},
		{Datetime: "2026-01-02 09:00:00", Title: "国际油价上涨", Content: "能源市场"},
	})
	_, err := a.Analyze(&DebateContext{TradeDate: "20260102", TsCode: "600519.SH", Name: "贵州茅台"})
	if err == nil {
		t.Fatal("LLM 未启用时不应静默产出报告")
	}
	if !strings.Contains(err.Error(), "LLM 未启用") {
		t.Fatalf("应先进入 LLM 调用再失败, 实际 %v", err)
	}
}

// TestNewsAnalystIgnoresStaleNews 决策日之前超过窗口的新闻不算相关: 一个月前的旧闻会给出方向错误的消息面结论
func TestNewsAnalystIgnoresStaleNews(t *testing.T) {
	a := newTestNewsAnalyst(t, []model.News{
		{Datetime: "2025-11-02 10:00:00", Title: "贵州茅台三年前的旧公告", Content: "历史"},
	})
	report, err := a.Analyze(&DebateContext{TradeDate: "20260102", TsCode: "600519.SH", Name: "贵州茅台"})
	if err != nil {
		t.Fatalf("Analyze 失败: %v", err)
	}
	if !report.IsMissingData() {
		t.Fatalf("窗口外新闻不应算作相关输入, 实际 %v", report.KeyPoints)
	}
}
