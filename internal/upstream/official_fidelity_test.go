package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestChatHeadersEnterpriseTenant 企业账号同时发企业/租户头；个人 global 发 X-No-Enterprise-Id。
func TestChatHeadersEnterpriseTenant(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		auth   *auth.Auth
		global bool
		want   map[string]string
	}{
		{
			name: "cn-enterprise",
			auth: &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "ent-1", Domain: "copilot.tencent.com"},
			want: map[string]string{"X-Enterprise-Id": "ent-1", "X-Tenant-Id": "ent-1", "X-Domain": "copilot.tencent.com"},
		},
		{
			name:   "global-enterprise",
			auth:   &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "ent-9", Domain: "www.workbuddy.ai"},
			global: true,
			want:   map[string]string{"X-Enterprise-Id": "ent-9", "X-Tenant-Id": "ent-9", "X-Domain": "www.workbuddy.ai"},
		},
		{
			name:   "global-personal",
			auth:   &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"},
			global: true,
			want:   map[string]string{"X-No-Enterprise-Id": "1", "X-Domain": "www.workbuddy.ai"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var got http.Header

			client := testClient(func(r *http.Request) (*http.Response, error) {
				got = r.Header.Clone()

				return jsonResp(200, "data: [DONE]\n\n"), nil
			})
			client.GlobalEnabled = true
			client.ChatBaseGlobal = "https://global.example"

			rc, status, _, err := client.ChatStream(testCase.auth, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
			if err != nil || status != 200 {
				t.Fatalf("status=%d err=%v", status, err)
			}

			if rc != nil {
				rc.Close()
			}

			for k, want := range testCase.want {
				if got.Get(k) != want {
					t.Errorf("%s=%q want %q", k, got.Get(k), want)
				}
			}
		})
	}
}

// TestRefresh401403SessionDead refresh 端点 401/403 必须进入会话失效分类。
func TestRefresh401403SessionDead(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		client := testClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(status, `{"code":401,"msg":"denied"}`), nil
		})
		err := client.RefreshToken(&auth.Auth{AccessToken: "at", RefreshToken: "rt"})

		var ue *Error
		if !asError(err, &ue) || ue.Kind != ErrSessionDead {
			t.Errorf("status=%d err=%v want ErrSessionDead", status, err)
		}
	}
}

// asError 是 errors.As 的测试内小包装，避免测试文件再引入 errors。
func asError(err error, target **Error) bool {
	return errors.As(err, target)
}

// TestDeltaReasoningAliasAggregates delta.reasoning 归一为 reasoning_content。
func TestDeltaReasoningAliasAggregates(t *testing.T) {
	t.Parallel()

	raw := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"think\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	choices := resp["choices"].([]any)

	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "think" {
		t.Errorf("reasoning_content=%v want think", msg["reasoning_content"])
	}
}

// TestStreamDuplicateToolCallIDStripped 同 index 后续分片重复 id 删除，保留首片 id。
func TestStreamDuplicateToolCallIDStripped(t *testing.T) {
	t.Parallel()

	raw := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"arguments\":\"}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	rec := &stringRecorder{header: http.Header{}}
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatalf("stream: %v", err)
	}

	frames := strings.Split(rec.body.String(), "\n\n")

	var seen int

	for _, frame := range frames {
		if !strings.HasPrefix(frame, "data: {") {
			continue
		}

		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &obj) != nil {
			continue
		}

		choices, _ := obj["choices"].([]any)
		if len(choices) == 0 {
			continue
		}

		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)

		tcs, _ := delta["tool_calls"].([]any)
		for _, item := range tcs {
			call := item.(map[string]any)

			seen++
			if seen == 2 {
				if _, ok := call["id"]; ok {
					t.Errorf("second chunk must not repeat id: %#v", call)
				}
			}
		}
	}

	if seen != 2 {
		t.Fatalf("tool call chunks=%d want 2", seen)
	}
}

// stringRecorder 最小 http.ResponseWriter 替身（测试用）。
type stringRecorder struct {
	header http.Header
	body   strings.Builder
	status int
}

func (r *stringRecorder) Header() http.Header { return r.header }
func (r *stringRecorder) Write(p []byte) (int, error) {
	written, _ := r.body.Write(p)

	return written, nil
}
func (r *stringRecorder) WriteHeader(status int) { r.status = status }
func (r *stringRecorder) Flush()                 {}

