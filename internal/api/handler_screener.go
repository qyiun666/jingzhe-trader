package api

import (
	"net/http"

	"jingzhe-trader/internal/store"
)

// HandleScreenResults GET /api/screener/results?date=
// 获取选股结果 (不传 date 返回最新一次)
func (s *Service) HandleScreenResults(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	var results []store.ScreenResult
	var err error
	if date != "" {
		results, err = s.screenRepo.GetByDate(date)
	} else {
		results, err = s.screenRepo.GetLatest()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询选股结果失败: "+err.Error())
		return
	}
	if results == nil {
		results = []store.ScreenResult{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count":   len(results),
		"results": results,
	})
}

// HandleScreenRun POST /api/screener/run
// 手动触发选股 (测试用, 正常由调度器自动执行)
func (s *Service) HandleScreenRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if s.screener == nil {
		writeError(w, http.StatusServiceUnavailable, "选股器未初始化")
		return
	}
	// 使用最新行情日期
	date, err := s.barRepo.GetMaxTradeDate()
	if err != nil || date == "" {
		writeError(w, http.StatusServiceUnavailable, "无可用行情数据")
		return
	}
	results, err := s.screener.Screen(date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "选股失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"date":    date,
		"count":   len(results),
		"results": results,
	})
}
