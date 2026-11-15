package jsonx

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"
	"testing"
)

// TestMarshalDeterministicMapOrder 编码口径：map 键按字典序输出（v1 口径）。
func TestMarshalDeterministicMapOrder(t *testing.T) {
	t.Parallel()

	out, err := Marshal(map[string]int{"z": 1, "a": 2, "m": 3})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if got, want := string(out), `{"a":2,"m":3,"z":1}`; got != want {
		t.Fatalf("Marshal=%s want %s", got, want)
	}
}

// TestMarshalNilContainerAsNull nil slice/map 输出 null（v1 口径），不变成空容器。
func TestMarshalNilContainerAsNull(t *testing.T) {
	t.Parallel()

	out, err := Marshal(struct {
		S []string          `json:"s"`
		M map[string]string `json:"m"`
	}{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if got, want := string(out), `{"s":null,"m":null}`; got != want {
		t.Fatalf("Marshal=%s want %s", got, want)
	}
}

// TestMarshalIndentTwoSpaces 落盘格式：两空格缩进、无尾随换行。
func TestMarshalIndentTwoSpaces(t *testing.T) {
	t.Parallel()

	out, err := MarshalIndent(map[string]int{"a": 1})
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}

	if got, want := string(out), "{\n  \"a\": 1\n}"; got != want {
		t.Fatalf("MarshalIndent=%q want %q", got, want)
	}
}

// TestUnmarshalCaseInsensitiveNames 解码口径：字段名大小写/下划线差异照 v1 容忍。
func TestUnmarshalCaseInsensitiveNames(t *testing.T) {
	t.Parallel()

	var got struct {
		MaxTokens int `json:"max_tokens"`
	}

	if err := Unmarshal([]byte(`{"MaxTokens":7}`), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.MaxTokens != 7 {
		t.Fatalf("MaxTokens=%d want 7", got.MaxTokens)
	}
}

// TestUnmarshalAllowsDuplicateNames 重复键按出现顺序覆盖（v1 口径），不拦成 400。
func TestUnmarshalAllowsDuplicateNames(t *testing.T) {
	t.Parallel()

	var got map[string]int
	if err := Unmarshal([]byte(`{"a":1,"a":2}`), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got["a"] != 2 {
		t.Fatalf("a=%d want 2（后者覆盖）", got["a"])
	}
}

// TestUnmarshalRejectsInvalidUTF8 非法 UTF-8 属于数据损坏，解码拒绝。
func TestUnmarshalRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()

	var got map[string]string
	if err := Unmarshal([]byte("{\"a\":\"\xff\"}"), &got); err == nil {
		t.Fatalf("非法 UTF-8 应报错，got=%q", got)
	}
}

// TestUnmarshalRejectsTrailingData 单个值之后不得有非空白内容。
func TestUnmarshalRejectsTrailingData(t *testing.T) {
	t.Parallel()

	var got map[string]int
	if err := Unmarshal([]byte(`{"a":1} garbage`), &got); err == nil {
		t.Fatal("尾随内容应报错")
	}
}

// TestDecodeAnyKeepsNumberLiterals 通用值解码保留数字字面量（种子/大整数不被截断）。
func TestDecodeAnyKeepsNumberLiterals(t *testing.T) {
	t.Parallel()

	value, err := DecodeAny([]byte(`{"big":12345678901234567890,"frac":1.50,"arr":[1,-2.5]}`))
	if err != nil {
		t.Fatalf("DecodeAny: %v", err)
	}

	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value=%T want map[string]any", value)
	}

	if got, want := jsonLiteral(obj["big"]), "12345678901234567890"; got != want {
		t.Fatalf("big=%q want %s", got, want)
	}

	if got, want := jsonLiteral(obj["frac"]), "1.50"; got != want {
		t.Fatalf("frac=%q want %s", got, want)
	}

	arr, ok := obj["arr"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("arr=%#v want 两个元素", obj["arr"])
	}

	if got, ok := Int64(arr[1]); !ok || got != -2 {
		t.Fatalf("arr[1]=%#v want -2", arr[1])
	}
}

// jsonLiteral 取通用值里的 JSON 数字字面量文本；非字面量返回空串。
func jsonLiteral(v any) string {
	raw, ok := v.(jsontext.Value)
	if !ok {
		return ""
	}

	return string(raw)
}

// TestDecodeAnyRejectsTrailingData 粘帧不由通用值解码吞掉。
func TestDecodeAnyRejectsTrailingData(t *testing.T) {
	t.Parallel()

	if _, err := DecodeAny([]byte(`{}{}`)); !errors.Is(err, ErrTrailingData) {
		t.Fatalf("err=%v want ErrTrailingData", err)
	}
}

// TestDecodeObjectNullIsNil 顶层 null 解码为 nil map（v1 口径）。
func TestDecodeObjectNullIsNil(t *testing.T) {
	t.Parallel()

	obj, err := DecodeObject([]byte(`null`))
	if err != nil || obj != nil {
		t.Fatalf("obj=%#v err=%v want nil,nil", obj, err)
	}
}

// TestValidLenient v1 口径的合法性判定：重复键与非法 UTF-8 都不算语法错误。
func TestValidLenient(t *testing.T) {
	t.Parallel()

	if !Valid([]byte(`{"a":1,"a":2}`)) {
		t.Fatal("重复键应判为合法")
	}

	if !Valid([]byte("{\"a\":\"\xff\"}")) {
		t.Fatal("非法 UTF-8 应判为合法（v1 口径）")
	}

	if Valid([]byte(`{"a":}`)) {
		t.Fatal("截断 JSON 应判为非法")
	}
}

// TestUnmarshalReadRejectsTrailingData 读满一个值后，尾部非空白数据视为错误。
func TestUnmarshalReadRejectsTrailingData(t *testing.T) {
	t.Parallel()

	var got map[string]int
	if err := UnmarshalRead(strings.NewReader(`{"a":1} trailing`), &got); err == nil {
		t.Fatal("尾随内容应报错")
	}
}

// TestUnmarshalRejectUnknownMembers 严格模式：未知字段报错（clientprofile 使用）。
func TestUnmarshalRejectUnknownMembers(t *testing.T) {
	t.Parallel()

	var got struct {
		A int `json:"a"`
	}

	if err := Unmarshal([]byte(`{"a":1,"b":2}`), &got, jsonv2.RejectUnknownMembers(true)); err == nil {
		t.Fatal("未知字段应报错")
	}
}
