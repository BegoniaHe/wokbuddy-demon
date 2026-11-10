// session_derive_binding_test.go 钉住「只发 user_id」客户端的内容派生账号粘性
// （issue #481）：键必须来自请求内容而非 user_id，跨轮稳定、不同对话不碰撞。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/session"
)

// userIDOnlyBody 构造一条只带 metadata.user_id 的多轮请求（不带任何会话标识）。
func userIDOnlyBody(first, last string) map[string]any {
	return map[string]any{
		"model":    "glm-5.2",
		"metadata": map[string]any{"user_id": "u1"},
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": first},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": last},
		},
	}
}

// marshalBody 把请求体 map 序列化成字符串（测试内联用）。
func marshalBody(t *testing.T, body map[string]any) string {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(raw)
}

// newUserIDStickyHandler 构造双号 + 空绑定的粘性路由，deriveBinding 控制开关。
func newUserIDStickyHandler(t *testing.T, deriveBinding bool) (*Handler, *bindStore) {
	t.Helper()

	binds := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     binds,
		Available: func() []string { return []string{"u1", "u2"} },
	})
	state := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	client := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusOK, sseOK, true
	})

	return NewHandler(&Config{
		Pool: state, Upstream: client, Session: sess,
		StickyDeriveBinding: deriveBinding,
	}), binds
}

// TestUserIDClientDerivesStickyBinding 只发 user_id 的客户端改用内容派生键做绑定：
// 同一段对话跨轮命中同一键，不同对话（同一 user_id）各自成键——不复活 user 维度粘性。
func TestUserIDClientDerivesStickyBinding(t *testing.T) {
	t.Parallel()
	handler, binds := newUserIDStickyHandler(t, true)

	send := func(first, last string) {
		t.Helper()

		rec := postChat(t, handler, marshalBody(t, userIDOnlyBody(first, last)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}

	send("first", "second") // 会话 A 第一轮
	assertContentBindings(t, binds, 1, true)

	send("first", "third") // 会话 A 下一轮：历史递增，键不变
	assertContentBindings(t, binds, 1, false)

	send("other", "second") // 同一 user_id 的另一段对话 → 另一个键
	assertContentBindings(t, binds, 2, true)
}

// assertContentBindings 断言绑定条数，并在 checkFresh 时校验键前缀与绑定号
// （首轮/新对话需要校验；同会话下一轮只关心「没有多出新键」）。
func assertContentBindings(t *testing.T, binds *bindStore, wantCount int, checkFresh bool) {
	t.Helper()

	got := binds.LoadBinds()
	if len(got) != wantCount {
		t.Fatalf("binds=%v want %d entries", got, wantCount)
	}

	if !checkFresh {
		return
	}

	for key, uid := range got {
		// 命名空间前缀：内容派生键不能与客户端显式 sessKey 混用同一个空间。
		if !strings.HasPrefix(key, "c-") {
			t.Errorf("binding key=%q want content-derived namespace prefix \"c-\"", key)
		}

		if uid != "u1" && uid != "u2" {
			t.Errorf("binding %q -> %q want a pool uid", key, uid)
		}
	}
}

// TestUserIDClientDeriveBindingCanBeDisabled 开关关闭时回退旧行为：无会话键即不绑定。
func TestUserIDClientDeriveBindingCanBeDisabled(t *testing.T) {
	t.Parallel()
	handler, binds := newUserIDStickyHandler(t, false)

	rec := postChat(t, handler, marshalBody(t, userIDOnlyBody("first", "second")))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	if got := binds.LoadBinds(); len(got) != 0 {
		t.Errorf("derive_binding=false must not bind, binds=%v", got)
	}
}
