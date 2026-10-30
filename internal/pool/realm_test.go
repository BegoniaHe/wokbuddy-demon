package pool

import (
	"reflect"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// realmPool 构造一个含 cn/global 账号的池，并确保 globalEnabled 开关开启（缺省）。
func realmPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)

	pool := New("")
	// CN 账号（显式 cn realm 或空 realm+cn domain 均可）→ Realm()=="cn"
	pool.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	pool.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn
	// global 账号 → Realm()=="global"
	pool.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	pool.Add(&auth.Auth{UID: "g2", Domain: "workbuddy.ai"})

	return pool
}

// realmOf 直接读账号 realm（等价 e.a.Realm()）。
func realmOf(a *auth.Auth) string { return a.Realm() }

func TestAvailableUIDsForRealm(t *testing.T) {
	t.Parallel()
	pool := realmPool(t)

	// 全部 healthy → cn 集合只有 cn1/cn2，global 集合只有 g1/g2。
	got := pool.AvailableUIDsForRealm("cn")

	want := []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(cn)=%v want %v", got, want)
	}

	got = pool.AvailableUIDsForRealm("global")

	want = []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(global)=%v want %v", got, want)
	}

	// realm=="" 退化为现状（等价 AvailableUIDs：全部 healthy）。
	got = pool.AvailableUIDsForRealm("")

	want = []string{"cn1", "cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm()=%v want %v", got, want)
	}
}

func TestAvailableUIDsForModelRealm(t *testing.T) {
	t.Parallel()
	pool := realmPool(t)
	// 6004 模型冷却只落在 cn-1 上 → AvailableUIDsForModelRealm("glm-5.2","cn") 排除它，
	// 但 cn 集合至少还保底 cn-2；global 集合不受 cn 冷却影响。
	pool.CooldownSoftForModel("cn1", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "model 6004")

	got := pool.AvailableUIDsForModelRealm("glm-5.2", "cn")
	if len(got) != 1 || got[0] != "cn2" {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,cn)=%v want [cn2]", got)
	}

	got = pool.AvailableUIDsForModelRealm("glm-5.2", "global")

	want := []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,global)=%v want %v", got, want)
	}
	// realm=="" → 不做 realm 过滤（模型豁免照常：cn1 仍被该模型冷却排除）。
	got = pool.AvailableUIDsForModelRealm("glm-5.2", "")

	want = []string{"cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2)=%v want %v", got, want)
	}
}

func TestWeightedAvailableUIDsForModelRealm(t *testing.T) {
	t.Parallel()
	pool := realmPool(t)
	now := time.Now()
	pool.SetCreditsDetailed("cn2", 100, 100, 100, now.Add(time.Hour), 100)

	got := pool.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")

	want := []string{"cn1", "cn2", "cn2", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weighted cn candidates=%v want %v", got, want)
	}

	// 在途满载账号不进入候选，快过期权重不得突破现有并发上限。
	pool.SetMaxInFlight(1)

	if !pool.Acquire("cn2") {
		t.Fatal("failed to acquire cn2")
	}

	got = pool.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want = []string{"cn1"}

	pool.Release("cn2")

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("in-flight-full weighted candidates=%v want %v", got, want)
	}

	// 关闭总开关后逐账号只出现一次，旧行为完全恢复。
	pool.SetPreferExpiring(false)
	got = pool.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")

	want = []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled weighted cn candidates=%v want %v", got, want)
	}
}

