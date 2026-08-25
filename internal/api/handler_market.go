package api

import (
	"fmt"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/model"
)

// RunMarket 市场概况
func (s *Service) RunMarket(date string) (*MarketSnapshotJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}
	if len(allBars) == 0 {
		return nil, fmt.Errorf("当日 %s 无行情数据", date)
	}

	prevBars := s.getPrevBars(date)
	return s.buildMarketSnapshot(date, allBars, prevBars), nil
}

// buildMarketSnapshot 构建市场快照 JSON
func (s *Service) buildMarketSnapshot(date string, allBars []model.Bar, prevBars map[string]*model.Bar) *MarketSnapshotJSON {
	snapshot := analysis.MonitorMarket(date, allBars, prevBars)

	result := &MarketSnapshotJSON{
		UpCount:        snapshot.UpCount,
		DownCount:      snapshot.DownCount,
		LimitUpCount:   snapshot.UpLimitCount,
		LimitDownCount: snapshot.DownLimitCount,
		VolumeRatio:    snapshot.VolumeRatio,
		HotSectors:     make([]map[string]interface{}, 0, len(snapshot.HotSectors)),
		Alarms:         make([]map[string]string, 0, len(snapshot.Alarms)),
	}

	for _, hs := range snapshot.HotSectors {
		result.HotSectors = append(result.HotSectors, map[string]interface{}{
			"sector":        hs.Sector,
			"avg_change":    hs.AvgChange,
			"leader_stock":  hs.LeaderStock,
			"leader_change": hs.LeaderChange,
		})
	}

	for _, a := range snapshot.Alarms {
		result.Alarms = append(result.Alarms, map[string]string{
			"level":   a.Level,
			"type":    a.Type,
			"ts_code": a.TsCode,
			"message": a.Message,
		})
	}

	return result
}
