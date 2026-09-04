// Package llm DeepSeek Responses API 客户端与买入评审。
//
// 原则：LLM 是买入决策者（标的与数量），风控只做硬截断；
// 失败必须显式落库并告警，绝不把失败标成"已分析"。
//
// 协议：只用 /v1/responses（不是 /chat/completions）。实测 deepseek-v4-flash 的
// web_search 工具只挂在 Responses API 上，chat 协议侧确实没有联网能力。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"jingzhe-trader/internal/observability"
)

// maxOutputTokens 单次补全的输出预算。
//
// 实测（2026-09-04）该接口 `max_output_tokens` 合法区间 [1, 393216]，而本文件以前
// 从不传这个字段——不传时服务端给的默认额度会被 reasoning token 先吃掉一大半
// （一问 6 只票、out=4805 里 reasoning 占 3057），整批 JSON 就写到一半停了。
// 表现是"评审未问出结果"，真因是输出预算，与模型听不听话无关。
// 取 131072：一批 TopN=20 只、每只 300 字理由也远远用不完，且计费按实际产出。
const maxOutputTokens = 131072

// Client DeepSeek Responses API 客户端（hc 可注入，单测打桩）。
type Client struct {
	apiKey     string
	baseURL    string
	model      string
	searchSize string // 配置的检索档（low|medium|high，装配期已校验）
	hc         *http.Client
}

// NewClient 构造客户端。hc 为 nil 时使用自带 600s 超时的默认客户端。
//
// 为什么这么长：改成"整批一次问"之后，单次回答的长度是逐只问的 N 倍——实测 6 只候选的
// 板块地位那一问在 120s 内没写完，被 context deadline exceeded 掐死、整批判成评审失败；
// 挂 web_search 的调用还要跑多轮检索。批量的代价就是把时间集中到一次请求上，
// 而一次请求被掐死的后果是"当日零计划"，所以宁可等久一点。
//
// searchSize 只接受 low|medium|high，非法值由装配层 validateEnums 拒绝，这里不再"认不出来用默认"。
func NewClient(apiKey, baseURL, model, searchSize string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 600 * time.Second}
	}
	return &Client{apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"),
		model: model, searchSize: searchSize, hc: hc}
}

// Enabled 客户端是否可用（key/model 配置齐全）。
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != "" && c.model != ""
}

// Request 一次补全的输入。Search=true 时挂 web_search 工具，只给需要外部事实的
// 维度用（其余维度刻意不联网，见 prompts.go）。
type Request struct {
	System string
	User   string
	Search bool
}

type toolSpec struct {
	Type              string `json:"type"`
	SearchContextSize string `json:"search_context_size,omitempty"`
}

type responseReq struct {
	Model           string     `json:"model"`
	Input           string     `json:"input"`
	Instructions    string     `json:"instructions"`
	Temperature     float64    `json:"temperature"`
	MaxOutputTokens int        `json:"max_output_tokens,omitempty"`
	Tools           []toolSpec `json:"tools,omitempty"`
}

