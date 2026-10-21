// cross_domain_test.go 钉住裸模型名跨域回退（pool.cross_domain_fallback）：
// 默认关闭零回归、开启后可按健康度换域、11102 跨域重取号一次、
// 显式域前缀与命名密钥绑定永不回退。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// crossDomainFixture 「一个 cn 号 + 一个 global 号」的池 + 按账号计数的假上游。
type crossDomainFixture struct {
	pool   *pool.Pool
	mu     sync.Mutex
	counts map[string]int
}

func (f *crossDomainFixture) calls(authz string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.counts[authz]
}

// newCrossDomainFixture 造 fixture。cnOnlyGlobalAccount=true 时池里只放 global 号
// （用来验证「本域无号 → 换域」）。
func newCrossDomainFixture(t *testing.T, globalOnly bool, respond func(authz string) (int, string, bool)) (*crossDomainFixture, *upstream.Client) {
	t.Helper()

	// global realm 路由需要 auth 侧进程级开关开启（默认开启，测试显式保证）。
	auth.SetGlobalEnabled(true)

	cnAcct := &auth.Auth{UID: "cn1", AccessToken: "at-cn", ExpiresAt: 9999999999}

	global := &auth.Auth{UID: "g1", AccessToken: "at-global", ExpiresAt: 9999999999}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatalf("backfill realm: %v", err)
	}

	state := pool.New("")

	state.SetRandomSource(func(int64) int64 { return 0 })

	accounts := []*auth.Auth{cnAcct, global}
	if globalOnly {
		accounts = []*auth.Auth{global}
	}

	for _, acct := range accounts {
		state.Add(acct)
		state.SetCredits(acct.UID, 1000, 0)
	}

	fixture := &crossDomainFixture{pool: state, counts: map[string]int{}}

	client := newFakeUpstream(t, func(authz string) (int, string, bool) {
		fixture.mu.Lock()
		fixture.counts[authz]++
		fixture.mu.Unlock()

		return respond(authz)
	})
	// 两域都指向假域，避免测试触网。
	client.ChatBaseGlobal = "https://fake.example"
	client.BillingBaseGlobal = "https://fake.example"
	client.GlobalEnabled = true

	return fixture, client
}

