package main

import (
	"encoding/json"
	"testing"
)

// authFile 必须同时兼容嵌套形（插件 OAuth）与扁平形（手写/旧版），与
// internal/auth.Parse 的服务端口径一致；此前只认嵌套形，扁平账号会报
// "no accessToken"。
func TestAuthFileSupportsNested(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"auth": {"accessToken": "nested-token", "domain": "www.workbuddy.ai", "realm": "global"},
		"account": {"uid": "nested-uid", "enterpriseId": "nested-eid", "nickname": "嵌套"}
	}`)

	var file authFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	assertAuthFile(t, &file, "nested-token", "nested-uid", "nested-eid", "嵌套", "www.workbuddy.ai")

	if got := billingBaseFor(&file); got != billingBaseGlobal {
		t.Errorf("nested global billing base=%q want %q", got, billingBaseGlobal)
	}
}

func TestAuthFileSupportsFlat(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"accessToken": "flat-token", "domain": "www.workbuddy.ai", "realm": "global",
		"uid": "flat-uid", "enterpriseId": "flat-eid", "nickname": "扁平"
	}`)

	var file authFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	assertAuthFile(t, &file, "flat-token", "flat-uid", "flat-eid", "扁平", "www.workbuddy.ai")

	if got := billingBaseFor(&file); got != billingBaseGlobal {
		t.Errorf("flat global billing base=%q want %q", got, billingBaseGlobal)
	}
}

func assertAuthFile(t *testing.T, file *authFile, token, uid, enterpriseID, nickname, domain string) {
	t.Helper()

	if got := file.accessToken(); got != token {
		t.Errorf("accessToken=%q want %q", got, token)
	}

	if got := file.uid(); got != uid {
		t.Errorf("uid=%q want %q", got, uid)
	}

	if got := file.enterpriseID(); got != enterpriseID {
		t.Errorf("enterpriseId=%q want %q", got, enterpriseID)
	}

	if got := file.nickname(); got != nickname {
		t.Errorf("nickname=%q want %q", got, nickname)
	}

	if got := file.domain(); got != domain {
		t.Errorf("domain=%q want %q", got, domain)
	}
}

// 嵌套字段非空时优先于扁平字段，避免混合形态下被扁平值覆盖。
func TestAuthFileNestedWinsOverFlat(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"auth": {"accessToken": "nested-token"},
		"account": {"uid": "nested-uid"},
		"accessToken": "flat-token", "uid": "flat-uid"
	}`)

	var file authFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	if got := file.accessToken(); got != "nested-token" {
		t.Errorf("accessToken=%q want nested-token", got)
	}

	if got := file.uid(); got != "nested-uid" {
		t.Errorf("uid=%q want nested-uid", got)
	}
}

// 显式 realm=cn 与 global domain 冲突时按 domain 判 global：服务端 Realm() 也是
// domain 优先，CLI 若短路到 CN 会拿 401。
func TestBillingBaseDomainWinsOverExplicitCN(t *testing.T) {
	t.Parallel()

	for _, domain := range []string{"www.workbuddy.ai", "www.codebuddy.ai"} {
		raw := []byte(`{"auth":{"accessToken":"token","domain":"` + domain + `","realm":"cn"}}`)

		var file authFile
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}

		if got := billingBaseFor(&file); got != billingBaseGlobal {
			t.Errorf("domain=%s billingBaseFor=%q want %q", domain, got, billingBaseGlobal)
		}
	}
}

// 官方 CLI 国际站的 codebuddy.ai 家族与 workbuddy.ai 同判 global；
// codebuddy.cn 仍走 CN，避免余额查询打错计费域。
func TestBillingBaseCodebuddyAIDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		domain string
		want   string
	}{
		{"www.codebuddy.ai", billingBaseGlobal},
		{"codebuddy.ai", billingBaseGlobal},
		{"sub.codebuddy.ai", billingBaseGlobal},
		{"  WWW.CodeBuddy.AI  ", billingBaseGlobal},
		{"www.codebuddy.cn", billingBaseCN},
		{"codebuddy.cn", billingBaseCN},
		{"", billingBaseCN},
	}
	for _, item := range cases {
		raw := []byte(`{"auth":{"accessToken":"token","domain":"` + item.domain + `"}}`)

		var file authFile
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}

		if got := billingBaseFor(&file); got != item.want {
			t.Errorf("domain=%q billingBaseFor=%q want %q", item.domain, got, item.want)
		}
	}
}
