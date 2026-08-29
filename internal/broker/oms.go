package broker

import (
	"fmt"
	"sync"
	"time"

	"jingzhe-trader/internal/model"
)

// OrderRecord OMS内部订单记录
// 状态统一使用 model.OrderStatus (与落库订单共用同一状态机, 避免双份定义漂移)
type OrderRecord struct {
	OrderID   string
	Req       OrderRequest
	State     model.OrderStatus
	FilledQty int
	AvgPrice  float64
	Trades    []model.Trade
	CreateAt  time.Time
	UpdateAt  time.Time
	RejectMsg string
}

// OMS 订单管理系统
type OMS struct {
	mu       sync.RWMutex
	orders   map[string]*OrderRecord
	orderSeq int64
}

// NewOMS 创建订单管理系统
func NewOMS() *OMS {
	return &OMS{
		orders: make(map[string]*OrderRecord),
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
		State:    model.StatusCreated,
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
		rec.State = model.StatusSubmitted
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
		rec.State = model.StatusFilled
	} else {
		rec.State = model.StatusPartial
	}
	rec.UpdateAt = time.Now()
}

// RejectOrder 订单被拒绝
func (o *OMS) RejectOrder(orderID string, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if rec, ok := o.orders[orderID]; ok {
		rec.State = model.StatusRejected
		rec.RejectMsg = reason
		rec.UpdateAt = time.Now()
	}
}

// CancelOrder 撤单
func (o *OMS) CancelOrder(orderID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if rec, ok := o.orders[orderID]; ok {
		if rec.State == model.StatusCreated || rec.State == model.StatusSubmitted {
			rec.State = model.StatusCanceled
			rec.UpdateAt = time.Now()
			return true
		}
	}
	return false
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
