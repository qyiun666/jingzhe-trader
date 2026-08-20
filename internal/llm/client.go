package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// maxAttempts 最大尝试次数(含首次请求), 即最多重试 2 次
	maxAttempts = 3
	// defaultMaxTokens 输出上限默认值, 过小容易导致 LLM 返回的 JSON 被截断
	defaultMaxTokens = 2048
	// defaultTemperature 分析类任务默认低温度, 保证输出稳定
	defaultTemperature = 0.3
	// defaultTimeoutSeconds HTTP 请求超时默认值
	defaultTimeoutSeconds = 30
)

// retryBackoffBase 重试退避基数(指数退避: base, 2*base), 测试可调小以加速
var retryBackoffBase = time.Second

// Client LLM 客户端
// 仅支持 DeepSeek API (OpenAI 兼容接口)
type Client struct {
	apiKey      string
	baseURL     string
	model       string
	httpClient  *http.Client
	enabled     bool
	temperature float64
	maxTokens   int
	jsonMode    bool
	cache       sync.Map // 进程内缓存: key=date+symbol+role+输入hash, value=响应内容
}

// Config LLM 配置
type Config struct {
	APIKey         string  `mapstructure:"api_key"`
	BaseURL        string  `mapstructure:"base_url"` // 默认 "https://api.deepseek.com/v1"
	Model          string  `mapstructure:"model"`    // 默认 "deepseek-chat"
	Enabled        bool    `mapstructure:"enabled"`
	Temperature    float64 `mapstructure:"temperature"`     // 默认 0.3
	MaxTokens      int     `mapstructure:"max_tokens"`      // 默认 2048
	TimeoutSeconds int     `mapstructure:"timeout_seconds"` // 默认 30
	JSONMode       *bool   `mapstructure:"json_mode"`       // nil 表示默认 false, 强制 JSON 输出 (仅 DeepSeek 支持)
}

// NewClient 创建 LLM 客户端
// 如果未启用或 API Key 为空，返回一个禁用状态的客户端，所有调用都会返回错误
func NewClient(cfg Config) *Client {
	if !cfg.Enabled || cfg.APIKey == "" {
		return &Client{enabled: false}
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	model := cfg.Model
	if model == "" {
		model = "deepseek-chat"
	}
	temperature := cfg.Temperature
	if temperature < 0 {
		temperature = defaultTemperature
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds
	}
	jsonMode := false // 默认关闭, 仅 DeepSeek 支持
	if cfg.JSONMode != nil {
		jsonMode = *cfg.JSONMode
	}
	return &Client{
		apiKey:      cfg.APIKey,
		baseURL:     baseURL,
		model:       model,
		temperature: temperature,
		maxTokens:   maxTokens,
		jsonMode:    jsonMode,
		httpClient: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		},
		enabled: true,
	}
}

// IsEnabled 是否启用了 LLM
func (c *Client) IsEnabled() bool {
	return c.enabled
}

// ChatMessage 聊天消息
type ChatMessage struct {
	Role    string `json:"role"` // system / user / assistant
	Content string `json:"content"`
}

// ChatCompletionRequest 请求体
type ChatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []ChatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ResponseFormat 响应格式约束 (OpenAI 兼容 json_object mode)
type ResponseFormat struct {
	Type string `json:"type"`
}

// ChatCompletionResponse 响应体
type ChatCompletionResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat 发送聊天请求
// systemPrompt: 系统提示词，定义角色和输出格式
// userPrompt: 用户提示词，包含具体的任务内容
// 仅对网络错误 / 5xx / 429 做指数退避重试(最多 maxAttempts 次), 4xx 等业务错误不重试
func (c *Client) Chat(systemPrompt, userPrompt string) (string, error) {
	if !c.enabled {
		return "", fmt.Errorf("LLM 未启用")
	}

	reqBody := ChatCompletionRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
	}
	if c.jsonMode {
		// 强制 JSON 输出, 避免分析结果被 markdown 代码块或散文包裹
		// 前置条件: prompt 中需包含 "json" 字样 (所有内置 prompt 均已满足)
		reqBody.ResponseFormat = &ResponseFormat{Type: "json_object"}
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("序列化请求失败: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避: 第1次重试等 base, 第2次等 2*base
			time.Sleep(retryBackoffBase << (attempt - 1))
		}
		content, retryable, err := c.doChatRequest(bodyBytes)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !retryable {
			// 4xx 等业务错误重试无意义, 直接返回
			return "", err
		}
	}
	return "", lastErr
}

// ChatWithCache 带进程内缓存的聊天请求
// key = 日期 + 股票代码 + 角色 + 输入内容hash, 同日同股票同角色且输入相同时直接命中缓存, 不重复调用 LLM
// 仅缓存成功响应; 进程内有效, 进程重启后自动失效
func (c *Client) ChatWithCache(tradeDate, tsCode, role, systemPrompt, userPrompt string) (string, error) {
	if !c.enabled {
		return "", fmt.Errorf("LLM 未启用")
	}
	sum := sha256.Sum256([]byte(systemPrompt + "\x00" + userPrompt))
	key := fmt.Sprintf("%s|%s|%s|%s", tradeDate, tsCode, role, hex.EncodeToString(sum[:]))
	if v, ok := c.cache.Load(key); ok {
		return v.(string), nil
	}
	resp, err := c.Chat(systemPrompt, userPrompt)
	if err != nil {
		return "", err
	}
	actual, _ := c.cache.LoadOrStore(key, resp)
	return actual.(string), nil
}

// doChatRequest 执行单次聊天请求
// 返回 retryable=true 表示网络错误 / 5xx / 429, 可安全重试; 4xx 及响应解析错误不重试
func (c *Client) doChatRequest(bodyBytes []byte) (string, bool, error) {
	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", false, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// 网络层错误(连接失败/超时等), 可重试
		return "", true, fmt.Errorf("请求 LLM 接口失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", true, fmt.Errorf("读取响应失败: %w", err)
	}

	// 非 2xx 响应直接报错 (避免将 HTML 错误页误报为"解析失败")
	// 429(限流) 与 5xx(服务端错误) 可重试, 其余 4xx 不重试
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result ChatCompletionResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", false, fmt.Errorf("解析响应失败: %w, 原始内容: %s", err, string(respBody))
	}

	if result.Error != nil {
		return "", false, fmt.Errorf("LLM 错误: %s", result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return "", false, fmt.Errorf("无响应内容")
	}

	return result.Choices[0].Message.Content, false, nil
}
