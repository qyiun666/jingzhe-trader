package api

import (
	"fmt"
	"net/http"

	"jingzhe-trader/internal/store"
)

// HandleScreenResults GET /api/screener/results?date=
// 获取选股结果 (不传 date 返回最新一次)
func (s *Service) HandleScreenResults(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	writeJSON(w, http.StatusOK, s.BuildScreenerResults(date))
}

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

// HandleScreenRun POST /api/screener/run
// 手动触发选股 (测试用, 正常由调度器自动执行)
func (s *Service) HandleScreenRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	result, err := s.RunScreener()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
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
