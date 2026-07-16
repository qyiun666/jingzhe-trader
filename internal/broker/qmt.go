package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// qmtPosition sidecar 返回的持仓数据结构
type qmtPosition struct {
	StockCode   string  `json:"stock_code"`
	Volume      int     `json:"volume"`
	AvgPrice    float64 `json:"avg_price"`
	OpenPrice   float64 `json:"open_price"`
	MarketValue float64 `json:"market_value"`
}

// qmtAsset sidecar 返回的资产数据结构
type qmtAsset struct {
	Cash        float64 `json:"cash"`
	TotalAsset  float64 `json:"total_asset"`
	MarketValue float64 `json:"market_value"`
}

// qmtOrderResp sidecar 返回的下单响应
type qmtOrderResp struct {
	Success bool   `json:"success"`
	OrderID string `json:"order_id"`
	Error   string `json:"error"`
}

// qmtPositionsResp 查询持仓的响应结构
type qmtPositionsResp struct {
	Success   bool          `json:"success"`
	Error     string        `json:"error"`
	Positions []qmtPosition `json:"positions"`
}

// qmtAssetResp 查询资产的响应结构
type qmtAssetResp struct {
	Success     bool    `json:"success"`
	Error       string  `json:"error"`
	Cash        float64 `json:"cash"`
	TotalAsset  float64 `json:"total_asset"`
	MarketValue float64 `json:"market_value"`
}

// qmtConnectReq 连接请求结构
type qmtConnectReq struct {
	Path      string `json:"path"`
	SessionID int    `json:"session_id"`
	AccountID string `json:"account_id"`
}

// QMTBridge 通过 HTTP 调用 Python sidecar 连接 miniQMT 的桥接器
type QMTBridge struct {
	baseURL   string       // sidecar 地址，如 "http://127.0.0.1:16888"
	client    *http.Client // HTTP 客户端
	mu        sync.RWMutex // 保护 callbacks
	callbacks []func(model.Trade)
}

// NewQMTBridge 创建 QMTBridge 实例
func NewQMTBridge(baseURL string) *QMTBridge {
	return &QMTBridge{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		callbacks: make([]func(model.Trade), 0),
	}
}

// Name 返回券商名称
func (q *QMTBridge) Name() string {
	return "qmt"
}

// PlaceOrder 下单，返回订单ID
func (q *QMTBridge) PlaceOrder(req OrderRequest) (string, error) {
	orderType := "buy"
	if req.Side == model.SideSell {
		orderType = "sell"
	}

	payload := map[string]interface{}{
		"stock_code":    req.TsCode,
		"order_type":    orderType,
		"volume":        req.Qty,
		"price_type":    "fix",
		"price":         req.Price,
		"strategy_name": req.Strategy,
		"remark":        req.Reason,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("序列化下单请求失败: %w", err)
	}

	respBody, err := q.post("/order", body)
	if err != nil {
		return "", fmt.Errorf("下单请求失败: %w", err)
	}

	var resp qmtOrderResp
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("解析下单响应失败: %w", err)
	}

	if !resp.Success {
		return "", fmt.Errorf("下单被拒绝: %s", resp.Error)
	}

	logger.L().Infof("[QMTBridge] 下单成功 %s %s %d股 @%.2f, order_id=%s",
		req.TsCode, orderType, req.Qty, req.Price, resp.OrderID)
	return resp.OrderID, nil
}

// CancelOrder 撤单
func (q *QMTBridge) CancelOrder(orderID string) error {
	payload := map[string]string{
		"order_id": orderID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化撤单请求失败: %w", err)
	}

	respBody, err := q.post("/cancel", body)
	if err != nil {
		return fmt.Errorf("撤单请求失败: %w", err)
	}

	var resp qmtOrderResp
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("解析撤单响应失败: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("撤单失败: %s", resp.Error)
	}

	logger.L().Infof("[QMTBridge] 撤单成功 order_id=%s", orderID)
	return nil
}

