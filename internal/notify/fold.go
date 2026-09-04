// Package notify 邮件构建/折行/发送/重试、告警落库与分级（D1 破解"任务全绿但零邮件"）。
//
// 核心原则：
//   - 邮件未启用不是 no-op：Send 显式落一条 mail:* 失败轨迹并返回 ErrNotConfigured；
//   - 折行按字节（≤990）且不切断 UTF-8 多字节字符（§6-D8 / 附录 A）；
//   - urgent 告警立即发信，普通告警同 code 1 小时去重。
package notify

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxLineBytes 邮件单行字节上限（RFC 5321 为 998，留 8 字节余量，附录 A）。
const MaxLineBytes = 990

// ErrNotConfigured 邮件未启用或配置缺失（显式失败哨兵，errors.Is 判等）。
var ErrNotConfigured = errors.New("邮件未启用或收件人/发件人未配置")

// FoldLines 将正文按行折行：
// 任一输出行字节数 ≤ MaxLineBytes，且绝不在 UTF-8 多字节字符中间截断。
// 空输入返回空切片。纯函数（验收 §10.5-4）。
func FoldLines(body string) []string {
	if body == "" {
		return nil
	}
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		for len(line) > MaxLineBytes {
			cut := MaxLineBytes
			// 回退到 rune 起始字节：line[cut] 是某 rune 的首字节 → line[:cut] 为完整字符序列
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			if cut == 0 {
				// 单 rune 超上限（不可能出现于合法 UTF-8）：强制 1 字节防死循环
				cut = 1
			}
			out = append(out, line[:cut])
			line = line[cut:]
		}
		out = append(out, line)
	}
	return out
}

// FoldBody 折行后以 \r\n 连接（SMTP 正文行分隔符）。
func FoldBody(body string) string {
	return strings.Join(FoldLines(body), "\r\n")
}
