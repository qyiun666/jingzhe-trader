package api

import (
	"strings"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/model"
)

// buildNewsJSON 构建新闻摘要 JSON
// 优先展示与配置股票池相关的新闻, 不足时补充近期热点新闻
func (s *Service) buildNewsJSON() *NewsJSON {
	recentNews, err := s.newsRepo.GetRecent(50)
	if err != nil || len(recentNews) == 0 {
		return &NewsJSON{
			Sentiment:   "中性",
			RelatedNews: []map[string]string{},
		}
	}

	// 按股票池过滤: 优先展示与持仓/配置universe相关的新闻
	universeCodes := s.cfg.UniverseCodes()
	positions := s.getPositions()
	relatedKeywords := make(map[string]bool)
	for _, code := range universeCodes {
		relatedKeywords[code] = true
		// 提取6位代码 (如 600519 from 600519.SH)
		if len(code) >= 9 {
			relatedKeywords[code[:6]] = true
		}
	}
	for code := range positions {
		relatedKeywords[code] = true
		if len(code) >= 9 {
			relatedKeywords[code[:6]] = true
		}
		// 也加入股票名称
		if name := s.stockName(code); name != code {
			relatedKeywords[name] = true
		}
	}

	var filtered []model.News
	var others []model.News
	for _, n := range recentNews {
		text := n.Title + " " + n.Content
		matched := false
		for kw := range relatedKeywords {
			if strings.Contains(text, kw) {
				matched = true
				break
			}
		}
		if matched {
			filtered = append(filtered, n)
		} else {
			others = append(others, n)
		}
	}

	// 相关新闻不足20条时, 用近期热点补充
	if len(filtered) < 20 {
		need := 20 - len(filtered)
		if need > len(others) {
			need = len(others)
		}
		filtered = append(filtered, others[:need]...)
	}
	// 最多展示20条
	if len(filtered) > 20 {
		filtered = filtered[:20]
	}

	// 使用 analysis.NewsAnalyzer 分析情感
	na := analysis.NewNewsAnalyzer()
	relatedNews := make([]map[string]string, 0, len(filtered))
	var totalScore float64
	count := 0

	for _, n := range filtered {
		score := na.SentimentScore(n.Title + " " + n.Content)
		totalScore += score
		count++

		relatedNews = append(relatedNews, map[string]string{
			"title":     n.Title,
			"source":    "",
			"time":      n.Datetime,
			"sentiment": scoreToLabel(score),
		})
	}

	avgScore := 0.0
	if count > 0 {
		avgScore = totalScore / float64(count)
	}
	sentiment := scoreToLabel(avgScore)

	return &NewsJSON{
		Sentiment:   sentiment,
		RelatedNews: relatedNews,
	}
}
