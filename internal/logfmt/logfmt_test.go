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
