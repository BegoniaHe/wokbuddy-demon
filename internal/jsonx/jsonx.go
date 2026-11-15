// Package jsonx 统一本项目的 JSON 编解码口径。
//
// 调用点走这里而不是直接调 encoding/json/v2，原因是 v2 的默认语义与 v1 有几处
// 实质差异（map 键序、nil 容器、字段名匹配、原始数字），而本项目对其中每一项
// 都有既定口径；散在近两百个文件里逐点书写选项迟早漂移。
package jsonx

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// encodeOpts 编码口径：
//
//   - Deterministic：map 键按字典序输出。v1 一直如此；上游请求体、缓存键与
//     落盘状态都依赖稳定字节序，v2 默认不排序。
//   - FormatNilSliceAsNull / FormatNilMapAsNull：nil 容器输出 null（v1 口径），
//     不把「字段缺省」悄悄变成「空容器」。
//
// 不做 HTML 转义（v2 默认）：< > & 原样出站，与上游协议和面板响应的原始语义一致。
var encodeOpts = jsonv2.JoinOptions(
	jsonv2.Deterministic(true),
	jsonv2.FormatNilSliceAsNull(true),
	jsonv2.FormatNilMapAsNull(true),
)

// decodeOpts 解码口径：
//
//   - MatchCaseInsensitiveNames：沿用 v1 的字段名匹配口径。OpenAI 兼容客户端
//     存在大小写 / 下划线写法差异，换编码器不该把它们拦成 400。
//   - AllowDuplicateNames：v1 允许重复键（后者覆盖），保持入站行为不变。
//
// 不放开 AllowInvalidUTF8：非法 UTF-8 是数据损坏，v2 默认拒绝正是要的口径。
var decodeOpts = jsonv2.JoinOptions(
	jsonv2.MatchCaseInsensitiveNames(true),
	jsontext.AllowDuplicateNames(true),
)

// ErrTrailingData JSON 值之后还有非空白内容。
var ErrTrailingData = errors.New("json: 值之后存在多余数据")

// Marshal 序列化：map 键序稳定，nil 容器输出 null。
//
//nolint:wrapcheck // 薄封装：错误原样透出，调用方按 encoding/json/v2 的口径处理。
func Marshal(in any, opts ...jsonv2.Options) ([]byte, error) {
	return jsonv2.Marshal(in, withBase(encodeOpts, opts)...)
}

// MarshalIndent 序列化并两空格缩进（落盘配置 / 状态的既有格式）。
//
//nolint:wrapcheck // 薄封装：错误原样透出。
func MarshalIndent(in any, opts ...jsonv2.Options) ([]byte, error) {
	return jsonv2.Marshal(in, withBase(encodeOpts, append(opts, jsontext.WithIndent("  ")))...)
}

// MarshalWrite 序列化到 io.Writer，不追加换行。
//
//nolint:wrapcheck // 薄封装：错误原样透出（含 io.Writer 的写入错误）。
func MarshalWrite(out io.Writer, in any, opts ...jsonv2.Options) error {
	return jsonv2.MarshalWrite(out, in, withBase(encodeOpts, opts)...)
}

// Unmarshal 反序列化单个 JSON 值：容忍重复键与字段名大小写差异。
//
//nolint:wrapcheck // 薄封装：错误原样透出。
func Unmarshal(in []byte, out any, opts ...jsonv2.Options) error {
	return jsonv2.Unmarshal(in, out, withBase(decodeOpts, opts)...)
}

// UnmarshalRead 从 io.Reader 读满单个 JSON 值；尾部非空白内容视为错误。
//
//nolint:wrapcheck // 薄封装：错误原样透出（含读取错误）。
func UnmarshalRead(in io.Reader, out any, opts ...jsonv2.Options) error {
	return jsonv2.UnmarshalRead(in, out, withBase(decodeOpts, opts)...)
}

// Valid 报告 b 是否是单个合法 JSON 值（容忍重复键与非法 UTF-8，与 v1 的
// json.Valid 同口径）。
func Valid(b []byte) bool {
	return jsontext.Value(b).IsValid(
		jsontext.AllowDuplicateNames(true),
		jsontext.AllowInvalidUTF8(true),
	)
}

// DecodeAny 把 JSON 解码成通用 Go 值，数字保留原始字面量（jsontext.Value）。
//
// 对应 v1 的 Decoder.UseNumber：float64 会把 20 位整数截成另一个值，而改写类
// 路径（模型名替换 / 拆句 / 结构化净化 / 客户端配置回写）只是改结构，不能顺手
// 改掉客户端的数据。尾部非空白内容视为错误（粘帧由调用方自行拆分）。
func DecodeAny(in []byte) (any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(in), jsontext.AllowDuplicateNames(true))

	value, err := decodeAnyValue(dec)
	if err != nil {
		return nil, err
	}

	return value, ensureEOF(dec)
}

