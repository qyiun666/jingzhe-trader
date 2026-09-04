// Package tushare Tushare HTTP 适配层（L2）。
//
// 职责边界（ARCHITECTURE §1.2/§11.1）：仅此层触达外部网络；
// 所有调用统一经过令牌桶限流 + 错误分类 + 指数退避重试。
package tushare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// Client Tushare HTTP 适配客户端。
type Client struct {
	token       string
	baseURL     string
	httpClient  *http.Client
	limiter     *rate.Limiter // 令牌桶：控制每分钟调用次数（对齐 tushare.rate_per_min）
	maxRetries  int
	baseBackoff time.Duration
	rateWindow  time.Duration
}

// Option 可选配置项。
type Option func(*Client)

// NewClient 构造 Tushare 客户端。
//   - token: tushare.token（必填）
//   - baseURL: tushare.base_url（缺省 http://api.tushare.pro）
//   - ratePerMin: tushare.rate_per_min（令牌桶速率，缺省 200）
func NewClient(token, baseURL string, ratePerMin int, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = "http://api.tushare.pro"
	}
	if ratePerMin <= 0 {
		ratePerMin = 200
	}
	c := &Client{
		token:       token,
		baseURL:     baseURL,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		limiter:     rate.NewLimiter(rate.Limit(float64(ratePerMin)/60.0), ratePerMin),
		maxRetries:  5,
		baseBackoff: time.Second,
		rateWindow:  60 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type tushareRequest struct {
	API    string                 `json:"api_name"`
	Token  string                 `json:"token"`
	Params map[string]interface{} `json:"params"`
	Fields string                 `json:"fields,omitempty"`
}

type tushareResponse struct {
	Code int          `json:"code"`
	Msg  string       `json:"msg"`
	Data *tushareData `json:"data"`
}

type tushareData struct {
	Fields  []string        `json:"fields"`
	Items   [][]interface{} `json:"items"`
	HasMore bool            `json:"has_more"`
	Count   int             `json:"count"`
}

// Call 发起一次 Tushare 接口调用，内置：
//  1. 令牌桶限流（limiter.Wait）
//  2. 错误分类（Classify）：永久错误不重试直接返回；频率/瞬时错误按策略退避重试
//
// 成功返回 (fields, items)。失败返回 *APIError（含 Kind）。
func (c *Client) Call(ctx context.Context, apiName string, params map[string]interface{}, fields ...string) ([]string, [][]interface{}, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, nil, fmt.Errorf("tushare 限流等待被取消: %w", err)
		}
		fieldsOut, items, callErr := c.do(ctx, apiName, params, fields...)
		if callErr == nil {
			return fieldsOut, items, nil
		}

		apiErr, ok := callErr.(*APIError)
		if !ok {
			apiErr = &APIError{API: apiName, Code: 0, Msg: callErr.Error(), Kind: KindTransient, Err: callErr}
		}
		lastErr = apiErr

		switch apiErr.Kind {
		case KindPermanent:
			// 无权限/接口名错/积分不足：不重试，直接由上层告警
			return nil, nil, apiErr
		case KindRateLimited:
			if werr := sleepCtx(ctx, c.rateWindow); werr != nil {
				return nil, nil, werr
			}
		case KindTransient:
			backoff := c.baseBackoff * time.Duration(1<<uint(attempt))
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			if werr := sleepCtx(ctx, backoff); werr != nil {
				return nil, nil, werr
			}
		}
	}
	return nil, nil, lastErr
}

// do 执行单次 HTTP 请求并解析 Tushare 响应（不重试）。
func (c *Client) do(ctx context.Context, apiName string, params map[string]interface{}, fields ...string) ([]string, [][]interface{}, error) {
	reqBody := tushareRequest{API: apiName, Token: c.token, Params: params}
	if len(fields) > 0 {
		reqBody.Fields = strings.Join(fields, ",")
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, &APIError{API: apiName, Kind: KindTransient, Msg: "请求序列化失败", Err: err}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, &APIError{API: apiName, Kind: KindTransient, Msg: "构造请求失败", Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, nil, &APIError{API: apiName, Code: 0, Msg: "网络错误", Kind: KindTransient, Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, &APIError{API: apiName, Kind: KindTransient, Msg: "读取响应失败", Err: err}
	}
	var tr tushareResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, nil, &APIError{API: apiName, Kind: KindTransient, Msg: "响应 JSON 解析失败", Err: err}
	}
	if tr.Code != 0 {
		kind := Classify(tr.Code)
		return nil, nil, &APIError{API: apiName, Code: tr.Code, Msg: tr.Msg, Kind: kind}
	}
	if tr.Data == nil {
		return []string{}, [][]interface{}{}, nil
	}
	return tr.Data.Fields, tr.Data.Items, nil
}

// sleepCtx 可被 ctx 取消的 sleep。
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
