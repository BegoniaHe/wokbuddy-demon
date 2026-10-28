package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
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

// TestClassifyQueueFullTextFallback 队列满载的文案外卡：上游换状态码或丢 code 时
// 仍须归 ErrQueueFull（渠道级容量），不得落 ErrSoftRate 逐号软冷却。
func TestClassifyQueueFullTextFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		// 结构化码优先，状态码无关。
		{http.StatusTooManyRequests, `{"code":6021,"msg":"queue.full.title"}`, ErrQueueFull},
		{http.StatusInternalServerError, `{"code":6021,"msg":"queue.full.title"}`, ErrQueueFull},
		// 只有文案：queue.full 连写两种形态。
		{http.StatusTooManyRequests, `{"msg":"queue.full.title"}`, ErrQueueFull},
		{http.StatusServiceUnavailable, `{"msg":"Queue Full"}`, ErrQueueFull},
		// 防过宽：普通 429 与普通 5xx 不得被外卡吞掉。
		{http.StatusTooManyRequests, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{http.StatusInternalServerError, `{"msg":"internal error"}`, ErrServer},
	}

	for _, testCase := range cases {
		if got := Classify(testCase.status, testCase.body); got != testCase.want {
			t.Errorf("Classify(%d,%s)=%v want %v", testCase.status, testCase.body, got, testCase.want)
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
		{`{"code":1001,"msg":"already"}`, CheckinAlready},
		{`{"code":1002,"msg":"not eligible"}`, CheckinInactive},
		{`{"code":1003,"msg":"event ended"}`, CheckinInactive},
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
		CapacityRemainPrecise: "75", CapacityUsedPrecise: "25", CapacitySizePrecise: "100",
	})
	if remain != 75 || used != 25 || size != 100 {
		t.Errorf("precise=%d/%d/%d want 75/25/100", remain, used, size)
	}

	remain, used, size = packageRemainUsed(respAccount{
		CapacityType:   4,
		CapacityRemain: 10, CapacityUsed: 90, CapacitySize: 100,
		SlicePeriodUsageDetails: []slicePeriodUsage{{
			SlicePeriodCapacitySizePrecise: "50", SlicePeriodCapacityRemainPrecise: "20",
		}},
	})
	if remain != 20 || used != 30 || size != 50 {
		t.Errorf("slice=%d/%d/%d want 20/30/50", remain, used, size)
	}

	// 周期套餐同时下发 Capacity* 与 Cycle* 精确字段时，仍以 Cycle* 为准。
	remain, used, size = packageRemainUsed(respAccount{
		CapacitySizePrecise: "1000", CapacityRemainPrecise: "900", CapacityUsedPrecise: "100",
		CycleCapacitySizePrecise: "100", CycleCapacityRemainPrecise: "40", CycleCapacityUsedPrecise: "60",
	})
	if remain != 40 || used != 60 || size != 100 {
		t.Errorf("cycle precise=%d/%d/%d want 40/60/100", remain, used, size)
	}
}

// TestSummaryTotalsDefaultsSizeToRemain 汇总包缺失总额时按剩余兜底，不能报 0/0。
func TestSummaryTotalsDefaultsSizeToRemain(t *testing.T) {
	t.Parallel()

	var summary resourceSummaryResp
	if err := json.Unmarshal([]byte(`{"Packages":[
		{"CycleTotalCapacity":0,"CycleRemainCapacity":12}
	]}`), &summary); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	remain, total := summary.summaryTotals()
	if remain != 12 || total != 12 {
		t.Errorf("summary=%d/%d want 12/12", remain, total)
	}
}

// TestFlexInt64Shapes 容量字段兼容数字/字符串/浮点串，非法值与溢出值必须报错，
// 以便调用方继续回落旧端点。
func TestFlexInt64Shapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "number", raw: `1000`, want: 1000},
		{name: "string", raw: `"1000"`, want: 1000},
		{name: "string-spaces", raw: `" 1000 "`, want: 1000},
		{name: "negative", raw: `"-5"`, want: -5},
		{name: "float-string", raw: `"700.9"`, want: 700},
		{name: "scientific-string", raw: `"1e3"`, want: 1000},
		{name: "escaped-string", raw: `"\u0031000"`, want: 1000},
		{name: "null", raw: `null`, want: 0},
		{name: "empty-string", raw: `""`, wantErr: true},
		{name: "blank-string", raw: `"  "`, wantErr: true},
		{name: "quoted-null", raw: `"null"`, wantErr: true},
		{name: "text", raw: `"abc"`, wantErr: true},
		{name: "nan", raw: `"NaN"`, wantErr: true},
		{name: "inf", raw: `"Inf"`, wantErr: true},
		{name: "overflow", raw: `"1e19"`, wantErr: true},
		{name: "negative-overflow", raw: `"-9223372036854775809"`, wantErr: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var got flexInt64

			err := got.UnmarshalJSON([]byte(testCase.raw))
			if testCase.wantErr {
				if !errors.Is(err, errFlexInt64Invalid) {
					t.Fatalf("raw=%s err=%v want errFlexInt64Invalid", testCase.raw, err)
				}

				return
			}

			if err != nil || int64(got) != testCase.want {
				t.Fatalf("raw=%s got=%d err=%v want %d", testCase.raw, int64(got), err, testCase.want)
			}
		})
	}
}

