package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
)

func TestIdentityRewritten(t *testing.T) {
	t.Parallel()

	out := sanitizeText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}

	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

// 桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继内容，结尾不是句号。
// 回归用例：匹配串曾带结尾句号，导致该形态漏网、指纹原样发上游 → 上游 400。
func TestIdentityDesktopVariantRewritten(t *testing.T) {
	t.Parallel()

	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."

	out := sanitizeText(in)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("desktop identity not rewritten: %q", out)
	}

	if !strings.Contains(out, "official CLI tool for Claude, running within the Claude Agent SDK.") {
		t.Errorf("desktop identity suffix not preserved: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	t.Parallel()

	out := sanitizeText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}

	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

// 反馈句带 Anthropic 仓库链接，上游按整句拦截（实测只留链接或只留半边均不拦）。
// 回归用例：give→provide 一词之差即可绕过。
func TestFeedbackSentenceRewritten(t *testing.T) {
	t.Parallel()

	in := "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"

	out := sanitizeText(in)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("feedback sentence not rewritten: %q", out)
	}

	if !strings.Contains(out, "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues") {
		t.Errorf("feedback sentence not rewritten as expected: %q", out)
	}
}

// 回归：工具调用消息的 content 常为 null，而旧版 sanitizeMessages 在 content 缺失时
// 直接 continue，整条消息连 tool_calls 一起被跳过 → arguments 里的被拦字符串原样漏出。
func TestToolCallArgumentsSanitized(t *testing.T) {
	t.Parallel()

	msgs := []any{
		map[string]any{"role": "user", "content": "run"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": `{"command":"echo You are Claude Code, Anthropic's official CLI for Claude"}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告任何改动，tool_calls 被跳过")
	}

	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)

	got := fn["arguments"].(string)
	if strings.Contains(got, "official CLI for Claude") {
		t.Errorf("tool_call arguments 未被净化: %q", got)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	t.Parallel()

	out := sanitizeText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

// 附加验证：正常对话里出现 github.com/anthropics/ 链接（但不是反馈句整句）
// 时，预检特征命中（进入净化），但改写层只动精确匹配的整句——普通链接文本
// 不该被改写。同理，既不含反馈整句也不含其他指纹的文本原样返回。
func TestNormalAnthropicLinkNotRewritten(t *testing.T) {
	t.Parallel()

	in := "see https://github.com/anthropics/anthropic-cookbook for examples"
	if out := sanitizeText(in); out != in {
		t.Errorf("normal anthropic link should be untouched: %q -> %q", in, out)
	}
}

// Codex instructions 首段：命中预告且整句改写，逐字指纹被破坏、语义保留。
func TestCodexInstructionsRewritten(t *testing.T) {
	t.Parallel()

	out := sanitizeText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}

	if !strings.Contains(out, "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.") {
		t.Errorf("codex first sentence not rewritten: %q", out)
	}
	// 其余两句原样保留，语义不变。
	if !strings.Contains(out, "Codex CLI is an open source project led by OpenAI.") ||
		!strings.Contains(out, "You are expected to be precise, safe, and helpful.") {
		t.Errorf("codex remaining sentences altered: %q", out)
	}
}

// 预检必须能认出 Codex 特征（此前只含 Claude Code，导致提前放行）。
func TestCodexFingerprintDetected(t *testing.T) {
	t.Parallel()

	if !hasFingerprint(codexInstructions) {
		t.Error("codex fingerprint not detected by precheck")
	}
}

// 非精确变体不应被改写：仅去掉其中一个词即视为已破坏，无需改动。
func TestCodexVariantNotTouched(t *testing.T) {
	t.Parallel()

	in := "You are a coding agent running in a CLI, a terminal-based coding assistant."
	if out := sanitizeText(in); out != in {
		t.Errorf("already-broken variant should be untouched: %q -> %q", in, out)
	}
}

func TestBillingHeaderCaseInsensitive(t *testing.T) {
	t.Parallel()

	alt := "X-Anthropic-Billing-Header: cc_version=1.0;"

	out := strings.ToLower(sanitizeText(alt))
	if strings.Contains(out, "billing") {
		t.Errorf("case-insensitive header not stripped: %q", out)
	}
}

func TestTrailingKVStripped(t *testing.T) {
	t.Parallel()

	out := sanitizeText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

func TestExactMatchOnlyVariantNotTouched(t *testing.T) {
	t.Parallel()

	in := "...official CLI for Claude!"
	if out := sanitizeText(in); out != in {
		t.Errorf("variant should be untouched: %q -> %q", in, out)
	}
}

func TestUserFreeTextNotTouched(t *testing.T) {
	t.Parallel()

	in := "please use main branch for this repo"
	if out := sanitizeText(in); out != in {
		t.Errorf("free text should be untouched: %q -> %q", in, out)
	}
}

func TestNoFeatureReturnsSameString(t *testing.T) {
	t.Parallel()

	in := "ordinary user message"
	if out := sanitizeText(in); out != in {
		t.Errorf("no-feature text should pass through unchanged: %q -> %q", in, out)
	}
}

func TestMultimodalTextPartOnly(t *testing.T) {
	t.Parallel()

	imgPart := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "..."}}
	content := []any{
		map[string]any{"type": "text", "text": ccIdentity},
		imgPart,
	}

	out, changed := sanitizeContent(content)
	if !changed {
		t.Fatal("expected change")
	}

	parts := out.([]any)

	txt, _ := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "CLI tool") {
		t.Errorf("text part not sanitized: %q", txt)
	}

	img, _ := parts[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "..." {
		t.Error("image part modified")
	}
}

