package main

import (
	"testing"
	"unicode/utf8"
)

// TestTruncKeepsValidUTF8 截断不得把多字节字符切成半个 rune。
func TestTruncKeepsValidUTF8(t *testing.T) {
	t.Parallel()

	// 4 个汉字 = 12 字节；截到 11 字节必须回退到第 3 个字符末尾（9 字节）。
	got := trunc("测试昵称", 11)
	if got != "测试昵" {
		t.Fatalf("trunc=%q want %q", got, "测试昵")
	}

	if !utf8.ValidString(got) {
		t.Fatalf("trunc 结果不是合法 UTF-8: %q", got)
	}

	if got := trunc("uid-123", 36); got != "uid-123" {
		t.Fatalf("未超长时应原样返回: %q", got)
	}
}