type outputItem struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responseResp struct {
	Status string       `json:"status"`
	Output []outputItem `json:"output"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// errTransport 标记"请求根本没拿到一个可解析的响应"：连不上、超时、响应体不是 JSON、
// 服务端 5xx。只有这一类值得原样重问一次。
//
// 与之相对：status 不是 completed、正文残缺、少答标的，都是"问到了但没答合契约"，
// 再问一遍不会有别的结局 —— 以前的"截断"其实是没传 max_output_tokens，见上面的常量。
var errTransport = errors.New("llm 传输层失败")

// errNoAnswer 标记"这一问没拿到任何答案"（响应里没有 message 条目）。
// 与传输层同属"这次什么都没给"，允许重问；与"给了但不合契约"是两回事。
var errNoAnswer = errors.New("llm 没有给出答案")

// Complete 发起一次补全，返回最终 message 正文。
//
// v4-flash 会先吐若干 reasoning / web_search_call 条目，答案只在其后的 message 里；
// 拿不到 message 一律按失败处理（绝不返回空串 + nil error 让上层当成"模型说没有"）。
// 每次调用（成与败）都记一行日志：token 用量与 status 是判断"截断还是没答"的唯一依据。
func (c *Client) Complete(ctx context.Context, req Request) (text string, callErr error) {
	if !c.Enabled() {
		return "", fmt.Errorf("llm 客户端未配置（api_key/model 缺失）")
	}
	body := responseReq{
		Model: c.model, Input: req.User, Instructions: req.System,
		Temperature: 0.2, MaxOutputTokens: maxOutputTokens,
	}
	if req.Search {
		body.Tools = []toolSpec{{Type: "web_search", SearchContextSize: c.searchSize}}
	}

	var rr responseResp
	started := time.Now()
	defer func() {
		fields := []any{"model", c.model, "search", req.Search, "search_size", c.searchSize,
			"status", rr.Status, "items", len(rr.Output),
			"in_tokens", rr.Usage.InputTokens, "out_tokens", rr.Usage.OutputTokens,
			"reasoning_tokens", rr.Usage.OutputTokensDetails.ReasoningTokens,
			"max_output_tokens", maxOutputTokens, "secs", time.Since(started).Seconds()}
		if callErr != nil {
			observability.S().Errorw("LLM 补全失败", append(fields, "err", callErr.Error())...)
			return
		}
		observability.S().Infow("LLM 补全", append(fields, "chars", len(text))...)
	}()

	reqBody, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("序列化 LLM 请求失败: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("构建 LLM 请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("调用 LLM 失败: %w（%w）", err, errTransport)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("读取 LLM 响应失败: %w（%w）", err, errTransport)
	}
	if err := json.Unmarshal(raw, &rr); err != nil {
		return "", fmt.Errorf("解析 LLM 响应失败（http %d）: %w（%w）", resp.StatusCode, err, errTransport)
	}
	// 状态码先判：5xx 是"这次请求没成"，属传输层，可原样重问一次；
	// 服务端错误体里的 message 只是给人看的补充，不改变可不可重试的分类。
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError {
			return "", fmt.Errorf("LLM http 状态 %d: %s（%w）",
				resp.StatusCode, truncate(string(raw), 300), errTransport)
		}
		return "", fmt.Errorf("LLM http 状态 %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if rr.Error != nil && rr.Error.Message != "" {
		return "", fmt.Errorf("LLM 返回错误: %s", rr.Error.Message)
	}
	// status 不是 completed 就是"这一问没答完"，直接点名原因；
	// 让它落到下面的"无正文/不可解析"会把输出预算问题误诊成模型不听话。
	if rr.Status != "" && rr.Status != "completed" {
		return "", fmt.Errorf("LLM 输出未完成 status=%s（in=%d out=%d reasoning=%d，预算 %d）",
			rr.Status, rr.Usage.InputTokens, rr.Usage.OutputTokens,
			rr.Usage.OutputTokensDetails.ReasoningTokens, maxOutputTokens)
	}
	text = lastMessage(rr.Output)
	if text == "" {
		// 空答案：status=completed 却没有任何 message 条目（实测 out=368、reasoning=368，
		// 预算全花在推理上就收尾了）。它和传输层失败同类 —— 这次没拿到东西，可以重问。
		return "", fmt.Errorf("LLM 响应无 message 正文（status=%s items=%d in=%d out=%d reasoning=%d）（%w）",
			rr.Status, len(rr.Output), rr.Usage.InputTokens, rr.Usage.OutputTokens,
			rr.Usage.OutputTokensDetails.ReasoningTokens, errNoAnswer)
	}
	return text, nil
}

// lastMessage 取最后一个 message 条目的全部 text 段。
//
// 取"最后一个"而不是第一个：v4-flash 挂检索时会先吐若干段过场话（"我换个词再查"），
// 真正的答案在它后面。
func lastMessage(items []outputItem) string {
	var last string
	for _, it := range items {
		if it.Type != "message" {
			continue
		}
		var b strings.Builder
		for _, c := range it.Content {
			if c.Type == "text" || c.Text != "" {
				b.WriteString(c.Text)
			}
		}
		if b.Len() > 0 {
			last = b.String()
		}
	}
	return last
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