// TestSummaryResourceFallbackOnInvalidCapacity 容量值非法时 summary 解码失败并
// 回落旧端点，不能静默按 0 计；解码失败是确定性错误，不能触发瞬时重试。
func TestSummaryResourceFallbackOnInvalidCapacity(t *testing.T) {
	t.Parallel()

	var summaryCalls atomic.Int32

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "get-user-resource-summary") {
			summaryCalls.Add(1)

			return jsonResp(200, `{"code":0,"data":{"Packages":[
				{"CycleTotalCapacity":"abc","CycleRemainCapacity":"1"}
			]}}`), nil
		}

		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"CycleCapacitySize":500,"CycleCapacityRemain":400,"CycleCapacityUsed":100}
		]}}}}`), nil
	})

	remain, total, err := client.UserResource(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil || remain != 400 || total != 500 {
		t.Fatalf("remain=%d total=%d err=%v want fallback 400/500", remain, total, err)
	}

	if got := summaryCalls.Load(); got != 1 {
		t.Fatalf("summary calls=%d want 1 (decode failure must not retry)", got)
	}
}

// TestSummaryTotalsAcceptsStringCapacity 上游把容量字段下发为字符串时仍能解析。
func TestSummaryTotalsAcceptsStringCapacity(t *testing.T) {
	t.Parallel()

	var summary resourceSummaryResp
	if err := json.Unmarshal([]byte(`{"Packages":[
		{"CycleTotalCapacity":" 1000 ","CycleRemainCapacity":"700.0","CycleUsedCapacity":"300"}
	]}`), &summary); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	remain, total := summary.summaryTotals()
	if remain != 700 || total != 1000 {
		t.Errorf("summary=%d/%d want 700/1000", remain, total)
	}
}

// TestSummaryResourceAcceptsStringCapacity 端到端覆盖上游以字符串下发容量字段：
// summary 解码成功后直接采用汇总值，不回落旧端点。
func TestSummaryResourceAcceptsStringCapacity(t *testing.T) {
	t.Parallel()

	var fallbackCalls atomic.Int32

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "get-user-resource-summary"):
			return jsonResp(200, `{"code":0,"data":{"Packages":[
				{"PackageCode":"p","CycleTotalCapacity":"1000","CycleRemainCapacity":"700","CycleUsedCapacity":"300"}
			]}}`), nil
		case strings.HasSuffix(r.URL.Path, "get-user-resource"):
			fallbackCalls.Add(1)

			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
				{"CycleCapacitySize":1,"CycleCapacityRemain":1,"CycleCapacityUsed":0}
			]}}}}`), nil
		default:
			return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
		}
	})

	remain, total, err := client.UserResource(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil || remain != 700 || total != 1000 {
		t.Fatalf("remain=%d total=%d err=%v want summary 700/1000", remain, total, err)
	}

	if got := fallbackCalls.Load(); got != 0 {
		t.Fatalf("fallback calls=%d want 0 (string summary must decode)", got)
	}
}

