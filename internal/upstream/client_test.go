package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

var (
	errTestRefreshSuccessFailed          = errors.New("TestRefreshSuccess failed")
	errMissingXRefreshToken              = errors.New("missing X-Refresh-Token")
	errTestUserResourceAggregationFailed = errors.New("TestUserResourceAggregation failed")
	errWantPOST                          = errors.New("want POST")
	errWrongPath2                        = errors.New("wrong path")
)

func TestClassify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// 限流文案（issue #28）：状态码不是 429 时也必须识别为软限流，
		// 否则账号不会被冷却，下次请求仍会被选中。
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" 不是余额语义（无 credit/quota/积分/额度 等计费词），
		// 属于模型侧用量节流 → 短冷却（误判为硬冷却会把有余量的号停到次日 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // 限流文案优先于 5xx 分类
		// 内容策略拦截（HTTP 400 + 审核文案）：误报信号，不罚账号，走降级重试。
		{400, `Illegal API invocation from an unapproved channel`, ErrContentBlocked},
		{400, `{"code":"11-128","msg":"blocked by security policy"}`, ErrContentBlocked},
		{400, `unapproved channel`, ErrContentBlocked},
		// 通用 4xx（非审核文案）：仍判 ErrClient，只换号不罚。
		{400, `bad request`, ErrClient},
		// ErrBadParams：请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）。
		// 这是"发给上游的 body 有问题"（网关截断已由 413 消灭，剩余为客户端畸形 JSON），
		// 换了账号也一样 400，不罚号。具体词优先于通用 4xx。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		{400, `{"code":11101,"msg":"x"}`, ErrBadParams},
		// 图片格式/数据错误是确定性请求错误，分类后不轮转、不罚号。
		{400, `{"code":11101,"msg":"Parse message failed: invalid image_url content at index 2: json: cannot unmarshal string into Go value of type v2.ImageContent"}`, ErrImageInvalid},
		{400, `{"code":11135,"msg":"invalid_image_data"}`, ErrImageInvalid},
		{400, `invalid_image_data`, ErrImageInvalid},
		// 11135 业务码须容忍 JSON 空白（5d5223d：字面量 marker 只覆盖紧凑形态）。
		{400, `{"code": 11135, "msg":"image data invalid"}`, ErrImageInvalid},
		{400, `{"error":{"code": "11135", "message":"image invalid"}}`, ErrImageInvalid},
		// 11133 细分（issue #246）：缺/空 messages 是确定性请求体错误；图像/GIF 是模型
		// 能力问题；其余按官方「模型无法处理此请求」归请求级，不喂账号连败。
		{400, `{"code":11133,"extError":{"param":"messages"},"msg":"messages is required"}`, ErrBadParams},
		{400, `{"code":11133,"msg":"model does not support image"}`, ErrImageInvalid},
		{400, `{"code": 11133, "msg":"model failed to process request"}`, ErrBadParams},
		{429, `{"code":11133,"msg":"model busy"}`, ErrSoftRate},
		{200, `quota exceeded`, ErrHardCredit},
		// session 死亡优先于限流文案（401+12153 需人工重登，短冷却无意义）。
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		// 401 一律会话失效（issue #245）：plain 401 / APISIX invalid_token 不能落
		// ErrClient 只换号，否则死号会留在池中每个请求刷一遍 401。
		{401, `{"code":9999,"msg":"bad token"}`, ErrSessionDead},
		{401, `<html><body>invalid_token</body></html>`, ErrSessionDead},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
		// 11102「上游模型目录无此模型」：确定性答复，归 ErrModelNotFound 请求级 fail-fast。
		{404, `{"code":11102,"msg":"model [deepseek-v3-2-volc] service info not found"}`, ErrModelNotFound},
		{400, `{"error":{"code":"11102","message":"model service info not found"}}`, ErrModelNotFound},
		{400, `{"msg":"service info not found"}`, ErrModelNotFound},
		// 同节点内 code 为空串/纯空白时须继续回落后备键 errCode/error_code：
		// 停在空 code 会丢掉 11102 判定（回归：firstCodeField 曾「首个存在即返回」）。
		{400, `{"code":"","errCode":"11102"}`, ErrModelNotFound},
		{404, `{"code":"   ","error_code":"11102"}`, ErrModelNotFound},
		// msg 为纯空白时须继续回落后备键 message（同类回归）。
		{400, `{"msg":"   ","message":"service info not found"}`, ErrModelNotFound},
		// 11102 撞在 requestId 上不算（不得误避让可用模型）。
		{404, `{"requestId":"11102","msg":"ok"}`, ErrNotFound},
		// 429 + 11102 → 限流语义（ErrSoftRate），不是模型不存在。
		{429, `{"code":11102,"msg":"service info not found"}`, ErrSoftRate},
		// 429 + 余额措辞 → 限流语义（fork-scan-absorb T-3，本次修复点）：限流响应
		// body 高频携带 "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，
		// hardRule 在 429 之前会误判 ErrHardCredit 硬冷却到次日 04:00，白扔号约 12h。
		// 状态码是比关键词更权威的信号：真余额耗尽走 402，非 429 的 quota 措辞
		// 仍归 hardRule（上方 {200,"quota exceeded"} 语义不变）。
		{429, `quota exceeded`, ErrSoftRate},
		{429, `{"code":1,"msg":"quota exceeded, please wait"}`, ErrSoftRate},
		{429, `insufficient credits`, ErrSoftRate},
		{429, `{"code":1,"msg":"额度不足"}`, ErrSoftRate},
		{429, `积分不足，请充值`, ErrSoftRate},
		// 429 + 账号级故障码防回归（accountFault/sessionDead 仍先于 429 判定）：
		// 落到 status==429 兜底会误归 soft_rate，账号级故障等不来自愈。
		{429, `{"code":14017,"msg":"trial not activated"}`, ErrAccountFault},
		{429, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		{429, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{429, `{"code":14015,"msg":"enterprise auth expired"}`, ErrSessionDead},
		// Issue #175：14018 明确表示账号积分耗尽，即使 HTTP 状态是 429 也必须
		// 走硬积分冷却；仅有相同文案而无该业务码的普通 429 仍保持软限流。
		{429, `{"code":14018,"msg":"Credits exhausted"}`, ErrHardCredit},
		{429, `{"error":{"data":{"code":"14018","msg":"Credits exhausted"}}}`, ErrHardCredit},
		{429, `{"requestId":"14018","msg":"Credits exhausted"}`, ErrSoftRate},
		{429, `{"code":1,"msg":"Credits exhausted"}`, ErrSoftRate},
		// 请求级码：客户端语义是 editInput/newTask/reupload/upgradeClient，同一 body
		// 换号结果不变 → ErrBadParams（不罚号、不轮转、透传原文）。
		{400, `{"code":11125,"msg":"input format unsupported"}`, ErrBadParams},
		{400, `{"code":11129,"msg":"tool schema invalid"}`, ErrBadParams},
		{400, `{"code":11130,"msg":"too many attachments"}`, ErrBadParams},
		{400, `{"code":11132,"msg":"missing request info"}`, ErrBadParams},
		{400, `{"code":11137,"msg":"image too large"}`, ErrBadParams},
		{400, `{"code":11138,"msg":"client too old"}`, ErrBadParams},
		{400, `{"code":11139,"msg":"image format unsupported"}`, ErrBadParams},
		{400, `{"code":11148,"msg":"tool record incomplete"}`, ErrBadParams},
		{400, `{"code":11149,"msg":"tool record duplicated"}`, ErrBadParams},
		{400, `{"code":11150,"msg":"thinking effort unsupported"}`, ErrBadParams},
		{400, `{"code":11151,"msg":"blank message"}`, ErrBadParams},
		{400, `{"code":11152,"msg":"tool name invalid"}`, ErrBadParams},
		{400, `{"code":11153,"msg":"bad conversation ending"}`, ErrBadParams},
		{400, `{"code":11154,"msg":"attachment too large"}`, ErrBadParams},
		{400, `{"code":11155,"msg":"thinking record missing"}`, ErrBadParams},
		{400, `{"code":12654,"msg":"platform internal info"}`, ErrBadParams},
		{400, `{"code":14405,"msg":"request risk"}`, ErrBadParams},
		// 嵌套 data.code 与 bizCode 同义字段。
		{400, `{"error":{"data":{"code":11129}}}`, ErrBadParams},
		{400, `{"bizCode":11129,"msg":"tool schema invalid"}`, ErrBadParams},
		{400, `{"bizCode":"11148"}`, ErrBadParams},
		// 11136/11158 账号级权限缺失 → 换号 + 模型避让；11141 平台下线 → fail-fast。
		{400, `{"code":11136,"msg":"no model permission"}`, ErrModelBlocked},
		{400, `{"code":11158,"msg":"model is not allowed for user type"}`, ErrModelBlocked},
		{400, `{"code":11141,"msg":"model offline"}`, ErrModelAccess},
		{400, `{"error":{"data":{"code":11141}}}`, ErrModelAccess},
		{400, `{"bizCode":11136}`, ErrModelBlocked},
		// 11158 账号类型级模型权限拒绝（`model [x] is not allowed for user type [y]`）：
		// 400/404 与 11136 同族 → 换号 + 模型避让；429 带权限码按限流语义（与
		// linguo/misakano/csszyft 同口径），由 handler 走软限流换号。
		{400, `{"code":11158,"msg":"model [x] is not allowed for user type [y]"}`, ErrModelBlocked},
		{400, `{"code":"11158"}`, ErrModelBlocked},
		{400, `{"msg":"model [x] is not allowed for user type [y]"}`, ErrModelBlocked},
		{429, `{"code":11158,"msg":"model [x] is not allowed for user type [y]"}`, ErrSoftRate},
		// 14003 模型繁忙：429 限流语义（模型级短窗口）。
		{429, `{"code":14003,"msg":"too many requests"}`, ErrSoftRate},
		// 11-128 入站内容拦截（与出站 sanitize 同语义），不依赖文案 marker。
		{400, `{"code":"11-128","msg":"request intercepted"}`, ErrContentBlocked},
		{200, `{"code":"11-128","msg":"request intercepted"}`, ErrContentBlocked},
		// 11-128 分流：`+ request illegal` 是**通道校验失败**（账号健康、不降级重试），
		// 其余 11-128 仍是内容拦截——两种语义不能合并。
		{400, `{"code":"11-128","msg":"request illegal"}`, ErrChannelRefused},
		{400, `{"error":{"code":"11-128","message":"request illegal"}}`, ErrChannelRefused},
		// 非法 JSON 形态（code 值未加引号）：按字节子串仍须命中。
		{400, `{"code":11-128,"msg":"request illegal"}`, ErrChannelRefused},
		// 裸 "request illegal"（无 11140 code）不再升级成账号故障（上游本轮删掉的过宽 marker）。
		{400, `{"msg":"request illegal"}`, ErrClient},
		{400, `{"code":1,"msg":"request illegal"}`, ErrClient},
		// 11140 + request illegal 才是账号受限（code 与文案同时命中）。
		{400, `{"code":11140,"msg":"request illegal"}`, ErrAccountFault},
		// 额度族新增码与中文 hard marker。
		{400, `{"code":11145,"msg":"TokenPlan exhausted"}`, ErrHardCredit},
		{200, `{"code":14002,"msg":"agent quota exhausted"}`, ErrHardCredit},
		// 10105/15001 是账号级使用上限（会话数/联网搜索），软限流而非硬冷却。
		{200, `{"code":10105,"msg":"quota exhausted"}`, ErrSoftRate},
		{200, `{"code":15001,"msg":"quota exhausted"}`, ErrSoftRate},
		{400, `{"code":1,"msg":"余额已耗尽"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度已达上限"}`, ErrHardCredit},
		// 429 仍先于 hardMarkers：中文额度文案不得把限流硬冷却到次日。
		{429, `{"code":1,"msg":"额度已达上限"}`, ErrSoftRate},
		// 11142 账号级 auth 故障，先于 429。
		{400, `{"code":11142,"msg":"auth required"}`, ErrAccountFault},
		{429, `{"code":11142,"msg":"auth required"}`, ErrAccountFault},
		// 限流/窗口用量族 6000-6008：6004 保持模型级判定，其余账号级软限流。
		{200, `{"code":6000,"msg":"too frequent"}`, ErrSoftRate},
		{400, `{"code":6001,"msg":"send too fast"}`, ErrSoftRate},
		{400, `{"code":6002,"msg":"minute usage limit"}`, ErrSoftRate},
		{200, `{"code":6003,"msg":"hour usage limit"}`, ErrSoftRate},
		{200, `{"code":6004,"msg":"model daily limit"}`, ErrSoftRate},
		{400, `{"code":6005,"msg":"send too fast"}`, ErrSoftRate},
		{200, `{"code":6006,"msg":"minute count limit"}`, ErrSoftRate},
		{200, `{"code":6007,"msg":"hour count limit"}`, ErrSoftRate},
		{200, `{"code":6008,"msg":"daily count exhausted"}`, ErrSoftRate},
		{400, `{"code":1,"msg":"本分钟用量已达上限"}`, ErrSoftRate},
		{400, `{"code":1,"msg":"请求过于频繁"}`, ErrSoftRate},
		// WAF 403（P0-1）：403 + 无业务信封（无 "code":/"msg": 字段）→ ErrWafBlock。
		// 空体 / HTML 拦截页 / 纯文本 / 非信封 JSON 均命中。
		{403, ``, ErrWafBlock},
		{403, `<html><body>403 Forbidden</body></html>`, ErrWafBlock},
		{403, `Forbidden`, ErrWafBlock},
		{403, `{"message":"blocked by waf"}`, ErrWafBlock},
		{403, `<head><script>...</script></head><body>blocked</body>`, ErrWafBlock},
		// 403 带业务信封的仍走既有分类（P0-1 约束：不劫持业务 403）。
		{403, `{"code":"11-128","msg":"blocked by security policy"}`, ErrContentBlocked},
		{403, `{"code":60001,"msg":"quota exceeded"}`, ErrHardCredit},
		{403, `{"code":1,"msg":"unknown business error"}`, ErrClient},
		// 非 403 的无信封错误体不进 WAF 分类（WAF 判定绑定 403 形态）。
		{400, `bad request`, ErrClient},
		{429, ``, ErrSoftRate},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestIsModelRateLimit 判断 429 body 是否明确指向模型级限流（code 6004）。
func TestIsModelRateLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want bool
	}{
		// 6004：模型级限流（issue #31 的核心场景）。
		{`{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`, true},
		{`{"code": 6004,"msg":"x"}`, true},
		// 其他 code（非模型级限流）→ 不算。
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":1,"msg":"429 rate limit"}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestIsModelBusy 14003 与 6004 的 code 判定互不误伤。
func TestIsModelBusy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		body string
		want bool
	}{
		{`{"code":14003,"msg":"too many requests"}`, true},
		{`{"code": 14003}`, true},
		{`{"code":"14003"}`, true},
		{`{"code":6004,"msg":"model usage limit"}`, false},
		{`{"code":140030,"msg":"other"}`, false},
		{`{"requestId":"14003","msg":"ok"}`, false},
	}
	for _, tc := range cases {
		if got := IsModelBusy(tc.body); got != tc.want {
			t.Errorf("IsModelBusy(%s)=%v want %v", tc.body, got, tc.want)
		}
	}
}

