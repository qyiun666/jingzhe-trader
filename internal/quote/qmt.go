package quote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// QMTQuote 通过 QMT sidecar 获取实时行情 (broker.type=qmt 时可选)
type QMTQuote struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewQMTQuote 创建 QMT 行情源
func NewQMTQuote(baseURL string) *QMTQuote {
	return &QMTQuote{
		baseURL: baseURL,
		token:   os.Getenv("QMT_SIDECAR_TOKEN"),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// qmtQuoteResp sidecar /quote 响应结构
type qmtQuoteResp struct {
	Success bool               `json:"success"`
	Error   string             `json:"error"`
	Prices  map[string]float64 `json:"prices"`
}

// GetRealtimePrices 批量获取最新价
func (q *QMTQuote) GetRealtimePrices(codes []string) (map[string]float64, error) {
	payload, err := json.Marshal(map[string]interface{}{"codes": codes})
	if err != nil {
		return nil, fmt.Errorf("序列化行情请求失败: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, q.baseURL+"/quote", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构建行情请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if q.token != "" {
		req.Header.Set("X-QMT-Token", q.token)
	}

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求QMT行情失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取QMT行情响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("QMT行情响应异常: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var qr qmtQuoteResp
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("解析QMT行情响应失败: %w", err)
	}
	if !qr.Success {
		return nil, fmt.Errorf("QMT行情查询失败: %s", qr.Error)
	}
	return qr.Prices, nil
}

// 编译期接口检查
var (
	_ Source = (*TencentQuote)(nil)
	_ Source = (*QMTQuote)(nil)
)