// TestResourcePackageFreeBodyDesktopShape free 包 body 对齐官方桌面 validCreditPackages：
// `Status:[0,3]` + `OnlyValidPeriod:true` + `IsDisplayTotalInfo:true` + `PageSize:200`，
// 不发 SlicePeriod*（那是另一套 BackendProvider 实现的字段，桌面不出现）。
func TestResourcePackageFreeBodyDesktopShape(t *testing.T) {
	t.Parallel()

	var freeBody []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/get-user-resource-free-packages") {
			freeBody, _ = io.ReadAll(r.Body)
		}

		return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
	})

	if _, _ = client.resourcePackages(&auth.Auth{AccessToken: "at", UID: "u1"}, time.Now(), 0); len(freeBody) == 0 {
		t.Fatal("free package request not captured")
	}

	for _, want := range []string{
		`"Status":[0,3]`,
		`"OnlyValidPeriod":true`,
		`"PageSize":200`,
		`"IsDisplayTotalInfo":true`,
	} {
		if !strings.Contains(string(freeBody), want) {
			t.Errorf("free body=%s missing %s", freeBody, want)
		}
	}

	for _, unwanted := range []string{"SlicePeriodStartTime", "SlicePeriodEndTime"} {
		if strings.Contains(string(freeBody), unwanted) {
			t.Errorf("free body=%s 不应含 %s（桌面不发 slice 窗口）", freeBody, unwanted)
		}
	}
}

// TestResourcePackageExpirySendsPackageCodes 上游要求显式 PackageCodes；
// paid/free 两个端点必须各带自己的码表，且不能混用。
func TestResourcePackageExpirySendsPackageCodes(t *testing.T) {
	t.Parallel()

	bodies := make(map[string]string)

	client := testClient(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		for _, key := range []string{"paid", "free"} {
			if strings.HasSuffix(r.URL.Path, "/get-user-resource-"+key+"-packages") {
				bodies[key] = string(body)
			}
		}

		return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
	})

	if _, _ = client.resourcePackages(&auth.Auth{AccessToken: "at", UID: "u1"}, time.Now(), 0); len(bodies) != 2 {
		t.Fatalf("captured bodies=%v want paid and free", bodies)
	}

	if !strings.Contains(bodies["paid"], paidResourcePackageCodes[0]) ||
		strings.Contains(bodies["paid"], freeResourcePackageCodes[0]) {
		t.Errorf("paid body=%s want paid codes only", bodies["paid"])
	}

	if !strings.Contains(bodies["free"], freeResourcePackageCodes[0]) ||
		strings.Contains(bodies["free"], paidResourcePackageCodes[0]) {
		t.Errorf("free body=%s want free codes only", bodies["free"])
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

// TestRefresh403WAFNotSessionDead CDN/WAF 403（无业务信封）不得被 refresh 路径
// 误判为会话失效而永久禁用账号。
func TestRefresh403WAFNotSessionDead(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusForbidden, `<html><body>blocked</body></html>`), nil
	})

	err := client.RefreshToken(&auth.Auth{AccessToken: "at", RefreshToken: "rt"})

	var ue *Error
	if !asError(err, &ue) || ue.Kind != ErrWafBlock {
		t.Errorf("err=%v want ErrWafBlock", err)
	}
}

// TestCheckinBusinessCodeExactMatch 业务码按完整数字匹配，10012 不得命中 1001。
func TestCheckinBusinessCodeExactMatch(t *testing.T) {
	t.Parallel()

	if got := ClassifyCheckin(&Error{Status: 200, Msg: `{"code":10012,"msg":"unknown"}`}); got != CheckinFailed {
		t.Errorf("ClassifyCheckin(code=10012)=%v want CheckinFailed", got)
	}

	if got := ClassifyCheckin(&Error{Status: 200, Msg: `{"code":1001,"msg":"already"}`}); got != CheckinAlready {
		t.Errorf("ClassifyCheckin(code=1001)=%v want CheckinAlready", got)
	}
}

// TestStringPreciseCapacityDecoded 上游 precise 容量实为 JSON 字符串；解码后应
// 仍按精确字段聚合，而不是让 packages/resource 解析报 string into int64。
func TestStringPreciseCapacityDecoded(t *testing.T) {
	t.Parallel()

	var pkg billingPackageAccount

	rawPackage := `{
		"CapacityType":2,
		"CapacityRemain":10,"CapacityUsed":90,"CapacitySize":100,
		"CapacityRemainPrecise":"75","CapacityUsedPrecise":"25","CapacitySizePrecise":"100",
		"CycleCapacitySizePrecise":"0"
	}`
	if err := json.Unmarshal([]byte(rawPackage), &pkg); err != nil {
		t.Fatalf("package unmarshal: %v", err)
	}

	cycle, remain, used, size := creditPackageAmounts(&pkg)
	if cycle || remain != 75 || used != 25 || size != 100 {
		t.Errorf("package amounts=%v/%d/%d/%d want false/75/25/100", cycle, remain, used, size)
	}

	var acct resourceAccount

	rawAccount := `{
		"CapacityType":2,
		"CapacityRemain":10,"CapacityUsed":90,"CapacitySize":100,
		"CapacityRemainPrecise":"75","CapacityUsedPrecise":"25","CapacitySizePrecise":"100"
	}`
	if err := json.Unmarshal([]byte(rawAccount), &acct); err != nil {
		t.Fatalf("resource unmarshal: %v", err)
	}

	remain, used, size = packageRemainUsed(respAccount{
		CapacityType:          acct.CapacityType,
		CapacityRemain:        acct.CapacityRemain,
		CapacityUsed:          acct.CapacityUsed,
		CapacitySize:          acct.CapacitySize,
		CapacityRemainPrecise: acct.CapacityRemainPrecise,
		CapacityUsedPrecise:   acct.CapacityUsedPrecise,
		CapacitySizePrecise:   acct.CapacitySizePrecise,
	})
	if remain != 75 || used != 25 || size != 100 {
		t.Errorf("resource amounts=%d/%d/%d want 75/25/100", remain, used, size)
	}
}