// QueryPositions 查询持仓
func (q *QMTBridge) QueryPositions() (map[string]*model.Position, error) {
	respBody, err := q.get("/positions")
	if err != nil {
		return nil, fmt.Errorf("查询持仓请求失败: %w", err)
	}

	var resp qmtPositionsResp
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("解析持仓响应失败: %w", err)
	}

	if !resp.Success {
		return nil, fmt.Errorf("查询持仓失败: %s", resp.Error)
	}

	positions := make(map[string]*model.Position, len(resp.Positions))
	for _, p := range resp.Positions {
		pos := &model.Position{
			TsCode:       p.StockCode,
			TotalQty:     p.Volume,
			AvailableQty: p.Volume, // 真实券商中 T+1 由券商处理，可用量等于总量
			CostPrice:    p.AvgPrice,
			MarketValue:  p.MarketValue,
		}
		if p.Volume > 0 {
			pos.MarketPrice = p.MarketValue / float64(p.Volume)
		} else {
			pos.MarketPrice = p.OpenPrice
		}
		if pos.CostPrice > 0 {
			pos.FloatingPnL = pos.MarketValue - pos.CostPrice*float64(pos.TotalQty)
			pos.FloatingPnLPct = pos.FloatingPnL / (pos.CostPrice * float64(pos.TotalQty))
		}
		positions[p.StockCode] = pos
	}

	return positions, nil
}

// QueryAsset 查询账户资产
func (q *QMTBridge) QueryAsset() (*AssetInfo, error) {
	respBody, err := q.get("/asset")
	if err != nil {
		return nil, fmt.Errorf("查询资产请求失败: %w", err)
	}

	var resp qmtAssetResp
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("解析资产响应失败: %w", err)
	}

	if !resp.Success {
		return nil, fmt.Errorf("查询资产失败: %s", resp.Error)
	}

	// 同时查询持仓
	positions, err := q.QueryPositions()
	if err != nil {
		logger.L().Warnf("[QMTBridge] 查询资产时获取持仓失败: %v", err)
		positions = make(map[string]*model.Position)
	}

	return &AssetInfo{
		Cash:        resp.Cash,
		TotalAsset:  resp.TotalAsset,
		MarketValue: resp.MarketValue,
		Positions:   positions,
	}, nil
}

// OnTrade 注册成交回调
func (q *QMTBridge) OnTrade(callback func(model.Trade)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.callbacks = append(q.callbacks, callback)
}

// SettleT1 T+1结算，QMT自身处理，此处为空实现
func (q *QMTBridge) SettleT1() {
	// QMT 券商端自行处理 T+1，无需桥接层干预
}

// UpdateMarketValue 更新持仓市值，QMT查询真实市值，此处为空实现
func (q *QMTBridge) UpdateMarketValue(bars map[string]*model.Bar) {
	// QMT 通过 QueryPositions / QueryAsset 获取真实市值，无需用行情数据覆盖
}

// Health 检查 sidecar 健康状态
func (q *QMTBridge) Health() (bool, error) {
	respBody, err := q.get("/health")
	if err != nil {
		return false, err
	}

	// 尝试解析为通用响应
	var generic map[string]interface{}
	if err := json.Unmarshal(respBody, &generic); err == nil {
		if success, ok := generic["success"].(bool); ok {
			return success, nil
		}
		if status, ok := generic["status"].(string); ok {
			return status == "ok" || status == "healthy", nil
		}
	}

	// 只要能响应且状态码为 200，即认为健康
	return true, nil
}

// Connect 连接 miniQMT
func (q *QMTBridge) Connect(path string, sessionID int, accountID string) error {
	req := qmtConnectReq{
		Path:      path,
		SessionID: sessionID,
		AccountID: accountID,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("序列化连接请求失败: %w", err)
	}

	respBody, err := q.post("/connect", body)
	if err != nil {
		return fmt.Errorf("连接请求失败: %w", err)
	}

	var resp qmtOrderResp
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("解析连接响应失败: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("连接 QMT 失败: %s", resp.Error)
	}

	logger.L().Infof("[QMTBridge] 连接成功 path=%s session_id=%d account_id=%s",
		path, sessionID, accountID)
	return nil
}

// get 发送 GET 请求
func (q *QMTBridge) get(path string) ([]byte, error) {
	url := q.baseURL + path
	resp, err := q.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

// post 发送 POST 请求
func (q *QMTBridge) post(path string, body []byte) ([]byte, error) {
	url := q.baseURL + path
	resp, err := q.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

// 编译期接口检查
var _ Broker = (*QMTBridge)(nil)