// TestParseSoftRateReset 解析上游 429 6004 msg 里的「将在 … 重置」时间（## UTC+8）。
func TestParseSoftRateReset(t *testing.T) {
	t.Parallel()

	future := time.Now().Add(35 * time.Minute)
	str := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")

	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"6004 带时间+UTC+8 后缀", `{"code":6004,"msg":"将在 ` + str + ` UTC+8 重置"}`, true},
		{"6004 带时间无后缀", `{"code":6004,"msg":"将在 ` + str + ` 重置"}`, true},
		{"6004 无时间文案", `{"code":6004,"msg":"model usage limit exceeded"}`, false},
		{"非 6004 但带时间（ParseRateReset 统一解析；模型级豁免由调用侧按 6004 判定）", `{"code":11140,"msg":"将在 ` + str + ` UTC+8 重置"}`, true},
		{"非法时间格式", `{"code":6004,"msg":"将在 明天 重置"}`, false},
		{"空 body", ``, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseRateReset(item.body)
			if ok != item.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, item.ok, item.body)
			}

			if ok {
				// 解析结果 = ts 在 UTC+8 解释下的墙钟（截断到分钟），应与 future 相差 ±2 分钟。
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}

				if got.Location() != time.UTC {
					// 不同指针的 FixedZone 实例相等性按 offset 判，这里只断言 offset。
					if _, off := got.Zone(); off != 8*60*60 {
						t.Errorf("zone offset=%d want +08:00", off)
					}
				}
			}
		})
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, fmt.Errorf("%s: %w", "wrong path: "+r.URL.Path, errTestRefreshSuccessFailed)
		}

		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errMissingXRefreshToken
		}

		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})

	account := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := client.RefreshToken(account); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if account.AccessToken != "newat" || account.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", account)
	}

	if account.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", account.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	t.Parallel()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})

	account := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(account); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if account.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", account.ExpiresAt)
	}

	if account.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", account.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}

	err := client.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}

	var error *Error
	if !errors.As(err, &error) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}

	if error.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", error.Kind)
	}
}

