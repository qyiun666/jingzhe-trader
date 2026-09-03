package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// ---------- 邮件折行：≤990 字节且不切断 UTF-8 多字节字符（§10.5-4）----------

func checkFold(t *testing.T, input string) {
	t.Helper()
	lines := FoldLines(input)
	for i, l := range lines {
		if len(l) > MaxLineBytes {
			t.Errorf("第 %d 行 %d 字节 > 上限 %d", i, len(l), MaxLineBytes)
		}
		if !utf8.ValidString(l) {
			t.Errorf("第 %d 行不是合法 UTF-8: %q", i, l)
		}
	}
	// 无嵌入换行时，FoldLines 仅将超长行切块，join("") 应原样重建（不丢字符、不切断 rune）。
	if !strings.Contains(input, "\n") {
		if strings.Join(lines, "") != input {
			t.Errorf("重建结果与原串不一致\n got=%q\nwant=%q", strings.Join(lines, ""), input)
		}
	}
}

func TestFoldLines(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := FoldLines(""); got != nil {
			t.Errorf("空串应返回 nil，实际 %v", got)
		}
	})
	t.Run("short_line_unchanged", func(t *testing.T) {
		checkFold(t, "这是一条很短的邮件正文，远不足 990 字节。")
	})
	t.Run("exactly_990_ascii", func(t *testing.T) {
		s := strings.Repeat("a", 990)
		checkFold(t, s)
	})
	t.Run("ascii_overflow", func(t *testing.T) {
		// 2000 字节 ASCII：应折成 990 + 990 + 20
		s := strings.Repeat("a", 2000)
		lines := FoldLines(s)
		if len(lines) != 3 {
			t.Fatalf("期望 3 行，实际 %d: %v", len(lines), lines)
		}
		if len(lines[0]) != 990 || len(lines[1]) != 990 || len(lines[2]) != 20 {
			t.Errorf("分行长度错误: %d/%d/%d", len(lines[0]), len(lines[1]), len(lines[2]))
		}
		checkFold(t, s)
	})
	t.Run("utf8_cjk_safe", func(t *testing.T) {
		// 1000 个「中」= 3000 字节，含非 3 倍数长度的边界
		s := strings.Repeat("中", 1000)
		checkFold(t, s)
	})
	t.Run("utf8_mixed_boundary", func(t *testing.T) {
		// 330 中 + 1 X + 330 中 = 1981 字节，触发 rune 回退对齐
		s := strings.Repeat("中", 330) + "X" + strings.Repeat("中", 330)
		checkFold(t, s)
	})
	t.Run("utf8_emoji_safe", func(t *testing.T) {
		// 4 字节 emoji 同样不可切断
		s := strings.Repeat("🚀", 300) // 1200 字节
		checkFold(t, s)
	})
	t.Run("multi_line", func(t *testing.T) {
		// 每行独立折行；长行被折、短行保留；原始换行被当作折行分隔符（不保留为字符）。
		// 正确不变量：每个输出行 ≤990 字节且为合法 UTF-8；内容字符数与原串一致（仅插入分隔符）。
		input := "短行\n" + strings.Repeat("中", 400) + "\n另一短行"
		lines := FoldLines(input)
		for i, l := range lines {
			if len(l) > MaxLineBytes {
				t.Errorf("第 %d 行 %d 字节超上限", i, len(l))
			}
			if !utf8.ValidString(l) {
				t.Errorf("第 %d 行非法 UTF-8", i)
			}
		}
		got := strings.Join(lines, "")
		// 折叠只改变"换行分隔符"，非换行字符必须全部保留。
		wantContent := strings.ReplaceAll(input, "\n", "")
		if utf8.RuneCountInString(got) != utf8.RuneCountInString(wantContent) {
			t.Errorf("多行折叠丢失非换行字符：got=%d rune, want=%d", utf8.RuneCountInString(got), utf8.RuneCountInString(wantContent))
		}
	})
}