// DecodeObject 解析顶层 JSON 对象，数值按字面量保留。
//
// 顶层 null 返回 nil map（v1 口径：null 解码进 map 得到 nil，而不是错误）。
func DecodeObject(in []byte) (map[string]any, error) {
	value, err := DecodeAny(in)
	if err != nil {
		return nil, err
	}

	if value == nil {
		return nil, nil //nolint:nilnil // 顶层 null 表示「没有对象」，调用方按缺省处理
	}

	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("json: 顶层不是对象: %w", errNotObject)
	}

	return obj, nil
}

// errNotObject 顶层值不是 JSON 对象。
var errNotObject = errors.New("顶层值不是 JSON 对象")

// errInvalidValue 解码遇到非法 JSON 值。
var errInvalidValue = errors.New("非法 JSON 值")

// Number 判断 v 是否为 JSON 数字并返回其原始字面量。
func Number(v any) (jsontext.Value, bool) {
	raw, ok := v.(jsontext.Value)
	if !ok || raw.Kind() != '0' {
		return nil, false
	}

	return raw, true
}

// Int64 把 JSON 数值形态（原始字面量 / float64 / 整数）转 int64。
func Int64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case jsontext.Value:
		if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return i, true
		}

		f, err := strconv.ParseFloat(string(n), 64)
		if err != nil {
			return 0, false
		}

		return int64(f), true
	default:
		return 0, false
	}
}

// Float64 把 JSON 数值形态（原始字面量 / float64 / 整数）转 float64。
func Float64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case jsontext.Value:
		f, err := strconv.ParseFloat(string(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// withBase 把基础口径与调用点选项合成一份。
func withBase(base jsonv2.Options, opts []jsonv2.Options) []jsonv2.Options {
	joined := make([]jsonv2.Options, 0, len(opts)+1)
	joined = append(joined, base)

	return append(joined, opts...)
}

// decodeAnyValue 按 JSON 类型递归构造通用值；数字保留字面量。
func decodeAnyValue(dec *jsontext.Decoder) (any, error) {
	switch dec.PeekKind() {
	case '{':
		return decodeAnyObject(dec)
	case '[':
		return decodeAnyArray(dec)
	default:
		return decodeAnyScalar(dec)
	}
}

// decodeAnyScalar 读取一个标量值。
func decodeAnyScalar(dec *jsontext.Decoder) (any, error) {
	raw, err := dec.ReadValue()
	if err != nil {
		return nil, fmt.Errorf("读取 JSON 值: %w", err)
	}

	switch raw.Kind() {
	case '"':
		text, err := jsontext.AppendUnquote(nil, raw)
		if err != nil {
			return nil, fmt.Errorf("解转义 JSON 字符串: %w", err)
		}

		return string(text), nil
	case 't':
		return true, nil
	case 'f':
		return false, nil
	case 'n':
		return nil, nil //nolint:nilnil // JSON null 的 Go 值就是 nil
	case '0':
		// ReadValue 返回的是解码器内部缓冲的切片，下一次解码调用即失效；
		// 通用值需要活过整棵树的构建，必须克隆。
		return raw.Clone(), nil
	default:
		return nil, fmt.Errorf("%w: %v", errInvalidValue, raw.Kind())
	}
}

// decodeAnyObject 读取一个 JSON 对象（已确认下一个 token 是 '{'）。
func decodeAnyObject(dec *jsontext.Decoder) (any, error) {
	if _, err := dec.ReadToken(); err != nil {
		return nil, fmt.Errorf("读取 JSON 对象起始: %w", err)
	}

	obj := make(map[string]any)

	for {
		if dec.PeekKind() == '}' {
			if _, err := dec.ReadToken(); err != nil {
				return nil, fmt.Errorf("读取 JSON 对象结束: %w", err)
			}

			return obj, nil
		}

		token, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("读取 JSON 对象键: %w", err)
		}

		if token.Kind() != '"' {
			return nil, fmt.Errorf("%w: 对象键 %v", errInvalidValue, token.Kind())
		}

		// Token 只在下一次 Decoder 调用前有效，键名必须在递归解码值之前取出。
		name := token.String()

		value, err := decodeAnyValue(dec)
		if err != nil {
			return nil, err
		}

		obj[name] = value
	}
}

// decodeAnyArray 读取一个 JSON 数组（已确认下一个 token 是 '['）。
func decodeAnyArray(dec *jsontext.Decoder) (any, error) {
	if _, err := dec.ReadToken(); err != nil {
		return nil, fmt.Errorf("读取 JSON 数组起始: %w", err)
	}

	arr := make([]any, 0)

	for {
		if dec.PeekKind() == ']' {
			if _, err := dec.ReadToken(); err != nil {
				return nil, fmt.Errorf("读取 JSON 数组结束: %w", err)
			}

			return arr, nil
		}

		value, err := decodeAnyValue(dec)
		if err != nil {
			return nil, err
		}

		arr = append(arr, value)
	}
}

// ensureEOF 确认解码器已到输入末尾（只允许空白）。
func ensureEOF(dec *jsontext.Decoder) error {
	switch _, err := dec.ReadToken(); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return fmt.Errorf("检查 JSON 尾部: %w", err)
	default:
		return ErrTrailingData
	}
}