func TestRefreshAuthRequiredIsSessionDead(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":11142,"msg":"auth required"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}

	err := client.RefreshToken(a)

	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}

	if upstreamErr.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", upstreamErr.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	t.Parallel()

	got := map[string]string{}

	var gotBody []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		for _, k := range []string{
			"Authorization", "X-User-Id", "X-Product", "X-Agent-Intent",
			"X-Private-Data", "X-Ide-Type", "X-Ide-Version",
		} {
			got[k] = r.Header.Get(k)
		}

		gotBody, _ = io.ReadAll(r.Body)

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	client.Profile = testProfileSet(t)
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}

	rc, status, respBody, err := client.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}

	rc.Close()
	// 官方桌面端 chat 指纹：X-Product=SaaS、X-IDE-Type=WorkBuddy、X-IDE-Version=realm 版本、
	// X-Agent-Intent=craft、X-Private-Data=false（CN 5.9.3 个人号默认分支）。
	want := map[string]string{
		"Authorization":  "Bearer at",
		"X-User-Id":      "u1",
		"X-Product":      "SaaS",
		"X-Agent-Intent": "craft",
		"X-Private-Data": "false",
		"X-Ide-Type":     "WorkBuddy",
		"X-Ide-Version":  "9.9.9",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("header %s=%q want %q", k, got[k], w)
		}
	}

	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	t.Parallel()

	var outbound []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	infos, err := client.FetchModels(account)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := client.ChatStream(account, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	rc.Close()
	assertEffortsDowngrade(t, infos, outbound)
}

