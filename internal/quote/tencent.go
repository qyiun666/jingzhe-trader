package quote

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Source 实时行情源接口
// 仅用于盘中止损监控与计划参考价, 不驱动策略
type Source interface {
	// GetRealtimePrices 批量获取最新价, 返回 ts_code -> price
	GetRealtimePrices(codes []string) (map[string]float64, error)
}

// TencentQuote 腾讯免费行情源 (qt.gtimg.cn)
// 无需密钥, 适合小资金零成本方案; 仅解析最新价字段
type TencentQuote struct {
	client  *http.Client
	baseURL string
}

// NewTencentQuote 创建腾讯行情源
func NewTencentQuote() *TencentQuote {
	return &TencentQuote{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		baseURL: "https://qt.gtimg.cn",
	}
}

// GetRealtimePrices 批量获取最新价
func (t *TencentQuote) GetRealtimePrices(codes []string) (map[string]float64, error) {
	if len(codes) == 0 {
		return map[string]float64{}, nil
	}

	// ts_code 600519.SH -> sh600519
	symbols := make([]string, 0, len(codes))
	symbolToCode := make(map[string]string, len(codes))
	for _, code := range codes {
		sym := toTencentSymbol(code)
		if sym == "" {
			continue
		}
		symbols = append(symbols, sym)
		symbolToCode[sym] = code
	}
	if len(symbols) == 0 {
		return map[string]float64{}, nil
	}

	url := fmt.Sprintf("%s/q=%s", t.baseURL, strings.Join(symbols, ","))
	resp, err := t.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("请求腾讯行情失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("腾讯行情响应异常: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取腾讯行情响应失败: %w", err)
	}

	return parseTencentBody(string(body), symbolToCode), nil
}

// parseTencentBody 解析响应体
// 格式: v_sh600519="1~贵州茅台~600519~1405.00~..."; 最新价在第4个字段(下标3)
func parseTencentBody(body string, symbolToCode map[string]string) map[string]float64 {
	prices := make(map[string]float64)
	for _, line := range strings.Split(body, ";") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "v_") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		sym := strings.TrimPrefix(line[:eq], "v_")
		code, ok := symbolToCode[sym]
		if !ok {
			continue
		}
		content := strings.Trim(line[eq+1:], `"`)
		fields := strings.Split(content, "~")
		if len(fields) < 4 {
			continue
		}
		if price, err := strconv.ParseFloat(fields[3], 64); err == nil && price > 0 {
			prices[code] = price
		}
	}
	return prices
}

// toTencentSymbol ts_code 转腾讯代码: 600519.SH -> sh600519, 000001.SZ -> sz000001
func toTencentSymbol(tsCode string) string {
	parts := strings.Split(tsCode, ".")
	if len(parts) != 2 {
		return ""
	}
	switch strings.ToUpper(parts[1]) {
	case "SH":
		return "sh" + parts[0]
	case "SZ":
		return "sz" + parts[0]
	case "BJ":
		return "bj" + parts[0]
	default:
		return ""
	}
}