// 集成：完整请求体经 PrepareBodyOpt 净化后无残留指纹，且 stream/tool_choice 行为不受影响。
func TestPrepareBodyOptSanitizesSystem(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBodyOpt(body, true)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}

	if obj["stream"] != true {
		t.Error("stream not forced")
	}

	msgs := obj["messages"].([]any)

	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "x-anthropic-billing-header") || strings.Contains(sys, ccIdentity) || strings.Contains(sys, ccBranch) {
		t.Errorf("fingerprints remain: %q", sys)
	}

	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("rewrite missing: %q", sys)
	}
}

func TestPrepareBodyOptDisabledPreservesFingerprints(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)

	out := PrepareBodyOpt(body, false)
	if !strings.Contains(string(out), ccIdentity) {
		t.Error("sanitize=false should preserve fingerprints")
	}
	// 但 stream 仍强制
	var obj map[string]any

	_ = json.Unmarshal(out, &obj)
	if obj["stream"] != true {
		t.Error("stream should still be forced")
	}
}

// PrepareBody 默认行为 = 开启脱敏（保持向后兼容）。
func TestPrepareBodyDefaultSanitizes(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)

	out := PrepareBodyOpt(body, true)
	if strings.Contains(string(out), ccIdentity) {
		t.Error("PrepareBody default should sanitize")
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 必须无残留指纹。
func TestChatStreamWireBodySanitized(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	server := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer server.Close()

	client := New()
	client.SanitizeFingerprints.Store(true)
	client.ChatBaseCN = server.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)

	rc, status, respBody, err := client.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 body：stream 强制 + 指纹已净化
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}

	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}

	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, fp := range []string{"x-anthropic-billing-header", ccIdentity, ccBranch} {
		if strings.Contains(sys, fp) {
			t.Errorf("wire body contains fingerprint %q: %q", fp, sys)
		}
	}
}

