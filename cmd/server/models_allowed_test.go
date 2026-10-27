package main

import (
	"slices"
	"strings"
	"testing"
)

// TestModelsAllowedNormalize models.allowed 归一：去空白/丢空项/去重，大小写敏感，
// 前缀后为空拒启动且错误信息带字段名（与 models.hidden 同语法）。
func TestModelsAllowedNormalize(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfig([]byte(`{"listen":":7863","models":{"allowed":["  glm-5.2  ","","glm-5.2","global:kimi-k3"]}}`))
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"glm-5.2", "global:kimi-k3"}; !slices.Equal(cfg.Models.Allowed, want) {
		t.Fatalf("allowed=%v want %v", cfg.Models.Allowed, want)
	}

	if _, err := ParseConfig([]byte(`{"listen":":7863","models":{"allowed":["cn:"]}}`)); err == nil {
		t.Fatal("cn: 后为空应拒绝启动")
	} else if !strings.Contains(err.Error(), "models.allowed") {
		t.Errorf("错误信息应含 fields 名 models.allowed，got %v", err)
	}
}

// TestModelsAllowedDefaultsOff 空白名单保持 nil = 不启用（行为与未加该字段一致）。
func TestModelsAllowedDefaultsOff(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if cfg.Models.Allowed != nil {
		t.Fatalf("默认 allowed=%v want nil", cfg.Models.Allowed)
	}

	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}

	if cfg.Models.Allowed != nil {
		t.Fatalf("归一后 allowed=%v want nil", cfg.Models.Allowed)
	}
}

// TestModelsAllowedBackgroundTarget 后台改写目标必须在白名单内：裸目标按 cn 域解析，
// 裸名条目展开两域可覆盖；只放行另一域的同一裸名仍拒绝。
func TestModelsAllowedBackgroundTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		allowed string
		wantErr bool
	}{
		{"裸名覆盖两域", `["deepseek-v4.1-flash"]`, false},
		{"cn 前缀覆盖裸目标", `["cn:deepseek-v4.1-flash","cn:glm-5.2"]`, false},
		{"目标不在名单", `["cn:glm-5.2"]`, true},
		{"只放行 global 同名模型", `["global:deepseek-v4.1-flash"]`, true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			body := `{"listen":":7863","models":{"background_names":["memory-gen"],"allowed":` + testCase.allowed + `}}`
			_, err := ParseConfig([]byte(body))

			if !testCase.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatal("后台目标未被白名单覆盖时应拒绝启动")
			}

			for _, want := range []string{"models.allowed", "background_target"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息应含 %q，got %v", want, err)
				}
			}
		})
	}

	// 后台改写关闭（background_names 为空）时不要求覆盖默认目标。
	if _, err := ParseConfig([]byte(`{"listen":":7863","models":{"allowed":["cn:glm-5.2"]}}`)); err != nil {
		t.Fatalf("未开启后台改写时不应校验目标: %v", err)
	}
}
