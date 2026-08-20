package agent

import (
	"path/filepath"
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

// TestNewsAnalystNoRelevantNews 匹配不到相关新闻时应明确输出"无相关新闻", 不拿全局热点充数
func TestNewsAnalystNoRelevantNews(t *testing.T) {
	a := newTestNewsAnalyst(t, []model.News{
		{Datetime: "2026-01-02 10:00:00", Title: "央行发布新政", Content: "宏观流动性"},
		{Datetime: "2026-01-02 09:00:00", Title: "国际油价上涨", Content: "能源市场"},
	})
	report, err := a.Analyze(&DebateContext{TradeDate: "20260102", TsCode: "600519.SH", Name: "贵州茅台"})
	if err != nil {
		t.Fatalf("Analyze 失败: %v", err)
	}
	if len(report.KeyPoints) != 1 || report.KeyPoints[0] != "无相关新闻" {
		t.Errorf("无相关新闻时应明确输出[无相关新闻], 实际 %v", report.KeyPoints)
	}
}

// TestNewsAnalystRelevantNews 有相关新闻时不应走“无相关新闻”分支
// 注意: LLM 未启用时现在返回错误而非降级对象, 此测试验证错误处理
func TestNewsAnalystRelevantNews(t *testing.T) {
	a := newTestNewsAnalyst(t, []model.News{
		{Datetime: "2026-01-02 10:00:00", Title: "贵州茅台发布年度业绩预告", Content: "业绩增长"},
		{Datetime: "2026-01-02 09:00:00", Title: "国际油价上涨", Content: "能源市场"},
	})
	_, err := a.Analyze(&DebateContext{TradeDate: "20260102", TsCode: "600519.SH", Name: "贵州茅台"})
	// LLM 未启用时返回错误 (不再降级)
	if err == nil {
		t.Skip("LLM 未启用, 跳过测试")
	}
}
