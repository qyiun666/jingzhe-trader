package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client LLM 客户端
// 支持 OpenAI 兼容接口 (DeepSeek, 通义千问, 智谱等)
type Client struct {
	apiKey     string
	baseURL    string
	model      string
	httpClient *http.Client
	enabled    bool
}

// Config LLM 配置
type Config struct {
	APIKey  string `mapstructure:"api_key"`
	BaseURL string `mapstructure:"base_url"` // 默认 "https://api.deepseek.com/v1"
	Model   string `mapstructure:"model"`    // 默认 "deepseek-chat"
	Enabled bool   `mapstructure:"enabled"`
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
	return &Client{
		apiKey:  cfg.APIKey,
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
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
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
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
		Temperature: 0.3, // 分析类任务用低温度，保证输出稳定
		MaxTokens:   1024,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("序列化请求失败: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 LLM 接口失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	var result ChatCompletionResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析响应失败: %w, 原始内容: %s", err, string(respBody))
	}

	if result.Error != nil {
		return "", fmt.Errorf("LLM 错误: %s", result.Error.Message)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("无响应内容")
	}

	return result.Choices[0].Message.Content, nil
}