// assertEffortsDowngrade 断言 effort 能力缓存与降级后的出站 body（glm-5.2 只支持
// low/high，请求 max 应降级为 high）。
func assertEffortsDowngrade(t *testing.T, infos []ModelInfo, outbound []byte) {
	t.Helper()

	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts，DefaultEffort 应携带 reasoning.effort
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}

	if infos[0].DefaultEffort != "high" {
		t.Errorf("infos[0].DefaultEffort=%q want high", infos[0].DefaultEffort)
	}

	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}

	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	t.Parallel()

	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	// 错误信封一次成型：≥400 返回已分类的 *Error（Kind + body 全量仍经 respBody 透出）
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrHardCredit {
		t.Fatalf("hard credit should return classified *Error envelope, got %v", err)
	}

	if len(respBody) == 0 {
		t.Errorf("body should still be returned for passthrough")
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	t.Parallel()

	const frames = 6

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}

		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}

			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	client := New()
	client.ChatBaseCN = srv.URL
	client.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	rc, status, _, err := client.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)

	var (
		got      string
		gotSb391 strings.Builder
	)

	for i := range frames {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}

		gotSb391.WriteString(string(buf))
	}

	got += gotSb391.String()
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/billing/meter/get-user-resource") {
			return nil, fmt.Errorf("%s: %w", "wrong path: "+r.URL.Path, errTestUserResourceAggregationFailed)
		}

		if r.Method != http.MethodPost {
			return nil, errWantPOST
		}

		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, fmt.Errorf("%s: %w", "missing ProductCode: "+string(body), errTestUserResourceAggregationFailed)
		}

		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	remain, total, err := client.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}

	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}

	if total != 3000 {
		t.Errorf("total=%d want 3000", total)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})

	remain, total, err := client.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}

	if total != 100 {
		t.Errorf("total=%d want 100", total)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errWrongPath2
		}

		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})

	err := client.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