// TestClassifyQueueAndQuotaCodes 官方客户端业务码映射。
func TestClassifyQueueAndQuotaCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want ErrKind
	}{
		{`{"code":6020,"msg":"waiting"}`, ErrQueueWaiting},
		{`{"code":6021,"msg":"queue full"}`, ErrQueueFull},
		{`{"code":6022,"msg":"user limit"}`, ErrQueueFull},
		{`{"code":14001,"msg":"quota exhausted"}`, ErrHardCredit},
		{`{"code":14012,"msg":"quota exhausted"}`, ErrHardCredit},
		{`{"code":14014,"msg":"quota exhausted"}`, ErrHardCredit},
		{`{"code":14019,"msg":"quota exhausted"}`, ErrHardCredit},
		{`{"code":14015,"msg":"auth expired"}`, ErrSessionDead},
		{`{"code":14016,"msg":"not activated"}`, ErrAccountFault},
	}

	for _, testCase := range cases {
		if got := Classify(http.StatusBadRequest, testCase.body); got != testCase.want {
			t.Errorf("Classify(%s)=%v want %v", testCase.body, got, testCase.want)
		}
	}
}

// TestCheckinBusinessCodes 1001/1002/1003 分类。
func TestCheckinBusinessCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		msg  string
		want CheckinResult
	}{
		{"code=1001 msg=already", CheckinAlready},
		{"code=1002 msg=not eligible", CheckinInactive},
		{"code=1003 msg=event ended", CheckinInactive},
	}

	for _, testCase := range cases {
		if got := ClassifyCheckin(&Error{Status: 200, Msg: testCase.msg}); got != testCase.want {
			t.Errorf("ClassifyCheckin(%q)=%v want %v", testCase.msg, got, testCase.want)
		}
	}
}

// TestPrecisePackageCapacityPrecedence 精确容量字段优先；CapacityType=4 取切片明细。
func TestPrecisePackageCapacityPrecedence(t *testing.T) {
	t.Parallel()

	remain, used, size := packageRemainUsed(respAccount{
		CapacityRemain: 10, CapacityUsed: 90, CapacitySize: 100,
		CapacityRemainPrecise: 75, CapacityUsedPrecise: 25, CapacitySizePrecise: 100,
	})
	if remain != 75 || used != 25 || size != 100 {
		t.Errorf("precise=%d/%d/%d want 75/25/100", remain, used, size)
	}

	remain, used, size = packageRemainUsed(respAccount{
		CapacityType:   4,
		CapacityRemain: 10, CapacityUsed: 90, CapacitySize: 100,
		SlicePeriodUsageDetails: []slicePeriodUsage{{
			SlicePeriodCapacitySizePrecise: 50, SlicePeriodCapacityRemainPrecise: 20,
		}},
	})
	if remain != 20 || used != 30 || size != 50 {
		t.Errorf("slice=%d/%d/%d want 20/30/50", remain, used, size)
	}
}

// TestSlicePackageUsesCycleEndTime 切片/每日包到期以 CycleEndTime 为准。
func TestSlicePackageUsesCycleEndTime(t *testing.T) {
	t.Parallel()

	got, expiresAt := creditPackageExpiry(&billingPackageAccount{
		CapacityType: 4, CycleEndTime: "2026-10-20 00:00:00", DeductionEndTime: 1792063023116,
	})
	if got != "2026-10-20 00:00:00" || expiresAt != 0 {
		t.Errorf("expiry=%q/%d want CycleEndTime", got, expiresAt)
	}
}

// TestSummaryResourcePrimaryAndFallback summary 为主，旧端点失败回落。
//
//nolint:cyclop // 覆盖 summary 与旧端点两条路径。
func TestSummaryResourcePrimaryAndFallback(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "get-user-resource-summary"):
			return jsonResp(200, `{"code":0,"data":{"Packages":[
				{"PackageCode":"p","CycleTotalCapacity":1000,"CycleRemainCapacity":700,"CycleUsedCapacity":300}
			]}}`), nil
		case strings.Contains(r.URL.Path, "get-user-resource-"):
			return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
		default:
			return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
		}
	})

	remain, total, err := client.UserResource(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil || remain != 700 || total != 1000 {
		t.Fatalf("summary remain=%d total=%d err=%v want 700/1000", remain, total, err)
	}

	// summary 失败 → 旧端点兜底。
	client = testClient(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "get-user-resource-summary") {
			return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
		}

		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacitySize":500,"CycleCapacityRemain":400,"CycleCapacityUsed":100}
		]}}}}`), nil
	})

	remain, total, err = client.UserResource(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil || remain != 400 || total != 500 {
		t.Fatalf("fallback remain=%d total=%d err=%v want 400/500", remain, total, err)
	}
}

// TestAvailableModelsFiltering availableModels 非空时只暴露列出的模型。
func TestAvailableModelsFiltering(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"availableModels":["glm-5.2"],
				"models":[{"id":"glm-5.2"},{"id":"kimi-k3"}],
				"agents":[{"name":"cli","models":["glm-5.2","kimi-k3"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2"},{"id":"kimi-k3"}],"availableModels":["glm-5.2"]}}`), nil
		default:
			return jsonResp(404, `{"code":404}`), nil
		}
	})

	out, err := client.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}

	if len(out) != 1 || out[0].ID != "glm-5.2" {
		t.Errorf("models=%+v want only glm-5.2", out)
	}
}

