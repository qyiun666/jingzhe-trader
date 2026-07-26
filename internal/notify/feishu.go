package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"jingzhe-trader/pkg/logger"
)

// ==================== 飞书卡片结构定义 ====================

// FeishuCard 飞书消息卡片
type FeishuCard struct {
	Config   FeishuCardConfig    `json:"config"`
	Header   *FeishuCardHeader   `json:"header,omitempty"`
	Elements []FeishuCardElement `json:"elements"`
}

// FeishuCardConfig 卡片配置
type FeishuCardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
	EnableForward  bool `json:"enable_forward"`
}

// FeishuCardHeader 卡片头部
type FeishuCardHeader struct {
	Title    *FeishuCardTitle `json:"title"`
	Template string           `json:"template"` // red/orange/green/blue/grey
}

// FeishuCardTitle 卡片标题
type FeishuCardTitle struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

// FeishuCardElement 卡片元素 (支持多种类型)
type FeishuCardElement struct {
	Tag    string        `json:"tag"` // "div" / "field" / "note" / "action"
	Text   *FeishuText   `json:"text,omitempty"`
	Fields []FeishuField `json:"fields,omitempty"`
	Action *FeishuAction `json:"action,omitempty"`
}

// FeishuAction 卡片动作按钮
type FeishuAction struct {
	Tag  string      `json:"tag"` // "button"
	Text *FeishuText `json:"text"`
	URL  string      `json:"url,omitempty"`
	Type string      `json:"type"` // "primary" / "default" / "danger"
}

// FeishuText 富文本
type FeishuText struct {
	Tag     string `json:"tag"` // "lark_md" / "plain_text"
	Content string `json:"content"`
}

// FeishuField 字段组
type FeishuField struct {
	IsShort bool        `json:"is_short"`
	Text    *FeishuText `json:"text"`
}

// MdText 创建 lark_md 富文本
func MdText(content string) *FeishuText {
	return &FeishuText{Tag: "lark_md", Content: content}
}

// ==================== 飞书发送器 ====================

// FeishuNotifier 飞书 Webhook 通知器
// webhook 为空时所有发送调用降级为 no-op, 调用方无需判空
type FeishuNotifier struct {
	webhook string
	client  *http.Client
}

// NewFeishuNotifier 创建飞书通知器
func NewFeishuNotifier(webhook string) *FeishuNotifier {
	return &FeishuNotifier{
		webhook: webhook,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Enabled 是否已配置 webhook
func (n *FeishuNotifier) Enabled() bool {
	return n != nil && n.webhook != ""
}

// SendCard 发送卡片消息
func (n *FeishuNotifier) SendCard(card *FeishuCard) error {
	if !n.Enabled() || card == nil {
		return nil
	}
	body := map[string]interface{}{
		"msg_type": "interactive",
		"card":     card,
	}
	return n.post(body)
}

// SendText 发送纯文本消息 (告警/异常通知)
func (n *FeishuNotifier) SendText(text string) error {
	if !n.Enabled() || text == "" {
		return nil
	}
	body := map[string]interface{}{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	}
	return n.post(body)
}

// feishuResp 飞书 webhook 响应
type feishuResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// post 发送 webhook 请求
func (n *FeishuNotifier) post(body map[string]interface{}) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化飞书消息失败: %w", err)
	}

	resp, err := n.client.Post(n.webhook, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("发送飞书消息失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return fmt.Errorf("读取飞书响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("飞书响应异常: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var fr feishuResp
	if err := json.Unmarshal(respBody, &fr); err == nil && fr.Code != 0 {
		return fmt.Errorf("飞书返回错误: code=%d msg=%s", fr.Code, fr.Msg)
	}
	logger.L().Debugf("[Feishu] 消息发送成功")
	return nil
}
