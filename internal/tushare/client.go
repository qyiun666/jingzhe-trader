package tushare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/pkg/logger"
)

// Client Tushare API 客户端
// 封装了 HTTP 请求、令牌桶限流和指数退避重试逻辑
type Client struct {
	token         string
	baseURL       string
	httpClient    *http.Client
	rateBucket    chan struct{} // 令牌桶: 缓冲大小为每分钟配额, 取走一个令牌才能发起请求
	maxRetries    int           // 最大重试次数
	retryInterval time.Duration // 基础重试间隔, 实际退避按指数增长
}

// NewClient 根据 TushareConfig 构造一个客户端
// 当 RateLimit > 0 时启用令牌桶限流, 每分钟最多 RateLimit 次请求
func NewClient(cfg config.TushareConfig) *Client {
	c := &Client{
		token:         cfg.Token,
		baseURL:       cfg.BaseURL,
		httpClient:    &http.Client{Timeout: 60 * time.Second},
		maxRetries:    cfg.MaxRetries,
		retryInterval: time.Duration(cfg.RetryInterval) * time.Second,
	}

	// 构造令牌桶限流器
	if cfg.RateLimit > 0 {
		capacity := cfg.RateLimit
		c.rateBucket = make(chan struct{}, capacity)
		// 预先填满令牌, 允许开始时的突发请求
		for i := 0; i < capacity; i++ {
			c.rateBucket <- struct{}{}
		}
		// 每分钟均匀补充 RateLimit 个令牌
		interval := time.Minute / time.Duration(capacity)
		go c.refillTicker(interval)
	}

	return c
}

// refillTicker 启动一个定时器, 按固定间隔向令牌桶补充令牌
func (c *Client) refillTicker(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		select {
		case c.rateBucket <- struct{}{}:
			// 成功放入一个令牌
		default:
			// 桶已满, 丢弃本次令牌
		}
	}
}

// waitForToken 阻塞等待获取一个令牌(若启用了限流)
func (c *Client) waitForToken() {
	if c.rateBucket != nil {
		<-c.rateBucket
	}
}

// call 通用请求入口, 包含限流与指数退避重试
// apiName: Tushare 接口名; params: 接口参数; fields: 需返回的字段(逗号分隔, 可空)
func (c *Client) call(apiName string, params map[string]interface{}, fields string) (*Response, error) {
	retries := c.maxRetries
	if retries < 0 {
		retries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		// 重试时进行指数退避等待
		if attempt > 0 {
			backoff := c.retryInterval * time.Duration(1<<(attempt-1))
			logger.L().Warnf("tushare %s 第 %d 次重试, 等待 %s", apiName, attempt, backoff)
			time.Sleep(backoff)
		}

		c.waitForToken()

		resp, err := c.doRequest(apiName, params, fields)
		if err != nil {
			lastErr = err
			logger.L().Warnf("tushare %s 请求失败(第 %d 次): %v", apiName, attempt+1, err)
			continue
		}
		// code != 0 视为业务错误, 进行重试
		if resp.Code != 0 {
			lastErr = fmt.Errorf("tushare API %s 返回错误: code=%d msg=%s", apiName, resp.Code, resp.Msg)
			logger.L().Warnf("tushare %s 业务错误(第 %d 次): code=%d msg=%s", apiName, attempt+1, resp.Code, resp.Msg)
			continue
		}
		return resp, nil
	}

	return nil, fmt.Errorf("tushare 请求 %s 重试 %d 次后仍失败: %w", apiName, retries, lastErr)
}

// doRequest 执行一次 HTTP POST 请求并解析响应
func (c *Client) doRequest(apiName string, params map[string]interface{}, fields string) (*Response, error) {
	reqBody := Request{
		APIName: apiName,
		Token:   c.token,
		Params:  params,
		Fields:  fields,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var tushareResp Response
	if err := json.Unmarshal(raw, &tushareResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w, body=%s", err, string(raw))
	}
	return &tushareResp, nil
}