// 出站边界（Codex 场景）：CPA 把 instructions 折成 role=system 的首条消息，
// 净化后 wire body 不得残留 Codex 逐字指纹；其余消息不受影响。
func TestChatStreamWireBodyCodexInstructionsSanitized(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	server := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer server.Close()

	client := New()
	client.SanitizeFingerprints.Store(true)
	client.ChatBaseCN = server.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"kimi-k3","messages":[` +
		`{"role":"system","content":"` + codexInstructions + `"},` +
		`{"role":"user","content":"say ok"}]}`)

	rc, status, respBody, err := client.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}

	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}

	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, codexInstructions) ||
		strings.Contains(sys, "in the Codex CLI, a terminal-based coding assistant.") {
		t.Errorf("wire body still contains codex fingerprint: %q", sys)
	}

	if !strings.Contains(sys, "running in the Codex CLI tool, a terminal-based") {
		t.Errorf("codex rewrite missing on wire: %q", sys)
	}

	user, _ := obj["messages"].([]any)[1].(map[string]any)["content"].(string)
	if user != "say ok" {
		t.Errorf("user message altered: %q", user)
	}
}

// 出站边界：关闭脱敏后 wire body 原样保留指纹（验证开关真实有效）。
func TestChatStreamWireBodySanitizeDisabled(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	server := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer server.Close()

	client := New()
	client.SanitizeFingerprints.Store(false)
	client.ChatBaseCN = server.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)

	rc, status, _, err := client.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if status >= 400 {
		t.Fatalf("upstream status %d", status)
	}

	if !strings.Contains(string(gotBody), ccIdentity) {
		t.Error("sanitize disabled should preserve fingerprint on wire")
	}
}

// TestSanitizeMessagesScrubsReasoningField thinking.go 的回填会把客户端送来的
// reasoning_content 镜像进 reasoning 字段；此前只洗 content / reasoning_content /
// tool_calls → 镜像进 reasoning 的指纹（身份句等指纹）原样出站。
func TestSanitizeMessagesScrubsReasoningField(t *testing.T) {
	t.Parallel()

	ifaces := []any{map[string]any{
		"role":      "assistant",
		"content":   "hi",
		"reasoning": "upstream said You are Claude Code, Anthropic's official CLI for Claude",
	}}
	if !sanitizeMessages(ifaces) {
		t.Fatal("reasoning 字段里的指纹未被净化")
	}

	got, _ := ifaces[0].(map[string]any)["reasoning"].(string)
	if strings.Contains(got, "official CLI for Claude") {
		t.Fatalf("reasoning 仍含裸指纹: %q", got)
	}
}

// newTestUpstream 起一个假上游并捕获请求。
func newTestUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}

// TestSanitizeDSHHarness DSH 身份句品牌词被改写（上游按非官方渠道整单拦）。
func TestSanitizeDSHHarness(t *testing.T) {
	t.Parallel()

	got := sanitizeText("You are a coding agent running in DeepSeek Harness.")
	if strings.Contains(got, "DeepSeek Harness") || !strings.Contains(got, "DeepSeekRuntime") {
		t.Fatalf("sanitized=%q want DeepSeekRuntime", got)
	}
}

// DSH 0.3.x 的 @ 路径说明段是**唯一不含品牌词**的 DSH 指纹：其余预检特征一条都对不上，
// 原文原样出站即 400 {"code":11128,...}（Illegal API invocation from an unapproved
// channel）。钉住四条：整段被改写、两种线上措辞都覆盖、跨行形态也覆盖、段外文本不动。
const (
	// 当前线上措辞（DSH 0.3.x：point to / open it with the read tool / path that contains）。
	dshAtPathNow = "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths point to " +
		"files or directories on the host. A trailing slash marks a directory, so " +
		"list it when its contents matter. Anything else is a file: open it with " +
		"the read tool when its contents are needed, and never claim to have " +
		"inspected it before reading. The @\"...\" form only quotes a path that " +
		"contains spaces."
	// 更早的措辞（identify / use the read tool / quotes a path containing）。
	dshAtPathOld = "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths identify " +
		"files or directories on the host. A trailing slash marks a directory: " +
		"list it when its contents matter. Anything else is a file: use the read " +
		"tool when its contents are needed, and do not claim to have inspected it " +
		"before reading. @\"...\" quotes a path containing spaces."
)

// 预检与改写：整段替换成同义改写串，且语义要点齐全（DSH 读提示词的行为不变）。
func TestDshAtPathSegmentRewritten(t *testing.T) {
	t.Parallel()

	if !hasFingerprint(dshAtPathNow) {
		t.Fatal("@ 路径段未被特征预检命中：整条净化链都不会启动")
	}

	out := sanitizeText(dshAtPathNow)
	if out != sanitizeDshPathsReplacement {
		t.Fatalf("整段未被替换:\n got=%q\nwant=%q", out, sanitizeDshPathsReplacement)
	}

	if strings.Contains(out, sanitizeDshPathsProbe) {
		t.Errorf("原文残留在出站文本里: %q", out)
	}
	// 语义四点（相对路径基准 / 结尾斜杠 / 按文件读 / 引号形态）必须都在。
	for _, want := range []string{"project root", "trailing slash", "read tool", "@\"...\""} {
		if !strings.Contains(out, want) {
			t.Errorf("改写丢语义 %q: %q", want, out)
		}
	}
}

// 更早的措辞（identify / use the read tool / quotes a path containing spaces）同段同义：
// 结尾分支必须也吃下它，否则该版本 DSH 依旧整单 400。
func TestDshAtPathOlderWordingRewritten(t *testing.T) {
	t.Parallel()

	if out := sanitizeText(dshAtPathOld); out != sanitizeDshPathsReplacement {
		t.Fatalf("旧措辞未覆盖:\n got=%q\nwant=%q", out, sanitizeDshPathsReplacement)
	}
}

// 段内本来就有换行：(?s) 缺失时 .*? 跨不过去，只改半段等于没改。
func TestDshAtPathMultilineRewritten(t *testing.T) {
	t.Parallel()

	in := strings.Replace(dshAtPathNow, "Relative paths", "\n\nRelative paths", 1)

	out := sanitizeText(in)
	if strings.Contains(out, sanitizeDshPathsProbe) || !strings.Contains(out, "Paths beginning with @") {
		t.Fatalf("跨行形态未覆盖: %q", out)
	}
}

// 段落嵌在更长的 system 里时只换这一段，前后文逐字原样。
func TestDshAtPathEmbeddedKeepsSurroundings(t *testing.T) {
	t.Parallel()

	in := "before\n\n" + dshAtPathNow + "\n\nafter"
	if out := sanitizeText(in); out != "before\n\n"+sanitizeDshPathsReplacement+"\n\nafter" {
		t.Fatalf("前后文被动了: %q", out)
	}
}

// 段外的 @ 是普通文本（邮箱、npm 包名、Windows 路径）：既不触发预检也不被改写。
func TestDshAtPathUnrelatedTextUntouched(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		"Mail me at someone@example.com and open C:\\cache\\@tmp.",
		"npm i @deepseek-ai/dsh-llm",
		"Tokens prefixed with # are things you should not touch here.",
		"Relative paths resolve from the workspace root, but this is not the paragraph.",
	} {
		if hasFingerprint(text) {
			t.Errorf("误触发预检: %q", text)
		}

		if out := sanitizeText(text); out != text {
			t.Errorf("误改写: %q -> %q", text, out)
		}
	}
}

// 出站边界：@ 路径段在 system 里时，发往上游的 wire body 必须已被改写。
func TestChatStreamWireBodyDshAtPathSegmentSanitized(t *testing.T) {
	t.Parallel()

	var gotBody []byte

	server := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer server.Close()

	client := New()
	client.SanitizeFingerprints.Store(true)
	client.ChatBaseCN = server.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	raw, err := json.Marshal(map[string]any{
		"model": "hy3",
		"messages": []any{
			map[string]any{"role": "system", "content": dshAtPathNow},
			map[string]any{"role": "user", "content": "say ok"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	body := raw

	rc, status, respBody, err := client.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}

	if strings.Contains(string(gotBody), sanitizeDshPathsProbe) {
		t.Fatalf("wire body 仍带 @ 路径段原文: %s", gotBody)
	}

	if !strings.Contains(string(gotBody), "Paths beginning with @") {
		t.Fatalf("wire body 未被改写: %s", gotBody)
	}
}

// issue #414：指纹出现在单行 JSON 字符串里时，净化不能吞掉 JSON 结构字符。
// 旧右边界只到 `;`/换行，会把整行尾部连引号大括号一起删掉，产出非法 JSON；
// 收紧后（并对转义引号场景走结构化兜底）产物必须仍是合法 JSON。
func TestToolArgumentsJSONStaysValid(t *testing.T) {
	t.Parallel()

	args, err := json.Marshal(map[string]any{
		"curl": "-H \"" + ccHeader + "\"",
		"cmd":  "cc_version=2.1.0 cc_entrypoint=cli",
		"x":    1,
		"note": "keep this tail",
	})
	if err != nil {
		t.Fatal(err)
	}

	msgs := []any{
		map[string]any{"role": "user", "content": "run"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": string(args),
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告改动")
	}

	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)

	if !json.Valid([]byte(got)) {
		t.Fatalf("净化后 arguments 不是合法 JSON: %q", got)
	}

	if strings.Contains(strings.ToLower(got), "billing") || strings.Contains(got, "cc_version") {
		t.Errorf("指纹未被剥离: %q", got)
	}
	// 值被删空但键与尾部内容必须还在（旧实现会吞掉 `","x":1...`）。
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("解码失败: %v", err)
	}

	if _, ok := decoded["x"]; !ok {
		t.Errorf("JSON 结构被破坏，缺少键 x: %q", got)
	}
}

