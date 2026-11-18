package prompt

import "testing"

// TestSystemFingerprint 指纹是「同 system 文本 → 同值、异文本 → 异值、无 system → 空串」，
// 且不改写调用方 body（issue #591）。
func TestSystemFingerprint(t *testing.T) {
	t.Parallel()

	bodyA := `{"model":"m","messages":[{"role":"system","content":"persona-a"},{"role":"user","content":"hi"}]}`
	bodyA2 := `{"model":"m2","messages":[{"role":"developer","content":"persona-a"},{"role":"user","content":"hi"}]}`
	bodyB := `{"model":"m","messages":[{"role":"system","content":"persona-b"},{"role":"user","content":"hi"}]}`
	noSys := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	fpA := SystemFingerprint([]byte(bodyA))
	if fpA == "" {
		t.Fatal("有 system 文本时指纹不应为空")
	}
	// 与 model / role（system 与 developer 同级）无关，只看 system 文本。
	if got := SystemFingerprint([]byte(bodyA2)); got != fpA {
		t.Errorf("同一段 system 文本的指纹应稳定: %q vs %q", got, fpA)
	}

	if got := SystemFingerprint([]byte(bodyB)); got == fpA {
		t.Errorf("不同 system 文本应得到不同指纹，都得到 %q", got)
	}

	if got := SystemFingerprint([]byte(noSys)); got != "" {
		t.Errorf("无 system 文本应返回空指纹，得到 %q", got)
	}

	if got := SystemFingerprint([]byte("not json")); got != "" {
		t.Errorf("不可解析 body 应返回空指纹，得到 %q", got)
	}

	if len(fpA) != systemFingerprintBytes*2 {
		t.Errorf("指纹应为 %d 位 hex，得到 %q", systemFingerprintBytes*2, fpA)
	}
}
