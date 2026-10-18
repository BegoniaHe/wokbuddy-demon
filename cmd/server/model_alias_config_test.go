package main

import "testing"

func TestModelAliasesConfig(t *testing.T) {
	t.Parallel()

	config := Default()

	config.Models.Aliases = map[string]string{
		" claude-sonnet-4-5 ": " cn:hy4 ",
		"gpt-5-codex":         "hy4",
	}
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if got := config.Models.Aliases["claude-sonnet-4-5"]; got != "cn:hy4" {
		t.Fatalf("trimmed alias = %q, want cn:hy4", got)
	}

	if got := config.Models.Aliases["gpt-5-codex"]; got != "hy4" {
		t.Fatalf("alias = %q, want hy4", got)
	}
}

func TestModelAliasesConfigRejectsInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		aliases map[string]string
	}{
		{"空别名", map[string]string{" ": "cn:hy4"}},
		{"别名含 realm 前缀", map[string]string{"cn:claude": "hy4"}},
		{"空目标", map[string]string{"claude": " "}},
		{"目标缺裸名", map[string]string{"claude": "cn:"}},
		{"目标 realm 前缀未知", map[string]string{"claude": "other:hy4"}},
		{"目标 realm 前缀为空", map[string]string{"claude": ":hy4"}},
		{"去空白后重名", map[string]string{"claude": "hy4", " claude ": "hy5"}},
	}
	for _, tc := range cases {
		c := Default()

		c.Models.Aliases = tc.aliases
		if err := c.normalize(); err == nil {
			t.Errorf("%s: normalize 应报错", tc.name)
		}
	}
}