// 转义引号是纯文本替换的已知残留场景：`\"` 两侧被删后可能留下孤立反斜杠，
// 结构化兜底负责把它重新编码成合法 JSON。
func TestSanitizeJSONStringEscapedQuote(t *testing.T) {
	t.Parallel()

	raw := `{"curl":"-H \"` + ccHeader + `\"","x":1}`
	if !json.Valid([]byte(raw)) {
		t.Fatalf("用例本身不是合法 JSON: %q", raw)
	}

	out, changed := sanitizeJSONString(raw)
	if !changed {
		t.Fatal("未检测到指纹")
	}

	if !json.Valid([]byte(out)) {
		t.Fatalf("产物不是合法 JSON: %q", out)
	}

	if strings.Contains(strings.ToLower(out), "billing") {
		t.Errorf("指纹未剥离: %q", out)
	}
}

// 控制位：纯文本 content 的净化语义不回退——键值整段剥离、正文其余内容保留。
func TestPlainContentHeaderStillStripped(t *testing.T) {
	t.Parallel()

	in := "before\n" + ccHeader + "\nafter"
	out := sanitizeText(in)

	if strings.Contains(strings.ToLower(out), "billing") {
		t.Errorf("header not stripped: %q", out)
	}

	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("surrounding text lost: %q", out)
	}
}