// TestCrossDomainFallbackDefaultOff 默认关闭：本域无号时**不**换域（零回归）。
func TestCrossDomainFallbackDefaultOff(t *testing.T) {
	t.Parallel()

	fixture, client := newCrossDomainFixture(t, true, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	handler := NewHandler(&Config{Pool: fixture.pool, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("默认关闭时不应换域成功，code=%d body=%s", rec.Code, rec.Body)
	}

	if got := fixture.calls("Bearer at-global"); got != 0 {
		t.Errorf("默认关闭时不得请求 global 号，calls=%d", got)
	}
}

// TestCrossDomainFallbackEnabled 开启后：CN 域无号 → 换到 global 域成功。
func TestCrossDomainFallbackEnabled(t *testing.T) {
	t.Parallel()

	fixture, client := newCrossDomainFixture(t, true, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	fixture.pool.SetCrossDomainFallback(true)
	handler := NewHandler(&Config{Pool: fixture.pool, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("开启后应跨域成功，code=%d body=%s", rec.Code, rec.Body)
	}

	if got := fixture.calls("Bearer at-global"); got != 1 {
		t.Errorf("应请求 global 号一次，calls=%d", got)
	}
}

// TestCrossDomainFallbackExplicitPrefixNoFallback 显式域前缀永不回退。
func TestCrossDomainFallbackExplicitPrefixNoFallback(t *testing.T) {
	t.Parallel()

	fixture, client := newCrossDomainFixture(t, true, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	fixture.pool.SetCrossDomainFallback(true)
	handler := NewHandler(&Config{Pool: fixture.pool, Upstream: client})

	// 显式点 cn 域：即便池里只有 global 号也不得回退。
	rec := postChat(t, handler, `{"model":"cn:glm-5.2","messages":[]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("显式前缀不得跨域回退，code=%d body=%s", rec.Code, rec.Body)
	}

	if got := fixture.calls("Bearer at-global"); got != 0 {
		t.Errorf("显式前缀不得请求 global 号，calls=%d", got)
	}
}

// TestCrossDomainFallbackNamedKeyBindingNoFallback 命名密钥绑定出口时视为显式指定域。
func TestCrossDomainFallbackNamedKeyBindingNoFallback(t *testing.T) {
	t.Parallel()

	fixture, client := newCrossDomainFixture(t, true, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})
	fixture.pool.SetCrossDomainFallback(true)

	store := apikey.New("")

	_, raw, err := store.Create("cn-key")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if _, err := store.SetRealm(store.List()[0].ID, "cn"); err != nil {
		t.Fatalf("set realm: %v", err)
	}

	handler := NewHandler(&Config{Pool: fixture.pool, Upstream: client, APIKey: "secret", Keys: store})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+raw)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("key 绑定 cn 出口时不得跨域回退，code=%d body=%s", rec.Code, rec.Body)
	}

	if got := fixture.calls("Bearer at-global"); got != 0 {
		t.Errorf("key 绑定出口时不得请求 global 号，calls=%d", got)
	}
}

// TestCrossDomainFallbackOnModelNotFound 11102（本域目录无此模型）→ 跨域重取号一次。
func TestCrossDomainFallbackOnModelNotFound(t *testing.T) {
	t.Parallel()

	fixture, client := newCrossDomainFixture(t, false, func(authz string) (int, string, bool) {
		if authz == "Bearer at-cn" {
			return http.StatusNotFound, `{"code":11102,"msg":"model [glm-5.2] service info not found"}`, false
		}

		return http.StatusOK, sseOK, true
	})
	fixture.pool.SetCrossDomainFallback(true)
	handler := NewHandler(&Config{Pool: fixture.pool, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("11102 后应跨域重取号成功，code=%d body=%s", rec.Code, rec.Body)
	}

	if cn, global := fixture.calls("Bearer at-cn"), fixture.calls("Bearer at-global"); cn != 1 || global != 1 {
		t.Errorf("应各请求一次（cn 目录缺模型 → global 成功），cn=%d global=%d", cn, global)
	}
}

// TestCrossDomainFallbackModelNotFoundOnce 11102 只允许跨域一次：两域都没有时
// 仍然 404（不得无限跨域）。
func TestCrossDomainFallbackModelNotFoundOnce(t *testing.T) {
	t.Parallel()

	fixture, client := newCrossDomainFixture(t, false, func(string) (int, string, bool) {
		return http.StatusNotFound, `{"code":11102,"msg":"model [glm-5.2] service info not found"}`, false
	})
	fixture.pool.SetCrossDomainFallback(true)
	handler := NewHandler(&Config{Pool: fixture.pool, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("两域都无此模型应回 404，code=%d body=%s", rec.Code, rec.Body)
	}

	if cn, global := fixture.calls("Bearer at-cn"), fixture.calls("Bearer at-global"); cn != 1 || global != 1 {
		t.Errorf("每域只应请求一次，cn=%d global=%d", cn, global)
	}
}

// TestCrossDomainFallbackModelNotFoundNoSameRealmRetry 11102 切到无号的目标域后，
// 不得再跳回原域对同一个 11102 重试一次（「跨域仅一次」）。
func TestCrossDomainFallbackModelNotFoundNoSameRealmRetry(t *testing.T) {
	t.Parallel()

	fixture, client := newCrossDomainFixture(t, false, func(string) (int, string, bool) {
		return http.StatusNotFound, `{"code":11102,"msg":"model [glm-5.2] service info not found"}`, false
	})
	// 只留 CN 号：切到 global 后无号可服务。
	fixture.pool.Remove("g1")
	fixture.pool.SetCrossDomainFallback(true)
	handler := NewHandler(&Config{Pool: fixture.pool, Upstream: client})

	rec := postChat(t, handler, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("两域都无此模型应回 404，code=%d body=%s", rec.Code, rec.Body)
	}

	if cn, global := fixture.calls("Bearer at-cn"), fixture.calls("Bearer at-global"); cn != 1 || global != 0 {
		t.Errorf("原域不得因跨域失败被重试，cn=%d global=%d", cn, global)
	}
}
