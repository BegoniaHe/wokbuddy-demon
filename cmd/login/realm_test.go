package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestParseRealmArgs 覆盖 --realm 参数解析：缺省 cn、等号/分离式写法、
// 非法值/缺值报错、大小写归一、剥离 flag 后剩余参数保持相对顺序。
func TestParseRealmArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		args      []string
		wantRealm string
		wantRest  []string
		wantErr   bool
	}{
		{name: "缺省 cn", args: []string{"url"}, wantRealm: "cn", wantRest: []string{"url"}},
		{name: "等号 global 在前", args: []string{"--realm=global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "等号 cn 在后", args: []string{"poll", "--realm=cn"}, wantRealm: "cn", wantRest: []string{"poll"}},
		{name: "分离式 global", args: []string{"--realm", "global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "非法值报错", args: []string{"--realm=foo", "url"}, wantErr: true},
		{name: "分离式缺值报错", args: []string{"--realm", "url"}, wantErr: true},
		{name: "大小写归一", args: []string{"--realm=GLOBAL", "url"}, wantRealm: "global", wantRest: []string{"url"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			realm, rest, err := parseRealmArgs(item.args)
			if item.wantErr {
				if err == nil {
					t.Fatalf("parseRealmArgs(%v) err=nil, want error", item.args)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseRealmArgs(%v) err=%v", item.args, err)
			}

			if realm != item.wantRealm {
				t.Errorf("realm=%q want %q", realm, item.wantRealm)
			}

			if !reflect.DeepEqual(rest, item.wantRest) {
				t.Errorf("rest=%v want %v", rest, item.wantRest)
			}
		})
	}
}

// TestResolveRealmInput 覆盖交互式选域的输入→realm 映射（login.sh 交互分支的核心决策）：
// "1"/"cn"/""（回车默认）→ cn；"2"/"global" → global；大小写不敏感；非法 → ("",false)。
func TestResolveRealmInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{input: "1", want: "cn"},
		{input: "cn", want: "cn"},
		{input: "CN", want: "cn"},
		{input: "", want: "cn"}, // 回车默认
		{input: "2", want: "global"},
		{input: "global", want: "global"},
		{input: "GLOBAL", want: "global"},
	}
	for _, item := range cases {
		got, ok := resolveRealmInput(item.input)
		if !ok {
			t.Errorf("resolveRealmInput(%q) ok=false want true", item.input)
			continue
		}

		if got != item.want {
			t.Errorf("resolveRealmInput(%q)=%q want %q", item.input, got, item.want)
		}
	}
	// 非法输入 → (false)。
	for _, bad := range []string{"3", "cnn", "globalx", "foo"} {
		if got, ok := resolveRealmInput(bad); ok {
			t.Errorf("resolveRealmInput(%q)=(%q,true) want (_,false)", bad, got)
		}
	}
}

// TestPromptRealm 覆盖 promptRealm 的 I/O 行为（out 将接 os.Stderr，stdout 只出 realm）：
//
//	1 / 2 / 回车默认 → 分别输出来 cn / global / cn；均打印"选择登录版本"提示；
//	非法输入 → 警告并回落 cn；EOF（非交互直接管道）→ 回落 cn。
func TestPromptRealm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		input    string
		want     string
		wantHint bool // 输出中出现"选择登录版本"提示
	}{
		{name: "选 cn", input: "1\n", want: "cn", wantHint: true},
		{name: "选 global", input: "2\n", want: "global", wantHint: true},
		{name: "回车默认 cn", input: "\n", want: "cn", wantHint: true},
		{name: "非法回落 cn", input: "foo\n", want: "cn", wantHint: true},
		{name: "EOF 回落 cn", input: "", want: "cn", wantHint: true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			got := promptRealm(strings.NewReader(item.input), &out)
			if got != item.want {
				t.Errorf("promptRealm(%q)=%q want %q", item.input, got, item.want)
			}

			if item.wantHint && !strings.Contains(out.String(), "选择登录版本") {
				t.Errorf("prompt should contain '选择登录版本', got %q", out.String())
			}
		})
	}
}

// TestRealmSubcommandSelection 通过命令形式验证 realm 子命令（login.sh 交互分支调用）：
// 子命令剥离 --realm，剩余参数为首个 "realm"。
func TestRealmSubcommandSelection(t *testing.T) {
	t.Parallel()

	realm, rest, err := parseRealmArgs([]string{"realm"})
	if err != nil {
		t.Fatalf("parseRealmArgs(realm) err=%v", err)
	}

	if realm != "cn" {
		t.Errorf("realm default=%q want cn", realm)
	}

	if len(rest) != 1 || rest[0] != "realm" {
		t.Errorf("rest=%v want [realm]", rest)
	}
}

// TestBuildLoginOutputRealmAlwaysSet 登录产物（login.sh 据此落盘 auth 文件）恒含 realm 键：
// 显式 --realm 优先，缺省时按 domain 推断（echo 出的 global 账号即便未显式指定也带 global）。
// 这是「登录落盘永远带 realm 标识」契约的测试载体。
func TestBuildLoginOutputRealmAlwaysSet(t *testing.T) {
	t.Parallel()

	tok := struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600, Domain: "www.codebuddy.cn"}
	acct := struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}{UID: "u1", Nickname: "n1"}

	cases := []struct {
		name      string
		realm     string
		domain    string
		wantRealm string
	}{
		{name: "显式 global 优先", realm: "global", domain: "www.codebuddy.cn", wantRealm: "global"},
		{name: "显式 cn 优先", realm: "cn", domain: "www.workbuddy.ai", wantRealm: "cn"},
		{name: "缺省按 domain 推断 global", realm: "", domain: "www.workbuddy.ai", wantRealm: "global"},
		{name: "缺省空 domain 回落 cn", realm: "", domain: "", wantRealm: "cn"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			tokCopy := tok
			tokCopy.Domain = item.domain
			out := buildLoginOutput(tokCopy, item.realm, acct)

			m, ok := out["realm"].(string)
			if !ok {
				t.Fatalf("missing realm key in login output: %v", out)
			}

			if m != item.wantRealm {
				t.Errorf("realm=%q want %q", m, item.wantRealm)
			}
		})
	}
}