func TestBasesAlwaysCN(t *testing.T) {
	t.Parallel()

	client := testClient(nil)
	account := &auth.Auth{Domain: ""}
	other := &auth.Auth{Domain: "example.com"}

	if client.chatBase(account) != "https://chat.example" || client.billingBase(account) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	// 恒 CN：domain 不同不改变上游 host。
	if client.chatBase(other) != client.chatBase(account) || client.billingBase(other) != client.billingBase(account) {
		t.Error("bases must be CN regardless of domain")
	}
}

// TestChatBaseNormalizesConfiguredSuffix chatBase 是全部「chat 同源」端点的
// base 出口（images / modelbusy / v3/config / 任务 / 旅行）：配置允许 host /
// host/v2 / host/v2/chat/completions，出口必须先归一，否则这些路径会拼出
// /v2/v2/...。chat/search 此前的各自归一只是局部修复。
func TestChatBaseNormalizesConfiguredSuffix(t *testing.T) {
	t.Parallel()

	for _, base := range []string{
		"https://srv.example",
		"https://srv.example/v2",
		"https://srv.example/v2/chat/completions",
	} {
		client := testClient(nil)
		client.ChatBaseCN = base

		if got := client.chatBase(&auth.Auth{}); got != "https://srv.example" {
			t.Errorf("cn base=%q -> %q want https://srv.example", base, got)
		}
	}

	global := &auth.Auth{UID: "g1"}
	if _, err := auth.BackfillRealmFor(global, "global"); err != nil {
		t.Fatal(err)
	}

	client := testClient(nil)
	client.GlobalEnabled = true
	client.ChatBaseGlobal = "https://global.example/v2/chat/completions"

	if got := client.chatBase(global); got != "https://global.example" {
		t.Errorf("global base -> %q want https://global.example", got)
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	t.Parallel()

	client := New()
	if client.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}

	if client.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", client.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if client.ChatHTTP.Transport != client.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}

	htr, ok := client.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", client.ChatHTTP.Transport)
	}

	if htr.ResponseHeaderTimeout != 60*time.Second { // 连接层加固：响应头上限从 120s 收到 60s（慢冷启动留 3.75× 余量）
		t.Errorf("ResponseHeaderTimeout=%v want 60s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	t.Parallel()
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	client := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	client.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	rc, status, _, err := client.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}

	rc.Close()

	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}

	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if client.chatHTTP() != client.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

