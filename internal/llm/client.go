// Package llm DeepSeek 二次质证（P1 可选增强，默认关闭）。
//
// 原则（PRD / §5.1 15:40）：LLM 只做买入候选终审，不参与决策否决之外的环节；
// 失败必须显式告警（LLM_FAILED）并标注"未质证"，绝不把失败标成"已分析"。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client DeepSeek Chat Completions 客户端（http.Client 可注入，单测打桩）。
type Client struct {
	apiKey  string
	baseURL string
	model   string
	hc      *http.Client
}

// NewClient 构造客户端。hc 为 nil 时使用 30s 超时的默认客户端。
func NewClient(apiKey, baseURL, model string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"), model: model, hc: hc}
}

// Enabled 客户端是否可用（key/model 配置齐全）。
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != "" && c.model != ""
}

// chatRequest / chatResponse OpenAI 兼容协议的最小子集。
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat 发起一次对话补全，返回首个 choice 的文本内容。
// 非 200、响应含 error、choices 为空均返回显式错误（绝不返回空内容 + nil error）。
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("llm 客户端未配置（api_key/model 缺失）")
	}
	reqBody, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: 0.2,
	})
	if err != nil {
		return "", fmt.Errorf("序列化 LLM 请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("构建 LLM 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 LLM 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取 LLM 响应失败: %w", err)
	}
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("解析 LLM 响应失败（http %d）: %w", resp.StatusCode, err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("LLM 返回错误: %s", cr.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM http 状态 %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	if len(cr.Choices) == 0 || cr.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("LLM 响应为空（choices=%d）", len(cr.Choices))
	}
	return cr.Choices[0].Message.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
