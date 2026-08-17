package broker

import (
	"jingzhe-trader/internal/model"
)

// OrderRequest 下单请求
type OrderRequest struct {
	TsCode   string
	Side     model.Side
	Qty      int
	Price    float64 // 限价单价格, 0表示市价单
	Reason   string  // 下单原因
	Strategy string  // 来源策略名
	FillDate string  // 成交日 (Paper撮合: 晚于当前日时登记为待成交, 到成交日才入账)
	PreClose float64 // 成交日K线昨收价 (无涨跌停数据时自算兜底用, 0=不用)
	IsST     bool    // 是否ST股 (自算涨跌停兜底用)
}

// LimitProvider 涨跌停价提供者接口 (由 store.LimitRepo 实现)
type LimitProvider interface {
	GetByCodeAndDate(tsCode, tradeDate string) (*model.StkLimit, error)
}

// Broker 统一券商接口
// 回测、纸面交易、实盘共用此接口
type Broker interface {
	// Name 券商名称
	Name() string
	// PlaceOrder 下单, 返回订单ID
	PlaceOrder(req OrderRequest) (string, error)
	// CancelOrder 撤单
	CancelOrder(orderID string) error
	// QueryPositions 查询持仓
	QueryPositions() (map[string]*model.Position, error)
	// QueryAsset 查询账户资产
	QueryAsset() (*AssetInfo, error)
	// OnTrade 注册成交回调
	OnTrade(callback func(model.Trade))
	// SettleT1 T+1结算 (每日开盘前调用, date 为当日交易日 YYYYMMDD)
	SettleT1(date string)
	// UpdateMarketValue 按最新行情更新持仓市值
	UpdateMarketValue(bars map[string]*model.Bar)
}

// AssetInfo 账户资产信息
type AssetInfo struct {
	Cash        float64
	TotalAsset  float64
	MarketValue float64
	Positions   map[string]*model.Position
}

// NoOpBroker 空Broker (用于测试)
type NoOpBroker struct{}

func (b *NoOpBroker) Name() string                                        { return "noop" }
func (b *NoOpBroker) PlaceOrder(_ OrderRequest) (string, error)           { return "", nil }
func (b *NoOpBroker) CancelOrder(_ string) error                          { return nil }
func (b *NoOpBroker) QueryPositions() (map[string]*model.Position, error) { return nil, nil }
func (b *NoOpBroker) QueryAsset() (*AssetInfo, error)                     { return nil, nil }
func (b *NoOpBroker) OnTrade(_ func(model.Trade))                         {}
func (b *NoOpBroker) SettleT1(_ string)                                   {}
func (b *NoOpBroker) UpdateMarketValue(_ map[string]*model.Bar)           {}
