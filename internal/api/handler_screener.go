package api

import (
	"fmt"

	"jingzhe-trader/internal/store"
)

// BuildScreenerResults returns the latest or historical stock screening results.
func (s *Service) BuildScreenerResults(date string) map[string]interface{} {
	var results []store.ScreenResult
	var err error
	var resultDate string
	if date != "" {
		results, err = s.screenRepo.GetByDate(date)
		resultDate = date
	} else {
		results, err = s.screenRepo.GetLatest()
		if len(results) > 0 {
			resultDate = results[0].TradeDate
		}
	}
	if results == nil {
		results = []store.ScreenResult{}
	}
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	return map[string]interface{}{
		"date":    resultDate,
		"count":   len(results),
		"results": results,
	}
}

// RunScreener manually triggers the full-market stock screener.
func (s *Service) RunScreener() (map[string]interface{}, error) {
	if s.screener == nil {
		return nil, fmt.Errorf("选股器未初始化")
	}
	// 使用最新行情日期
	date, err := s.barRepo.GetMaxTradeDate()
	if err != nil || date == "" {
		return nil, fmt.Errorf("无可用行情数据")
	}
	results, err := s.screener.Screen(date)
	if err != nil {
		return nil, fmt.Errorf("选股失败: %w", err)
	}
	return map[string]interface{}{
		"date":    date,
		"count":   len(results),
		"results": results,
	}, nil
}
