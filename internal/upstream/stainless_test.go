package upstream

import (
	"net/http"
	"strings"
	"testing"
)

// TestChatStainlessHeaders CLI chat 面注入七个 x-stainless-* 头：头名必须是原始
// 小写键（Header.Set 会规范化成 X-Stainless-*，与官方字节不符），值整组取自 profile。
func TestChatStainlessHeaders(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()
	profile := client.profileFor(account)

	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	client.ChatHeaders(req, account, "", ChatMeta{})

	want := map[string]string{
		"x-stainless-arch":            profile.Emit.Stainless.Arch,
		"x-stainless-lang":            profile.Emit.Stainless.Lang,
		"x-stainless-os":              profile.Emit.Stainless.OS,
		"x-stainless-package-version": profile.Emit.Stainless.PackageVersion,
		"x-stainless-retry-count":     profile.Emit.Stainless.RetryCount,
		"x-stainless-runtime":         profile.Emit.Stainless.Runtime,
		"x-stainless-runtime-version": profile.Emit.Stainless.RuntimeVersion,
	}

	for name, value := range want {
		// 直接读原始 map 键：Get 会做规范化，无法证明线上头名大小写。
		got, ok := req.Header[name]
		if !ok {
			t.Errorf("%s 缺失", name)

			continue
		}

		if len(got) != 1 || got[0] != value {
			t.Errorf("%s=%v want [%q]", name, got, value)
		}
	}

	// 不得出现规范化后的等价键（否则线上会多出一组 X-Stainless-*）。
	for name := range req.Header {
		if name != strings.ToLower(name) && stringsHasStainlessPrefix(name) {
			t.Errorf("出现非小写 stainless 键 %q", name)
		}
	}

	if got := len(req.Header); got != 39 {
		t.Errorf("chat 头总数=%d want 39（含七个 stainless）", got)
	}
}

// TestStainlessHeadersOnlyOnCLIChat 其余 chat 族（agenttool 四路）与轻头族都不发
// x-stainless-*：官方只有 OpenAI SDK 的 chat 请求带这组头。
func TestStainlessHeadersOnlyOnCLIChat(t *testing.T) {
	t.Parallel()

	client := newProfileClient(t)
	account := surfaceGoldenAccount()

	surfaces := []struct {
		name string
		call func(req *http.Request)
	}{
		{"search", func(req *http.Request) { client.applyHeaders(surfaceSearch, req, account, "", ChatMeta{}) }},
		{"webfetch", func(req *http.Request) { client.applyHeaders(surfaceWebFetch, req, account, "", ChatMeta{}) }},
		{"agentic_search", func(req *http.Request) {
			client.applyHeaders(surfaceAgenticSearch, req, account, "", ChatMeta{})
		}},
		{"images", func(req *http.Request) { client.applyHeaders(surfaceImages, req, account, "", ChatMeta{}) }},
		{"refresh", func(req *http.Request) { client.RefreshHeaders(req, account) }},
		{"billing", func(req *http.Request) { client.BillingHeaders(req, account) }},
	}

	for _, testCase := range surfaces {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			testCase.call(req)

			for name := range req.Header {
				if stringsHasStainlessPrefix(name) {
					t.Errorf("%s 不应发 %s", testCase.name, name)
				}
			}
		})
	}
}

// TestStainlessHeadersWithoutProfile 无 profile 时整族不发且不 panic（单元测试
// 直接 New() 的场景）。
func TestStainlessHeadersWithoutProfile(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.cn/v2/chat/completions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	New().ChatHeaders(req, surfaceGoldenAccount(), "", ChatMeta{})

	for name := range req.Header {
		if stringsHasStainlessPrefix(name) {
			t.Errorf("无 profile 时不应发 %s", name)
		}
	}
}

// stringsHasStainlessPrefix 大小写无关地判断是否 x-stainless-* 键。
func stringsHasStainlessPrefix(name string) bool {
	return len(name) >= len("x-stainless-") && strings.ToLower(name[:len("x-stainless-")]) == "x-stainless-"
}