func TestFetchModelsDefaultEffortDualKeyAndSizes(t *testing.T) {
	t.Parallel()
	// 上游双键：老模型 reasoning.effort（auto），新模型（glm-5.3 系）只有 reasoning.defaultEffort；
	// credits/maxAllowedSize/canDisableThinking/supportsReasoning 等尺寸与能力字段应一并透出。
	client := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"glm-5.3","maxInputTokens":1000000,"maxOutputTokens":48000,"maxAllowedSize":1000000,"credits":"x0.79","supportsReasoning":true,"reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high","max"]}},
			{"id":"auto","maxInputTokens":168000,"maxOutputTokens":32000,"supportsReasoning":true,"reasoning":{"effort":"high"}}
		],"agents":[{"name":"cli","models":["glm-5.3","auto"]}]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	infos, err := client.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}

	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}

	assertDefaultEffortDualKey(t, byID, client)
}

// assertDefaultEffortDualKey 断言 defaultEffort 新键与 effort 老键双通道解析出的字段。
func assertDefaultEffortDualKey(t *testing.T, byID map[string]ModelInfo, client *Client) {
	t.Helper()

	modelInfo := byID["glm-5.3"]
	if modelInfo.DefaultEffort != "high" {
		t.Errorf("glm-5.3 DefaultEffort=%q want high (from defaultEffort key)", modelInfo.DefaultEffort)
	}

	if !modelInfo.CanDisableThinking || !modelInfo.SupportsReasoning {
		t.Errorf("glm-5.3 capability flags: canDisable=%v supportsReasoning=%v want true/true", modelInfo.CanDisableThinking, modelInfo.SupportsReasoning)
	}

	if modelInfo.MaxAllowedSize != 1000000 || modelInfo.MaxTokens != 48000 || modelInfo.Credits != "x0.79" {
		t.Errorf("glm-5.3 sizes: maxAllowed=%d maxOut=%d credits=%q", modelInfo.MaxAllowedSize, modelInfo.MaxTokens, modelInfo.Credits)
	}

	if got := client.ModelRate("cn", "glm-5.3"); got != "0.79" {
		t.Errorf("glm-5.3 ModelRate=%q want 0.79", got)
	}

	if au := byID["auto"]; au.DefaultEffort != "high" {
		t.Errorf("auto DefaultEffort=%q want high (from legacy effort key)", au.DefaultEffort)
	}
}

