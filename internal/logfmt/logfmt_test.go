package logfmt

import "testing"

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
