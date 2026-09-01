package tushare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/pkg/logger"
	"jingzhe-trader/pkg/retry"
)

// rateLimitWindow Tushare 频次限制的时间窗: 触发限流后等满一个窗口再重试
const rateLimitWindow = time.Minute

// burstTokens 令牌桶容量: 允许略高于并发的瞬时突发, 不放开整分钟配额
const burstTokens = 8

// Client Tushare API 客户端
// 封装了 HTTP 请求、令牌桶限流和指数退避重试逻辑
type Client struct {
	token         string
	baseURL       string
	httpClient    *http.Client
	rateBucket    chan struct{} // 令牌桶: 容量为突发上限, 长期速率由 refillTicker 按配额速率补充
	maxRetries    int           // 最大重试次数
	retryInterval time.Duration // 基础重试间隔, 实际退避按指数增长 (分钟级限流除外, 按整窗等待)
	quit          chan struct{} // refillTicker 退出信号 (Close 关闭)
	closeOnce     sync.Once
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
		c.quit = make(chan struct{})
		// 桶容量只容纳调度并发, 不等于每分钟配额:
		// 容量取配额会让整分钟的请求在一次突发里全部打完, 分钟级限制形同不存在
		capacity := burstTokens
		if cfg.RateLimit < capacity {
			capacity = cfg.RateLimit
		}
		c.rateBucket = make(chan struct{}, capacity)
		// 按配置速率匀速补充令牌, 由补充速度决定长期吞吐
		interval := rateLimitWindow / time.Duration(cfg.RateLimit)
		go c.refillTicker(interval)
	}

	return c
}

// Close 停止限流令牌补充 goroutine
// 进程内短生命周期 Client (如每次手动触发数据更新) 用完必须调用, 否则每次 NewClient 泄漏一个 goroutine
func (c *Client) Close() {
	if c.quit != nil {
		c.closeOnce.Do(func() { close(c.quit) })
	}
}

// refillTicker 启动一个定时器, 按固定间隔向令牌桶补充令牌
func (c *Client) refillTicker(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.quit:
			return
		case <-ticker.C:
			select {
			case c.rateBucket <- struct{}{}:
				// 成功放入一个令牌
			default:
				// 桶已满, 丢弃本次令牌
			}
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
//
// 只有"重试可能成功"的错误才重试: 权限/参数/日配额类错误重试只是白烧请求数与当天计次。
func (c *Client) call(apiName string, params map[string]interface{}, fields string) (*Response, error) {
	retries := c.maxRetries
	if retries < 0 {
		retries = 0
	}

	var lastErr error
	var waitBeforeNext time.Duration // 0 = 用默认指数退避
	for attempt := 0; attempt <= retries; attempt++ {
		// 重试时进行退避等待
		if attempt > 0 {
			wait := waitBeforeNext
			if wait == 0 {
				wait = retry.Backoff(c.retryInterval, attempt)
			}
			logger.L().Warnf("tushare %s 第 %d 次重试, 等待 %s: %v", apiName, attempt, wait, lastErr)
			time.Sleep(wait)
		}
		waitBeforeNext = 0

		c.waitForToken()

		resp, err := c.doRequest(apiName, params, fields)
		if err != nil {
			lastErr = err
			logger.L().Warnf("tushare %s 请求失败(第 %d 次): %v", apiName, attempt+1, err)
			continue
		}
		if resp.Code != 0 {
			apiErr := classify(apiName, resp.Code, resp.Msg)
			if apiErr.Permanent {
				return nil, fmt.Errorf("%w (不重试: %s)", apiErr, apiErr.Reason)
			}
			lastErr = apiErr
			if apiErr.RateLimited {
				// 分钟级限制: 指数退避的那几秒跨不出时间窗, 等满一个窗口再试
				waitBeforeNext = rateLimitWindow
			}
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
