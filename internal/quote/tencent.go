package quote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"jingzhe-trader/internal/model"
)

// TencentSource 腾讯 qt.gtimg.cn 行情（降级备用源）。GBK 解码。
type TencentSource struct {
	httpClient *http.Client
	cacheMu    sync.Mutex
	cache      map[string]Quote
}

// NewTencentSource 构造腾讯降级源。
func NewTencentSource() *TencentSource {
	return &TencentSource{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		cache:      make(map[string]Quote),
	}
}

// Fetch 拉取腾讯实时行情（GBK→UTF-8）。
func (s *TencentSource) Fetch(ctx context.Context, tsCodes []string) (map[string]Quote, error) {
	q := "http://qt.gtimg.cn/q=" + strings.Join(toTencentCodes(tsCodes), ",")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// GBK → UTF-8
	utf8, _, derr := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), raw)
	if derr != nil {
		return nil, fmt.Errorf("GBK 解码失败: %w", derr)
	}
	return s.parse(string(utf8), tsCodes)
}

// parse 解析腾讯行情文本行：v_sh600519="1~名称~代码~当前价~昨收~今开~..."。
func (s *TencentSource) parse(body string, tsCodes []string) (map[string]Quote, error) {
	res := make(map[string]Quote, len(tsCodes))
	for _, line := range strings.Split(body, ";") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "v_") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := line[2:eq] // sh600519
		val := strings.Trim(line[eq+1:], "\"")
		if val == "" {
			continue
		}
		parts := strings.Split(val, "~")
		if len(parts) < 6 {
			continue
		}
		ts := toTsCode(key)
		qq := Quote{
			TsCode:   ts,
			Price:    parseFen(parts[3]),
			PreClose: parseFen(parts[4]),
			Open:     parseFen(parts[5]),
			High:     parseFen(at(parts, 33)),
			Low:      parseFen(at(parts, 34)),
			Source:   "tencent",
		}
		if qq.High.IsZero() {
			qq.High = qq.Price
		}
		if qq.Low.IsZero() {
			qq.Low = qq.Price
		}
		res[ts] = qq
		s.cacheMu.Lock()
		s.cache[ts] = qq
		s.cacheMu.Unlock()
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("腾讯行情返回为空")
	}
	return res, nil
}

func parseFen(s string) model.Fen {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return model.FromFloat(f)
}

func at(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return ""
}

func toTencentCodes(tsCodes []string) []string {
	out := make([]string, 0, len(tsCodes))
	for _, tc := range tsCodes {
		out = append(out, toTencentCode(tc))
	}
	return out
}

func toTencentCode(tc string) string {
	parts := strings.SplitN(tc, ".", 2)
	code := parts[0]
	suffix := ""
	if len(parts) == 2 {
		suffix = strings.ToLower(parts[1])
	}
	switch suffix {
	case "sh":
		return "sh" + code
	case "sz":
		return "sz" + code
	case "bj":
		return "bj" + code
	default:
		return toTencentCodeByPrefix(code)
	}
}

func toTencentCodeByPrefix(code string) string {
	if len(code) == 0 {
		return code
	}
	switch code[0] {
	case '6':
		return "sh" + code
	case '0', '3':
		return "sz" + code
	case '8', '9':
		return "bj" + code
	default:
		return "sh" + code
	}
}

func toTsCode(tencentKey string) string {
	if len(tencentKey) < 2 {
		return tencentKey
	}
	prefix := tencentKey[:2]
	code := tencentKey[2:]
	var suffix string
	switch strings.ToLower(prefix) {
	case "sh":
		suffix = "SH"
	case "sz":
		suffix = "SZ"
	case "bj":
		suffix = "BJ"
	default:
		suffix = "SH"
	}
	return code + "." + suffix
}
