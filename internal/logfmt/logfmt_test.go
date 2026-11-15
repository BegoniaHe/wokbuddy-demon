package logfmt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncate 截断必须落在 UTF-8 rune 边界；先 TrimSpace，n<=0 返回空串。
// 各调用方（上游错误文本、CLI 表格、面板载荷）都依赖这几个口径。
func TestTruncate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		in    string
		n     int
		want  string
		valid bool
	}{
		{"未超长原样返回", "uid-123", 36, "uid-123", true},
		{"恰好等于上限", "abc", 3, "abc", true},
		{"中文字符整体让出", "测试昵称", 11, "测试昵", true},
		{"首字符即多字节", "测", 1, "", true},
		{"纯 ASCII 截断", "abcdef", 3, "abc", true},
		{"先 trim 再截断", "  abc  ", 5, "abc", true},
		{"n 为零", "abc", 0, "", true},
		{"n 为负", "abc", -1, "", true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := Truncate(testCase.in, testCase.n)
			if got != testCase.want {
				t.Fatalf("Truncate(%q, %d) = %q want %q", testCase.in, testCase.n, got, testCase.want)
			}

			if testCase.valid && !utf8.ValidString(got) {
				t.Fatalf("Truncate(%q, %d) 不是合法 UTF-8: %q", testCase.in, testCase.n, got)
			}

			if len(got) > max(testCase.n, 0) {
				t.Fatalf("Truncate(%q, %d) 超上限: %d 字节", testCase.in, testCase.n, len(got))
			}
		})
	}

	// 长中文串在任意字节上限下都不得切出半个字符。
	long := strings.Repeat("错", 40)
	for n := 1; n <= len(long); n++ {
		if got := Truncate(long, n); !utf8.ValidString(got) {
			t.Fatalf("Truncate(long, %d) 不是合法 UTF-8: %q", n, got)
		}
	}
}

// TestTruncateEllipsis 只有真正超限才追加省略号；仅裁掉首尾空白不算截断。
func TestTruncateEllipsis(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"未超限", "abc", 3, "abc"},
		{"超限追加省略号", "abcdef", 3, "abc…"},
		{"中文按 rune 边界", "测试昵称", 11, "测试昵…"},
		{"只裁空白不追加", "  abc  ", 3, "abc"},
		{"n 为零", "abc", 0, ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := TruncateEllipsis(testCase.in, testCase.n)
			if got != testCase.want {
				t.Fatalf("TruncateEllipsis(%q, %d) = %q want %q", testCase.in, testCase.n, got, testCase.want)
			}

			if !utf8.ValidString(got) {
				t.Fatalf("TruncateEllipsis(%q, %d) 不是合法 UTF-8: %q", testCase.in, testCase.n, got)
			}
		})
	}
}

// TestUID8 uid 统一截 8 位；空 uid 用 "-" 占位（日志表格列宽依赖该口径）。
func TestUID8(t *testing.T) {
	t.Parallel()

	cases := []struct {
		uid, want string
	}{
		{"00e26541abcdef012345", "00e26541"},
		{"abc", "abc"},
		{"", "-"},
		{"12345678", "12345678"},
	}
	for _, c := range cases {
		if got := UID8(c.uid); got != c.want {
			t.Errorf("UID8(%q)=%q want %q", c.uid, got, c.want)
		}
	}
}

// 昵称来自上游 JSON（不可信）：含换行就能在运行日志里伪造整行，过长会把一条
// 日志撑成多行。Label 必须折叠控制字符并截断。
func TestLabelSanitizesNickname(t *testing.T) {
	t.Parallel()

	got := Label("abcdef1234567890", "坏\r\n昵称\tX")
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("Label=%q 仍含控制字符", got)
	}

	// \r 与 \n 各自折叠成一个空格。
	if got != "坏  昵称 X(abcdef12)" {
		t.Fatalf("Label=%q want %q", got, "坏  昵称 X(abcdef12)")
	}

	long := strings.Repeat("长", 100)
	if got := Label("abcdef1234567890", long); len(got) > nickLabelMaxWidth+len("(abcdef12)") {
		t.Fatalf("Label 未截断，len=%d", len(got))
	}
}

// TestSanitizeLineDropsEscapes 行净化必须覆盖 ANSI 转义与 Unicode 行分隔符：
// 只折叠 \r\n\t 时，昵称里的 ESC 序列可以清屏/伪造提示，U+2028 在部分查看器里
// 同样按换行渲染。
func TestSanitizeLineDropsEscapes(t *testing.T) {
	t.Parallel()

	got := SanitizeLine("a\x1b[2Jb\x07c\u2028d\u2029e\x7ff")

	for _, bad := range []string{"\x1b", "\x07", "\u2028", "\u2029", "\x7f"} {
		if strings.Contains(got, bad) {
			t.Fatalf("净化后仍含 %q: %q", bad, got)
		}
	}

	if !strings.HasPrefix(got, "a [2Jb c d e f") {
		t.Fatalf("净化结果=%q", got)
	}
}

// TestUID8RuneSafe 非 ASCII uid 不得切碎多字节 rune（日志行出现非法 UTF-8）。
func TestUID8RuneSafe(t *testing.T) {
	t.Parallel()

	got := UID8("用户标识超过八字节")
	if !utf8.ValidString(got) {
		t.Fatalf("UID8=%q 不是合法 UTF-8", got)
	}

	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("UID8=%q 含替换字符", got)
	}
}