func TestModelRateCacheEffectiveAndNormalized(t *testing.T) {
	t.Parallel()

	client := New()
	factor := 0.5
	client.storeModelRates("cn", []ModelInfo{
		{ID: "base", Credits: "x0.50 credits"},
		{ID: "promo", Credits: "x0.80", PromoFactor: &factor, PromoCredits: "0.50x"},
	})

	if got := client.ModelRate("cn", "base"); got != "0.5" {
		t.Fatalf("base rate=%q want 0.5", got)
	}

	if got := client.ModelRate("cn", "promo"); got != "0.5" {
		t.Fatalf("promo rate=%q want 0.5", got)
	}

	if got := normalizeModelRate("x0.05 credits"); got != "0.05" {
		t.Fatalf("normalizeModelRate=%q want 0.05", got)
	}

	client.storeModelRates("cn", []ModelInfo{{ID: "base", Credits: "x0.79"}})

	if got := client.ModelRate("cn", "base"); got != "0.79" {
		t.Fatalf("refreshed base rate=%q want 0.79", got)
	}

	if got := client.ModelRate("cn", "promo"); got != "" {
		t.Fatalf("stale promo rate=%q want empty after full refresh", got)
	}
}

func TestFetchModelsOverlaysV3ConfigCapabilities(t *testing.T) {
	t.Parallel()
	// CLI 目录给 flash 精简字段（128K / 固定 high）；IDE /v3/config 给完整能力。
	var sawIDE atomic.Bool

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03 credits","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"effort":"high","summary":"auto"}}
			],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			sawIDE.Store(true)

			assertV3ConfigHeaders(t, r)

			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":393216,"credits":"x0.03","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"canDisableThinking":true,"defaultEffort":"high","summary":"auto","supportedEfforts":["low","high","max"]}}
			]}}`), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return jsonResp(404, `{}`), nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}

	infos, err := client.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}

	if !sawIDE.Load() {
		t.Fatal("expected /v3/config request")
	}

	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}

	assertV3Overlay(t, infos[0])
}

// assertV3ConfigHeaders 断言 /v3/config 请求带 IDE UA 与 SaaS 产品头。
func assertV3ConfigHeaders(t *testing.T, r *http.Request) {
	t.Helper()

	if ua := r.Header.Get("User-Agent"); ua != codeBuddyIDEUA && !strings.Contains(ua, "WorkBuddy/") {
		t.Errorf("v3/config UA=%q want IDE or desktop UA", ua)
	}

	if r.Header.Get("X-Product") != "SaaS" {
		t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
	}

	if r.Header.Get("X-User-Id") != "u1" {
		t.Errorf("X-User-Id=%q want u1", r.Header.Get("X-User-Id"))
	}
}

// assertV3Overlay 断言 /v3/config 的完整能力字段覆盖了 CLI 精简字段。
func assertV3Overlay(t *testing.T, modelInfo ModelInfo) {
	t.Helper()

	if modelInfo.MaxTokens != 393216 {
		t.Errorf("MaxTokens=%d want 393216", modelInfo.MaxTokens)
	}

	if modelInfo.ContextWindow != 1000000 {
		t.Errorf("ContextWindow=%d want 1000000", modelInfo.ContextWindow)
	}

	if !modelInfo.CanDisableThinking || !modelInfo.SupportsReasoning {
		t.Errorf("flags canDisable=%v supportsReasoning=%v", modelInfo.CanDisableThinking, modelInfo.SupportsReasoning)
	}

	if modelInfo.DefaultEffort != "high" {
		t.Errorf("DefaultEffort=%q want high", modelInfo.DefaultEffort)
	}

	if got := strings.Join(modelInfo.Efforts, ","); got != "low,high,max" {
		t.Errorf("Efforts=%v want low,high,max", modelInfo.Efforts)
	}
}

// TestUserResourceNoPrefixFirstFallsBackToV2 官方 5.9.3 桌面 resourcePrefix 为空：
// get-user-resource 先发无前缀路径，404 才回落 `/v2`。
func TestUserResourceNoPrefixFirstFallsBackToV2(t *testing.T) {
	t.Parallel()

	var paths []string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "get-user-resource-summary"):
			return jsonResp(http.StatusNotFound, `{"code":404}`), nil
		case strings.HasSuffix(r.URL.Path, "/billing/meter/get-user-resource"):
			paths = append(paths, r.URL.Path)
			if len(paths) == 1 {
				return jsonResp(http.StatusNotFound, `{"code":404}`), nil
			}

			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
				{"CycleCapacitySize":1000,"CycleCapacityRemain":700,"CycleCapacityUsed":300}
			]}}}}`), nil
		default:
			return jsonResp(http.StatusNotFound, `{"code":404}`), nil
		}
	})

	remain, total, err := client.UserResource(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil || remain != 700 || total != 1000 {
		t.Fatalf("remain=%d total=%d err=%v want 700/1000", remain, total, err)
	}

	want := []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths=%v want %v（无前缀优先，404 回落 /v2）", paths, want)
	}
}

