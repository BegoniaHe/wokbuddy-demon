package upstream

import (
	"reflect"
	"testing"
)

// TestNormalizeModalities 归一化：小写、去重、过滤未知值、保持顺序。
func TestNormalizeModalities(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"text", "image"}, []string{"text", "image"}},
		{[]string{" Text ", "IMAGE", "text"}, []string{"text", "image"}},
		{[]string{"text", "bogus"}, []string{"text"}},
		{[]string{"", "  "}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		if got := normalizeModalities(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("normalizeModalities(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

// TestEffectiveInputModalitiesPriority 来源优先级：上游字段 > models.dev >
// supports_images 推导；三级都无 → nil（未知，不宣称）。
func TestEffectiveInputModalitiesPriority(t *testing.T) {
	// 1) 上游字段优先。
	if got := (ModelInfo{ID: "m", InputModalities: []string{"text", "audio"}, SupportsImages: true}).EffectiveInputModalities(); !reflect.DeepEqual(got, []string{"text", "audio"}) {
		t.Errorf("upstream field priority: got %v", got)
	}
	// 2) models.dev 兜底（注入进程内索引）。
	resetModelsDev()
	t.Cleanup(resetModelsDev)
	modelsDev.mu.Lock()
	modelsDev.modalities = map[string][]string{"m": {"text", "file"}}
	modelsDev.mu.Unlock()
	if got := (ModelInfo{ID: "m"}).EffectiveInputModalities(); !reflect.DeepEqual(got, []string{"text", "file"}) {
		t.Errorf("models.dev fallback: got %v", got)
	}
	// 3) supports_images 兼容推导。
	if got := (ModelInfo{ID: "other", SupportsImages: true}).EffectiveInputModalities(); !reflect.DeepEqual(got, []string{"text", "image"}) {
		t.Errorf("supports_images derivation: got %v", got)
	}
	// 4) 未知：不宣称。
	if got := (ModelInfo{ID: "other"}).EffectiveInputModalities(); got != nil {
		t.Errorf("unknown model must return nil, got %v", got)
	}
}

// TestSupportsModalityUnknownAllows 能力未知时 SupportsModality 放行（不拒绝）。
func TestSupportsModalityUnknownAllows(t *testing.T) {
	if !(ModelInfo{ID: "unknown"}).SupportsModality(ModalityImage) {
		t.Error("unknown capability must allow (cannot prove unsupported)")
	}
	known := ModelInfo{ID: "text-only", InputModalities: []string{"text"}}
	if known.SupportsModality(ModalityImage) {
		t.Error("text-only must not claim image")
	}
	if !known.SupportsModality(ModalityText) {
		t.Error("text-only must claim text")
	}
}

// TestRequestPartModality 请求 part 类型映射。
func TestRequestPartModality(t *testing.T) {
	cases := map[string]string{
		"image_url":   ModalityImage,
		"input_image": ModalityImage,
		"input_audio": ModalityAudio,
		"audio":       ModalityAudio,
		"file":        ModalityFile,
		"input_file":  ModalityFile,
		"text":        "",
		"bogus":       "",
	}
	for in, want := range cases {
		if got := RequestPartModality(in); got != want {
			t.Errorf("RequestPartModality(%q)=%q want %q", in, got, want)
		}
	}
}

// TestDynModelEntryModalities 上游两种字段形态：扁平 inputModalities 优先，
// 缺失回落 modalities.input。
func TestDynModelEntryModalities(t *testing.T) {
	var e dynModelEntry
	e.InputModalities = []string{"text", "audio"}
	e.Modalities.Input = []string{"text", "image"}
	if got := e.modelInfo().InputModalities; !reflect.DeepEqual(got, []string{"text", "audio"}) {
		t.Errorf("flat inputModalities priority: got %v", got)
	}
	var e2 dynModelEntry
	e2.Modalities.Input = []string{"text", "file"}
	if got := e2.modelInfo().InputModalities; !reflect.DeepEqual(got, []string{"text", "file"}) {
		t.Errorf("modalities.input fallback: got %v", got)
	}
}

// TestParseModelsDevModalities vendor 官方源优先；否则 provider 字典序最小；
// 无名/无模态条目忽略。
func TestParseModelsDevModalities(t *testing.T) {
	raw := []byte(`{
		"zeta":  {"models": {"m": {"modalities": {"input": ["text", "image"]}}}},
		"alpha": {"models": {"m": {"modalities": {"input": ["text", "audio"]}}}},
		"zai":   {"models": {"m": {"modalities": {"input": ["text", "file"]}}}},
		"other": {"models": {"n": {"modalities": {"output": ["text"]}}}}
	}`)
	got, err := parseModelsDevModalities(raw)
	if err != nil {
		t.Fatal(err)
	}
	// zai 是官方 vendor 源：即使字典序不是最小也优先。
	if !reflect.DeepEqual(got["m"], []string{"text", "file"}) {
		t.Errorf("m=%v want vendor zai value", got["m"])
	}
	if _, ok := got["n"]; ok {
		t.Errorf("模型无 input 模态时不应入索引")
	}
}

// TestLookupModelsDevModalitiesNotLoaded 文档未加载：ok=false，不触发网络。
func TestLookupModelsDevModalitiesNotLoaded(t *testing.T) {
	resetModelsDev()
	t.Cleanup(resetModelsDev)
	if _, ok := lookupModelsDevModalities("m"); ok {
		t.Error("未加载文档时应返回 ok=false")
	}
}