func TestPickExcludingForRealm(t *testing.T) {
	t.Parallel()
	pool := realmPool(t)
	// realm=cn → 只从 cn 集合选。
	seen := map[string]bool{}

	for range 50 {
		account2 := pool.PickExcludingForRealm(nil, "", "cn")
		if account2 == nil {
			t.Fatal("PickExcludingForRealm(cn) returned nil")
		}

		seen[account2.UID] = true
		if realmOf(account2) != "cn" {
			t.Fatalf("PickExcludingForRealm(cn) returned global account %s", account2.UID)
		}
	}
	// global 同样只从 global 集合。
	for range 50 {
		account := pool.PickExcludingForRealm(nil, "", "global")
		if account == nil {
			t.Fatal("PickExcludingForRealm(global) returned nil")
		}

		if realmOf(account) != "global" {
			t.Fatalf("PickExcludingForRealm(global) returned cn account %s", account.UID)
		}
	}
	// realm=="" → 退化现状：所有 healthy 都能选。
	for range 50 {
		a := pool.PickExcludingForRealm(nil, "", "")
		if a == nil {
			t.Fatal("PickExcludingForRealm() returned nil")
		}
	}
}

func TestPickExcludingForRealmTried(t *testing.T) {
	t.Parallel()
	p := realmPool(t)
	// tried 排除在 realm 过滤之后（维持请求级轮换语义）。
	account := p.PickExcludingForRealm(map[string]bool{"cn1": true}, "", "cn")
	if account == nil {
		t.Fatal("tried cn1 → nil")
	}

	if account.UID == "cn1" {
		t.Fatalf("tried cn1 still picked: %s", account.UID)
	}

	if realmOf(account) != "cn" {
		t.Fatalf("picked non-cn %s", account.UID)
	}
}

// TestServableForModelRealm 跨域回退挑域口径：只看「该域是否存在能服务该模型的号」，
// 口径取 excludeReason 全集并额外排除路由级熔断。
func TestServableForModelRealm(t *testing.T) {
	t.Parallel()

	t.Run("两域都有健康号", func(t *testing.T) {
		t.Parallel()

		pool := realmPool(t)
		if !pool.ServableForModelRealm("glm-5.2", "cn") || !pool.ServableForModelRealm("glm-5.2", "global") {
			t.Error("两域都应可服务")
		}
	})

	t.Run("cn 全冷却则 cn 不可服务", func(t *testing.T) {
		t.Parallel()

		pool := realmPool(t)
		for _, uid := range []string{"cn1", "cn2"} {
			pool.CooldownSoftRate(uid, time.Hour, time.Time{}, "429 rate limit")
		}

		if pool.ServableForModelRealm("glm-5.2", "cn") {
			t.Error("cn 域全冷却时不应可服务")
		}

		if !pool.ServableForModelRealm("glm-5.2", "global") {
			t.Error("global 域仍应可服务")
		}
	})

	t.Run("模型级负缓存只影响该模型", func(t *testing.T) {
		t.Parallel()

		pool := realmPool(t)
		pool.BlockModelBackoff("cn1", "glm-5.2", "11136 model not permitted")
		pool.BlockModelBackoff("cn2", "glm-5.2", "11136 model not permitted")

		if pool.ServableForModelRealm("glm-5.2", "cn") {
			t.Error("cn 域该模型全被负缓存时不应可服务")
		}

		if !pool.ServableForModelRealm("other-model", "cn") {
			t.Error("其他模型不受影响")
		}
	})

	t.Run("路由熔断排除该域", func(t *testing.T) {
		t.Parallel()

		pool := realmPool(t)
		for range 3 {
			pool.NoteRouteFailure("cn", "glm-5.2")
		}

		if pool.ServableForModelRealm("glm-5.2", "cn") {
			t.Error("路由熔断中该域不应可服务")
		}
	})
}

// TestCrossDomainFallbackFlag 开关读写（默认关闭）。
func TestCrossDomainFallbackFlag(t *testing.T) {
	t.Parallel()

	state := New("")

	if state.CrossDomainFallback() {
		t.Fatal("默认应关闭跨域回退")
	}

	state.SetCrossDomainFallback(true)

	if !state.CrossDomainFallback() {
		t.Fatal("SetCrossDomainFallback(true) 后应开启")
	}

	state.SetCrossDomainFallback(false)

	if state.CrossDomainFallback() {
		t.Fatal("SetCrossDomainFallback(false) 后应关闭")
	}
}