// TestModelAliasesStoredAndResolved 上游 alias 参与解析，配置别名可覆盖。
func TestModelAliasesStoredAndResolved(t *testing.T) {
	t.Parallel()

	client := New()
	client.storeModelAliases("cn", []ModelInfo{{ID: "glm-5.2", Aliases: []string{"glm"}}})

	if got, ok := client.ResolveUpstreamAlias("cn", "glm"); !ok || got != "glm-5.2" {
		t.Errorf("alias=%q/%v want glm-5.2", got, ok)
	}

	if aliases := client.UpstreamAliases("cn"); aliases["glm"] != "glm-5.2" {
		t.Errorf("aliases=%v", aliases)
	}
}

// TestChatOriginFailover 首 token 前 502 切下一个 CN origin；自定义 base 不回落。
//
//nolint:cyclop // 覆盖 origin 顺序与自定义 base。
func TestChatOriginFailover(t *testing.T) {
	t.Parallel()

	var hosts []string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		if r.URL.Host == "www.workbuddy.cn" {
			return jsonResp(502, `{"code":502,"msg":"bad gateway"}`), nil
		}

		return jsonResp(200, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
	})
	client.ChatBaseCN = defaultCNChatBase

	rc, status, _, err := client.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}

	if rc != nil {
		rc.Close()
	}

	if len(hosts) != 2 || hosts[0] != "www.workbuddy.cn" || hosts[1] != "copilot.tencent.com" {
		t.Errorf("hosts=%v want workbuddy.cn -> copilot.tencent.com", hosts)
	}

	// 自定义 base 只使用配置值。
	hosts = nil

	client = testClient(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)

		return jsonResp(502, `{"code":502,"msg":"bad gateway"}`), nil
	})

	_, status, _, _ = client.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if status != 502 {
		t.Errorf("configured base status=%d want 502", status)
	}

	if len(hosts) != 1 || hosts[0] != "chat.example" {
		t.Errorf("configured hosts=%v want only chat.example", hosts)
	}
}

// TestCollectPackageExpiryStopsOnEmptyPage TotalCount 与过滤后页数不一致时，
// 空页必须立即终止，避免余额刷新空翻到 100 页上限。
func TestCollectPackageExpiryStopsOnEmptyPage(t *testing.T) {
	t.Parallel()

	var calls int

	client := testClient(func(r *http.Request) (*http.Response, error) {
		calls++

		return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":500}}`), nil
	})

	var totals resourceTotals
	if err := client.collectPackageExpiry(
		&auth.Auth{AccessToken: "at", UID: "u1"},
		"/billing/meter/get-user-resource-paid-packages", map[string]any{}, time.Now(), 0, &totals,
	); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if calls != 1 {
		t.Errorf("calls=%d want 1 (stop on empty page)", calls)
	}
}

// TestCreditPackageExpiryFallsBackToPackageEndTime 仅下发 PackageEndTime 的包仍能取到到期时刻。
func TestCreditPackageExpiryFallsBackToPackageEndTime(t *testing.T) {
	t.Parallel()

	got, expiresAt := creditPackageExpiry(&billingPackageAccount{PackageEndTime: "2026-10-20 00:00:00"})
	if got != "2026-10-20 00:00:00" || expiresAt != 0 {
		t.Errorf("expiry=%q/%d want PackageEndTime", got, expiresAt)
	}
}

// TestNormalizeChatBase 三种 base 形态归一为 host。
func TestNormalizeChatBase(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"https://h.example",
		"https://h.example/",
		"https://h.example/v2",
		"https://h.example/v2/chat/completions",
	} {
		if got := normalizeChatBase(input); got != "https://h.example" {
			t.Errorf("normalizeChatBase(%q)=%q", input, got)
		}
	}
}
