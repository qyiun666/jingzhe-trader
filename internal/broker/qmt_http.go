package broker

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// get 发送 GET 请求
func (q *QMTBridge) get(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, q.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}
	return q.do(req)
}

// post 发送 POST 请求
func (q *QMTBridge) post(path string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, q.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return q.do(req)
}

// do 执行请求, 统一携带鉴权头并读取响应
func (q *QMTBridge) do(req *http.Request) ([]byte, error) {
	if q.token != "" {
		req.Header.Set("X-QMT-Token", q.token)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}