// TestMalformedPreciseCapacityFallsBack 非法精确值不得吞掉整包，回落整数容量字段。
func TestMalformedPreciseCapacityFallsBack(t *testing.T) {
	t.Parallel()

	var pkg billingPackageAccount
	if err := json.Unmarshal([]byte(`{
		"CapacityRemain":10,"CapacityUsed":90,"CapacitySize":100,
		"CapacitySizePrecise":"bad"
	}`), &pkg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	_, remain, used, size := creditPackageAmounts(&pkg)
	if remain != 10 || used != 90 || size != 100 {
		t.Errorf("fallback=%d/%d/%d want 10/90/100", remain, used, size)
	}

	// CycleSize 有效但 CycleRemain 非法：整体回落整数，不得改用 Capacity 精确值
	// （Capacity* 是总额，不能覆盖当周期余额）。
	if err := json.Unmarshal([]byte(`{
		"CapacityRemain":10,"CapacityUsed":90,"CapacitySize":100,
		"CapacityRemainPrecise":"50","CapacityUsedPrecise":"50","CapacitySizePrecise":"50",
		"CycleCapacitySize":100,"CycleCapacityRemain":40,"CycleCapacityUsed":60,
		"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"bad","CycleCapacityUsedPrecise":"0"
	}`), &pkg); err != nil {
		t.Fatalf("cycle unmarshal: %v", err)
	}

	cycle, remain, used, size := creditPackageAmounts(&pkg)
	if !cycle || remain != 40 || used != 60 || size != 100 {
		t.Errorf("cycle fallback=%v/%d/%d/%d want true/40/60/100", cycle, remain, used, size)
	}
}

// TestStringPreciseCapacityBranches 覆盖字符串 precise 的切片/周期分支解码。
func TestStringPreciseCapacityBranches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		raw       string
		wantCycle bool
		remain    int64
		used      int64
		size      int64
	}{
		{
			name: "slice",
			raw: `{
				"CapacityType":4,
				"SlicePeriodUsageDetails":[{
					"SlicePeriodCapacitySizePrecise":"50",
					"SlicePeriodCapacityRemainPrecise":"20",
					"SlicePeriodCapacityUsedPrecise":"30"
				}]
			}`,
			wantCycle: true, remain: 20, used: 30, size: 50,
		},
		{
			name: "cycle",
			raw: `{
				"CapacityRemain":10,"CapacityUsed":90,"CapacitySize":1000,
				"CapacityRemainPrecise":"900","CapacityUsedPrecise":"100","CapacitySizePrecise":"1000",
				"CycleCapacityRemain":0,"CycleCapacityUsed":0,"CycleCapacitySize":100,
				"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"40","CycleCapacityUsedPrecise":"60"
			}`,
			wantCycle: true, remain: 40, used: 60, size: 100,
		},
	}
	for _, testCase := range cases {
		var pkg billingPackageAccount
		if err := json.Unmarshal([]byte(testCase.raw), &pkg); err != nil {
			t.Fatalf("%s unmarshal: %v", testCase.name, err)
		}

		cycle, remain, used, size := creditPackageAmounts(&pkg)
		if cycle != testCase.wantCycle || remain != testCase.remain || used != testCase.used || size != testCase.size {
			t.Errorf("%s amounts=%v/%d/%d/%d want %v/%d/%d/%d",
				testCase.name, cycle, remain, used, size,
				testCase.wantCycle, testCase.remain, testCase.used, testCase.size)
		}
	}
}
