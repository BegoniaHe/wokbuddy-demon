package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}

	return r
}

func TestVerifyBearer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		{"前缀大小写不符（规范要求精确）", "sk-abc123", "bearer sk-abc123", false},
		{"多余空格", "sk-abc123", "Bearer  sk-abc123", false},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	t.Parallel()

	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

// TestVerifyBearerAcceptXAPIKey Anthropic SDK 的 api_key 模式只发 x-api-key，
// 与 Bearer 同口径比较；Authorization 优先。
func TestVerifyBearerAcceptXAPIKey(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("X-Api-Key", "sk-anthropic")

	if !VerifyBearer(r, "sk-anthropic") {
		t.Error("x-api-key 正确密钥应通过")
	}

	r.Header.Set("X-Api-Key", "sk-wrong")

	if VerifyBearer(r, "sk-anthropic") {
		t.Error("x-api-key 错误密钥不应通过")
	}

	r.Header.Set("X-Api-Key", "sk-anthropic")
	r.Header.Set("Authorization", "Bearer sk-bearer")

	if !VerifyBearer(r, "sk-bearer") {
		t.Error("Authorization 应优先于 x-api-key")
	}

	gemini := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent", nil)
	gemini.Header.Set("X-Goog-Api-Key", "sk-gemini")

	if !VerifyBearer(gemini, "sk-gemini") {
		t.Error("x-goog-api-key 正确密钥应通过")
	}
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()

	keys := []Key{
		{ID: "k1", Hash: HashKey("sk-wb-first")},
		{ID: "k2", Hash: HashKey("sk-wb-second")},
	}

	cases := []struct {
		name       string
		defaultKey string
		keys       []Key
		authz      string
		wantOK     bool
		wantID     string
		wantDef    bool
	}{
		{"未启用鉴权（默认空+无命名）放行", "", nil, "", true, "", true},
		{"默认密钥命中", "sk-abc", nil, "Bearer sk-abc", true, "", true},
		{"默认密钥错误", "sk-abc", nil, "Bearer sk-nope", false, "", false},
		{"命名密钥命中第二个", "sk-abc", keys, "Bearer sk-wb-second", true, "k2", false},
		{"命名密钥命中第一个", "sk-abc", keys, "Bearer sk-wb-first", true, "k1", false},
		{"命名密钥错误", "sk-abc", keys, "Bearer sk-wb-nope", false, "", false},
		{"无默认密钥仅命名密钥：命中", "", keys, "Bearer sk-wb-first", true, "k1", false},
		{"无默认密钥仅命名密钥：缺头拒绝", "", keys, "", false, "", false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			got := Authenticate(req(item.authz), item.defaultKey, item.keys)
			if got.OK != item.wantOK || got.KeyID != item.wantID || got.IsDefault != item.wantDef {
				t.Errorf("Authenticate(authz=%q, default=%q) = %+v, want {OK:%v KeyID:%q IsDefault:%v}",
					item.authz, item.defaultKey, got, item.wantOK, item.wantID, item.wantDef)
			}
		})
	}
}

// TestAuthenticateMatchesFirstDuplicate 多个候选摘要相同时返回首个命中，且不因
// 命中而提前返回（实现层保证；此处断言稳定的首命中语义）。
func TestAuthenticateMatchesFirstDuplicate(t *testing.T) {
	t.Parallel()

	h := HashKey("sk-wb-dup")
	keys := []Key{{ID: "first", Hash: h}, {ID: "second", Hash: h}}

	got := Authenticate(req("Bearer sk-wb-dup"), "", keys)
	if !got.OK || got.KeyID != "first" {
		t.Errorf("duplicate hashes should match first, got %+v", got)
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	t.Parallel()
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}

	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}
