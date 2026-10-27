package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/livecfg"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// newAllowedModelsHandler 构造带 CN 目录缓存与调用计数的 handler；allowed 为空 = 不启用。
func newAllowedModelsHandler(allowed map[string]struct{}, aliases map[string]string) (*Handler, *atomic.Int64) {
	var calls atomic.Int64

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)

			return jsonResponse(http.StatusOK, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	handler := NewHandler(&Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:      client,
		AllowedModels: allowed,
		ModelAliases:  aliases,
		// ContextWindow 直接给值：避免列表组装走 models.dev 异步目录拉取（包级状态，
		// 会干扰并行用例的「不触发上游请求」断言）。
		modelsCache: &modelsCache{
			ids: []upstream.ModelInfo{
				{ID: "glm-5.2", ContextWindow: 1000000},
				{ID: "glm-other", ContextWindow: 1000000},
			},
			fetched: time.Now(),
		},
	})

	return handler, &calls
}

func allowedSet(names ...string) map[string]struct{} {
	return livecfg.NormalizeAllowedModels(names)
}

// TestModelsAllowedListFilters /v1/models 只列白名单内模型；cn:/global: 按域隔离。
func TestModelsAllowedListFilters(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		allowed map[string]struct{}
		want    []string
	}{
		{"白名单外的模型不列出", allowedSet("glm-5.2"), []string{"cn:glm-5.2"}},
		{"global 前缀不放行 cn", allowedSet("global:glm-5.2"), nil},
		{"未启用白名单保持全量", nil, []string{"cn:glm-5.2", "cn:glm-other"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			handler, _ := newAllowedModelsHandler(testCase.allowed, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}

			ids := modelIDsFromList(t, rec.Body.Bytes())
			if len(ids) != len(testCase.want) {
				t.Fatalf("ids=%v want %v", ids, testCase.want)
			}

			for _, want := range testCase.want {
				if !ids[want] {
					t.Fatalf("ids=%v missing %q", ids, want)
				}
			}
		})
	}
}

// TestModelsAllowedFiltersAliases 别名名字与目标都必须通过白名单：任一不通过都不列出
// （别名也是客户端可见的 model id，不能成为白名单外的入口）。
func TestModelsAllowedFiltersAliases(t *testing.T) {
	t.Parallel()

	aliases := map[string]string{"fast": "glm-5.2"}

	cases := []struct {
		name    string
		allowed map[string]struct{}
		want    bool
	}{
		{"目标不在白名单", allowedSet("fast"), false},
		{"目标在但别名名不在白名单", allowedSet("cn:glm-5.2"), false},
		{"别名名与目标都在白名单", allowedSet("fast", "cn:glm-5.2"), true},
		{"未启用白名单", nil, true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			handler, _ := newAllowedModelsHandler(testCase.allowed, aliases)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

			if got := modelIDsFromList(t, rec.Body.Bytes())["fast"]; got != testCase.want {
				t.Fatalf("alias fast 出现=%v want %v body=%s", got, testCase.want, rec.Body)
			}
		})
	}
}

// TestModelsAllowedRejectsPointCall 白名单外点名调用 400 model_unavailable，
// 不打上游、不占号；cn:/global: 按域隔离；白名单内正常放行。
func TestModelsAllowedRejectsPointCall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		model     string
		allowed   map[string]struct{}
		hidden    map[string]struct{}
		wantCode  int
		wantCalls int64
	}{
		{"白名单外拒绝", "glm-other", allowedSet("glm-5.2"), nil, http.StatusBadRequest, 0},
		{"global 前缀不放行 cn", "glm-5.2", allowedSet("global:glm-5.2"), nil, http.StatusBadRequest, 0},
		{"白名单内放行", "glm-5.2", allowedSet("glm-5.2"), nil, http.StatusOK, 1},
		{
			"与黑名单同时命中拒绝", "glm-5.2", allowedSet("glm-5.2"),
			livecfg.NormalizeHiddenModels([]string{"glm-5.2"}), http.StatusBadRequest, 0,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			handler, calls := newAllowedModelsHandler(testCase.allowed, nil)
			handler.cfg.HiddenModels = testCase.hidden

			state := handler.cfg.Pool
			rec := postChat(t, handler, `{"model":"`+testCase.model+`","messages":[]}`)

			if rec.Code != testCase.wantCode {
				t.Fatalf("code=%d body=%s want %d", rec.Code, rec.Body, testCase.wantCode)
			}

			if testCase.wantCode != http.StatusOK {
				if !strings.Contains(rec.Body.String(), "model_unavailable") {
					t.Errorf("错误体应含 model_unavailable: %s", rec.Body)
				}

				if st := statusOf(t, state, "u1"); st.ErrTotal != 0 || st.Cooling {
					t.Errorf("白名单拒绝不应罚账号: %+v", st)
				}
			}

			if got := calls.Load(); got != testCase.wantCalls {
				t.Fatalf("上游调用=%d want %d", got, testCase.wantCalls)
			}
		})
	}
}

// TestCountTokensAllowedModelRejected count_tokens 同样受部署级白名单约束，
// 白名单外返回 400 且不返回 token 估算。
func TestCountTokensAllowedModelRejected(t *testing.T) {
	t.Parallel()

	handler := NewHandler(&Config{
		Pool:          testPoolWith(),
		Upstream:      upstream.New(),
		modelsCache:   &modelsCache{},
		AllowedModels: allowedSet("cn:glm-5.2"),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-other","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Anthropic-Version", "2023-06-01")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "models.allowed") {
		t.Errorf("错误应说明被 models.allowed 拒绝: %s", rec.Body)
	}

	if strings.Contains(rec.Body.String(), "input_tokens") {
		t.Errorf("不得返回 token 估算: %s", rec.Body)
	}
}

// TestBusyAllowedModelUnavailable 预检里白名单外模型报 unavailable（与 hidden 同档）。
func TestBusyAllowedModelUnavailable(t *testing.T) {
	t.Parallel()

	handler, _ := newBusyTestHandler(t)
	handler.cfg.Live = livecfg.New(livecfg.Snapshot{AllowedModels: allowedSet("cn:glm-5.2")})

	rec := postBusy(t, handler, `{"models":["glm-5.2","kimi-k2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	want := map[string]string{"glm-5.2": "idle", "kimi-k2": "unavailable"}
	for _, item := range decodeBusy(t, rec).Data {
		if item.BusyLevel != want[item.Model] {
			t.Errorf("%s busy_level=%q want %q", item.Model, item.BusyLevel, want[item.Model])
		}
	}
}

// TestModelsAllowedRetrieveNotFound 白名单外模型的单条查询自然 404，不泄露存在性。
func TestModelsAllowedRetrieveNotFound(t *testing.T) {
	t.Parallel()

	handler, _ := newAllowedModelsHandler(allowedSet("cn:glm-5.2"), nil)

	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, httptest.NewRequest(http.MethodGet, "/v1/models/cn:glm-5.2", nil))

	if ok.Code != http.StatusOK {
		t.Fatalf("白名单内模型 code=%d body=%s", ok.Code, ok.Body)
	}

	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/v1/models/cn:glm-other", nil))

	if denied.Code != http.StatusNotFound || !strings.Contains(denied.Body.String(), "model_not_found") {
		t.Fatalf("白名单外模型 code=%d body=%s want 404", denied.Code, denied.Body)
	}
}
