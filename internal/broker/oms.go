package broker

import (
	"fmt"
	"sync"
	"time"

	"jingzhe-trader/internal/model"
)

// OrderState 订单状态机
type OrderState int

const (
	StatePending    OrderState = iota // 待处理
	StateSubmitted                     // 已提交
	StatePartial                       // 部分成交
	StateFilled                        // 全部成交
	StateCanceled                      // 已撤单
	StateRejected                      // 已拒绝
)

func (s OrderState) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateSubmitted:
		return "submitted"
	case StatePartial:
		return "partial"
	case StateFilled:
		return "filled"
	case StateCanceled:
		return "canceled"
	case StateRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// OrderRecord OMS内部订单记录
type OrderRecord struct {
	OrderID   string
	Req       OrderRequest
	State     OrderState
	FilledQty int
	AvgPrice  float64
	Trades    []model.Trade
	CreateAt  time.Time
	UpdateAt  time.Time
	RejectMsg string
}

// OMS 订单管理系统
type OMS struct {
	mu        sync.RWMutex
	orders    map[string]*OrderRecord
	orderSeq  int64
	callbacks []func(model.Trade)
}

// NewOMS 创建订单管理系统
func NewOMS() *OMS {
	return &OMS{
		orders:    make(map[string]*OrderRecord),
		callbacks: make([]func(model.Trade), 0),
	}
}

// CreateOrder 创建订单记录
func (o *OMS) CreateOrder(req OrderRequest) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.orderSeq++
	orderID := fmt.Sprintf("ORD_%d_%d", time.Now().Unix(), o.orderSeq)
	rec := &OrderRecord{
		OrderID:  orderID,
		Req:      req,
		State:    StatePending,
		CreateAt: time.Now(),
		UpdateAt: time.Now(),
	}
	o.orders[orderID] = rec
	return orderID
}

// SubmitOrder 提交订单
func (o *OMS) SubmitOrder(orderID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if rec, ok := o.orders[orderID]; ok {
		rec.State = StateSubmitted
		rec.UpdateAt = time.Now()
	}
}

// FillOrder 订单成交
func (o *OMS) FillOrder(orderID string, trade model.Trade) {
	o.mu.Lock()
	defer o.mu.Unlock()
	rec, ok := o.orders[orderID]
	if !ok {
		return
	}
	rec.Trades = append(rec.Trades, trade)
	rec.FilledQty += trade.Qty
	// 更新均价
	if len(rec.Trades) == 1 {
		rec.AvgPrice = trade.Price
	} else {
		totalQty := 0
		totalAmount := 0.0
		for _, t := range rec.Trades {
			totalQty += t.Qty
			totalAmount += t.Price * float64(t.Qty)
		}
		if totalQty > 0 {
			rec.AvgPrice = totalAmount / float64(totalQty)
		}
	}
	if rec.FilledQty >= rec.Req.Qty {
		rec.State = StateFilled
	} else {
		rec.State = StatePartial
	}
	rec.UpdateAt = time.Now()

	// 触发回调
	o.emitTrade(trade)
}

// RejectOrder 订单被拒绝
func (o *OMS) RejectOrder(orderID string, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if rec, ok := o.orders[orderID]; ok {
		rec.State = StateRejected
		rec.RejectMsg = reason
		rec.UpdateAt = time.Now()
	}
}

// CancelOrder 撤单
func (o *OMS) CancelOrder(orderID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if rec, ok := o.orders[orderID]; ok {
		if rec.State == StatePending || rec.State == StateSubmitted {
			rec.State = StateCanceled
			rec.UpdateAt = time.Now()
			return true
		}
	}
	return false
}

// GetOrder 获取订单
func (o *OMS) GetOrder(orderID string) *OrderRecord {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.orders[orderID]
}

// GetAllOrders 获取所有订单
func (o *OMS) GetAllOrders() []*OrderRecord {
	o.mu.RLock()
	defer o.mu.RUnlock()
	result := make([]*OrderRecord, 0, len(o.orders))
	for _, rec := range o.orders {
		result = append(result, rec)
	}
	return result
}

// RegisterCallback 注册成交回调
func (o *OMS) RegisterCallback(callback func(model.Trade)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.callbacks = append(o.callbacks, callback)
}

func (o *OMS) emitTrade(trade model.Trade) {
	for _, cb := range o.callbacks {
		go cb(trade) // 异步回调避免阻塞
	}
}

// Stats 统计
func (o *OMS) Stats() (total, filled, canceled, rejected int) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for _, rec := range o.orders {
		total++
		switch rec.State {
		case StateFilled:
			filled++
		case StateCanceled:
			canceled++
		case StateRejected:
			rejected++
		}
	}
	return
}