// TestClassifyRealChannelRefusedBody 用上游真实 11-128 响应体（code 为字符串
// "11-128"，带连字符）钉住通道拒绝分流。护栏来历：上游 PR #233 的实现与测试
// 曾一起把连字符写丢，双方自洽所以全绿，线上却永远命中不了真实业务码。
// 这里用码点构造业务码，防止「实现和测试一起写错」再次通过。
//
// 注意与出站 sanitize 的 5 位裸数字反探测串区分：后者是上游对请求体的反探测，
// 本用例钉的是上游响应里的业务码。
func TestClassifyRealChannelRefusedBody(t *testing.T) {
	t.Parallel()

	code := string([]rune{49, 49, 45, 49, 50, 56}) // "11-128"
	body := `{"code":"` + code + `","msg":"request illegal","requestId":"b1bc87ea","displayTips":{"en":"This call failed channel verification."}}`

	if got := Classify(http.StatusBadRequest, body); got != ErrChannelRefused {
		t.Fatalf("Classify(real 11-128)=%v want ErrChannelRefused", got)
	}

	if got := Classify(http.StatusForbidden, `{"code":"`+code+`","msg":"request illegal"}`); got != ErrChannelRefused {
		t.Fatalf("Classify(real 11-128, 403)=%v want ErrChannelRefused", got)
	}
}
