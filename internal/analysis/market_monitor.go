package analysis

import (
	"fmt"
	"sort"
	"strings"

	"jingzhe-trader/internal/model"
)

// MarketSnapshot 市场快照
type MarketSnapshot struct {
	TradeDate      string
	IndexBars      map[string]*model.Bar // 指数行情
	UpLimitCount   int                   // 涨停数
	DownLimitCount int                   // 跌停数
	UpCount        int                   // 上涨家数
	DownCount      int                   // 下跌家数
	VolumeRatio    float64               // 量比 (今日成交额/昨日成交额)
	TopGainers     []model.Bar           // 涨幅最大
	TopLosers      []model.Bar           // 跌幅最大
	HotSectors     []SectorHeat          // 热点板块
	Alarms         []MarketAlarm         // 告警列表
}

// SectorHeat 板块热度
type SectorHeat struct {
	Sector       string
	AvgChange    float64
	LeaderStock  string
	LeaderChange float64
}

// MarketAlarm 市场告警
type MarketAlarm struct {
	Level   string // warning/danger/info
	Type    string // limit_up/limit_down/large_drop/volume_spike/sector_crash
	TsCode  string
	Message string
}

// MoneyFlow 资金流向 (占位结构, 与 model 包保持一致)
type MoneyFlow struct {
	TsCode    string
	TradeDate string
	Inflow    float64 // 净流入
}

// TopList 龙虎榜 (占位结构)
type TopList struct {
	TsCode    string
	TradeDate string
	Reason    string
}

// 主要大盘指数代码
var indexCodes = map[string]bool{
	"000001.SH": true, // 上证指数
	"399001.SZ": true, // 深证成指
	"399006.SZ": true, // 创业板指
	"000300.SH": true, // 沪深300
	"000905.SH": true, // 中证500
	"000688.SH": true, // 科创50
}

// MonitorMarket 监控市场状态
func MonitorMarket(
	tradeDate string,
	allBars []model.Bar,
	prevBars map[string]*model.Bar,
	moneyflows []MoneyFlow,
	toplists []TopList,
) *MarketSnapshot {
	snapshot := &MarketSnapshot{
		TradeDate: tradeDate,
		IndexBars: make(map[string]*model.Bar),
		Alarms:    []MarketAlarm{},
	}

	// 分离指数和普通股票
	var stocks []model.Bar
	var todayAmount, yesterdayAmount float64

	for i := range allBars {
		bar := &allBars[i]
		if indexCodes[bar.TsCode] {
			snapshot.IndexBars[bar.TsCode] = bar
			continue
		}
		stocks = append(stocks, *bar)
		todayAmount += bar.Amount

		if prev, ok := prevBars[bar.TsCode]; ok {
			yesterdayAmount += prev.Amount
		}
	}

	// 1. 涨跌统计
	for i := range stocks {
		bar := stocks[i]
		if bar.PctChg > 9.5 {
			snapshot.UpLimitCount++
		} else if bar.PctChg < -9.5 {
			snapshot.DownLimitCount++
		}
		if bar.PctChg > 0 {
			snapshot.UpCount++
		} else if bar.PctChg < 0 {
			snapshot.DownCount++
		}
	}

	// 2. 量比监控
	if yesterdayAmount > 0 {
		snapshot.VolumeRatio = todayAmount / yesterdayAmount
		if snapshot.VolumeRatio > 1.5 {
			snapshot.Alarms = append(snapshot.Alarms, MarketAlarm{
				Level:   "warning",
				Type:    "volume_spike",
				TsCode:  "",
				Message: fmt.Sprintf("全市场放量, 量比 %.2f", snapshot.VolumeRatio),
			})
		}
	}

	// 3. 提取涨跌幅排行榜
	sortedByPct := make([]model.Bar, len(stocks))
	copy(sortedByPct, stocks)
	sort.Slice(sortedByPct, func(i, j int) bool {
		return sortedByPct[i].PctChg > sortedByPct[j].PctChg
	})
	if len(sortedByPct) > 10 {
		snapshot.TopGainers = sortedByPct[:10]
	} else {
		snapshot.TopGainers = sortedByPct
	}
	if len(sortedByPct) > 10 {
		snapshot.TopLosers = make([]model.Bar, 10)
		for i := 0; i < 10; i++ {
			snapshot.TopLosers[i] = sortedByPct[len(sortedByPct)-1-i]
		}
	} else {
		snapshot.TopLosers = make([]model.Bar, len(sortedByPct))
		for i := range sortedByPct {
			snapshot.TopLosers[i] = sortedByPct[len(sortedByPct)-1-i]
		}
	}

	// 4. 热点板块 (按代码前缀分组模拟板块)
	sectorMap := groupBySector(stocks)
	var sectors []SectorHeat
	for sector, bars := range sectorMap {
		if len(bars) < 3 {
			continue
		}
		var avgChange float64
		leader := bars[0]
		for _, b := range bars {
			avgChange += b.PctChg
			if b.PctChg > leader.PctChg {
				leader = b
			}
		}
		avgChange /= float64(len(bars))
		sectors = append(sectors, SectorHeat{
			Sector:       sector,
			AvgChange:    avgChange,
			LeaderStock:  leader.TsCode,
			LeaderChange: leader.PctChg,
		})
	}
	sort.Slice(sectors, func(i, j int) bool {
		return sectors[i].AvgChange > sectors[j].AvgChange
	})
	if len(sectors) > 3 {
		snapshot.HotSectors = sectors[:3]
	} else {
		snapshot.HotSectors = sectors
	}

	// 5. 大盘跌幅告警
	for code, bar := range snapshot.IndexBars {
		if bar.PctChg < -2.0 {
			snapshot.Alarms = append(snapshot.Alarms, MarketAlarm{
				Level:   "danger",
				Type:    "large_drop",
				TsCode:  code,
				Message: fmt.Sprintf("%s 大跌 %.2f%%, 建议减仓", code, bar.PctChg),
			})
		}
	}

	return snapshot
}

