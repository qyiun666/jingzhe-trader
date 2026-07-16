package risk

import (
	"time"

	"jingzhe-trader/internal/model"
)

// Blacklist 黑名单过滤器
// 用于过滤不符合条件的股票，如 ST 股、上市时间不足的新股、自定义黑名单等
type Blacklist struct {
	excludeST   bool          // 是否排除 ST 股
	minListDays int           // 最小上市天数
	customCodes map[string]bool // 自定义黑名单
}

// NewBlacklist 创建黑名单过滤器
// excludeST: 是否排除 ST 股
// minListDays: 最小上市天数，上市不满该天数的新股将被过滤
func NewBlacklist(excludeST bool, minListDays int) *Blacklist {
	return &Blacklist{
		excludeST:   excludeST,
		minListDays: minListDays,
		customCodes: make(map[string]bool),
	}
}

// Add 添加股票到自定义黑名单
func (bl *Blacklist) Add(code string) {
	bl.customCodes[code] = true
}

// Remove 从自定义黑名单中移除股票
func (bl *Blacklist) Remove(code string) {
	delete(bl.customCodes, code)
}

// Check 检查股票是否在黑名单中
// 返回 true 表示该股票被禁止交易（在黑名单中）
// tradeDate: 当前交易日期，格式 YYYYMMDD
func (bl *Blacklist) Check(stock *model.Stock, tradeDate string) bool {
	if stock == nil {
		return true
	}

	// 检查自定义黑名单
	if bl.customCodes[stock.TsCode] {
		return true
	}

	// 检查是否排除 ST 股
	if bl.excludeST && stock.IsST {
		return true
	}

	// 检查上市状态：非上市状态的股票禁止交易
	if stock.ListStatus != "L" {
		return true
	}

	// 检查最小上市天数
	if bl.minListDays > 0 && stock.ListDate != "" {
		listDate, err := time.Parse("20060102", stock.ListDate)
		if err == nil {
			curDate, err := time.Parse("20060102", tradeDate)
			if err == nil {
				days := int(curDate.Sub(listDate).Hours() / 24)
				if days < bl.minListDays {
					return true
				}
			}
		}
	}

	return false
}

// FilterSignals 过滤信号中的黑名单股票
// 返回通过过滤的信号列表和被拒绝的原因列表
func (bl *Blacklist) FilterSignals(signals []model.Signal, stocks map[string]*model.Stock,
	tradeDate string) ([]model.Signal, []RejectReason) {
	var passed []model.Signal
	var rejected []RejectReason

	for _, sig := range signals {
		stock := stocks[sig.TsCode]
		if stock == nil {
			rejected = append(rejected, RejectReason{
				TsCode: sig.TsCode,
				Signal: sig,
				Reason: "股票信息不存在",
				Rule:   "blacklist_no_stock_info",
			})
			continue
		}

		if bl.customCodes[sig.TsCode] {
			rejected = append(rejected, RejectReason{
				TsCode: sig.TsCode,
				Signal: sig,
				Reason: "股票在自定义黑名单中",
				Rule:   "blacklist_custom",
			})
			continue
		}

		if bl.excludeST && stock.IsST {
			rejected = append(rejected, RejectReason{
				TsCode: sig.TsCode,
				Signal: sig,
				Reason: "ST 股被排除",
				Rule:   "blacklist_st",
			})
			continue
		}

		if stock.ListStatus != "L" {
			rejected = append(rejected, RejectReason{
				TsCode: sig.TsCode,
				Signal: sig,
				Reason: "股票未上市或已退市",
				Rule:   "blacklist_list_status",
			})
			continue
		}

		if bl.minListDays > 0 && stock.ListDate != "" {
			listDate, err1 := time.Parse("20060102", stock.ListDate)
			curDate, err2 := time.Parse("20060102", tradeDate)
			if err1 == nil && err2 == nil {
				days := int(curDate.Sub(listDate).Hours() / 24)
				if days < bl.minListDays {
					rejected = append(rejected, RejectReason{
						TsCode: sig.TsCode,
						Signal: sig,
						Reason: "上市天数不足",
						Rule:   "blacklist_min_list_days",
					})
					continue
				}
			}
		}

		passed = append(passed, sig)
	}

	return passed, rejected
}