// MonitorPortfolio 监控持仓告警
// holdings: 当前持仓 map[tsCode]position
// todayBars: 当日行情
// targetBuys: 目标买入列表 (策略信号产生的买入候选)
func (s *MarketSnapshot) MonitorPortfolio(
	holdings map[string]*model.Position,
	todayBars map[string]*model.Bar,
	targetBuys []string,
) {
	for tsCode, _ := range holdings {
		bar, ok := todayBars[tsCode]
		if !ok {
			continue
		}

		// 持仓股跌停 -> danger
		if bar.PctChg < -9.5 {
			s.Alarms = append(s.Alarms, MarketAlarm{
				Level:   "danger",
				Type:    "limit_down",
				TsCode:  tsCode,
				Message: fmt.Sprintf("持仓股 %s 跌停 (%.2f%%), 建议风控处理", tsCode, bar.PctChg),
			})
			continue
		}

		// 持仓股放量下跌（量比>2且跌>5%）-> warning
		prevBar, hasPrev := prevBarsForCheck[tsCode]
		volumeRatio := 0.0
		if hasPrev && prevBar.Vol > 0 {
			volumeRatio = bar.Vol / prevBar.Vol
		}
		if volumeRatio > 2.0 && bar.PctChg < -5.0 {
			s.Alarms = append(s.Alarms, MarketAlarm{
				Level:   "warning",
				Type:    "large_drop",
				TsCode:  tsCode,
				Message: fmt.Sprintf("持仓股 %s 放量下跌 %.2f%% (量比%.2f), 建议关注", tsCode, bar.PctChg, volumeRatio),
			})
		}
	}

	// 目标买入股涨停 -> info
	targetSet := make(map[string]bool)
	for _, tsCode := range targetBuys {
		targetSet[tsCode] = true
	}
	for tsCode := range targetSet {
		bar, ok := todayBars[tsCode]
		if !ok {
			continue
		}
		if bar.PctChg > 9.5 {
			s.Alarms = append(s.Alarms, MarketAlarm{
				Level:   "info",
				Type:    "limit_up",
				TsCode:  tsCode,
				Message: fmt.Sprintf("目标股 %s 涨停, 可能买不到", tsCode),
			})
		}
	}
}

// prevBarsForCheck 用于 MonitorPortfolio 计算量比的临时存储
var prevBarsForCheck map[string]*model.Bar

// SetPrevBars 设置前一日行情 (供 MonitorPortfolio 使用)
func SetPrevBars(prev map[string]*model.Bar) {
	prevBarsForCheck = prev
}

// groupBySector 按行业/板块分组 (简单按代码前缀或市场分组)
func groupBySector(bars []model.Bar) map[string][]model.Bar {
	result := make(map[string][]model.Bar)
	for i := range bars {
		bar := bars[i]
		sector := inferSector(bar.TsCode)
		result[sector] = append(result[sector], bar)
	}
	return result
}

// inferSector 根据代码推断所属板块类别 (简化版)
func inferSector(tsCode string) string {
	if len(tsCode) < 6 {
		return "其他"
	}
	symbol := tsCode[:6]
	switch {
	case strings.HasPrefix(symbol, "60"):
		return "沪市主板"
	case strings.HasPrefix(symbol, "00"):
		return "深市主板"
	case strings.HasPrefix(symbol, "30"):
		return "创业板"
	case strings.HasPrefix(symbol, "68"):
		return "科创板"
	case strings.HasPrefix(symbol, "8") || strings.HasPrefix(symbol, "4"):
		return "北交所"
	default:
		return "其他"
	}
}

// AlarmCountByLevel 按级别统计告警数量
func (s *MarketSnapshot) AlarmCountByLevel(level string) int {
	count := 0
	for _, a := range s.Alarms {
		if a.Level == level {
			count++
		}
	}
	return count
}

// HasDangerAlarm 是否存在危险级别告警
func (s *MarketSnapshot) HasDangerAlarm() bool {
	for _, a := range s.Alarms {
		if a.Level == "danger" {
			return true
		}
	}
	return false
}
