// restart_fields_test.go 保证 restartFieldPaths 与 Config 的 JSON 结构不脱节。
//
// 风险：路径写错（拼错 / 字段改名后没跟）时 restartRequiredFields 永远比较
// nil==nil，面板会静默宣称「已立即生效」，而用户实际必须重启。这个回归全靠
// 启动期不报错的字符串表，因此按 Config 的类型（而非默认值）枚举 JSON 路径做
// 存在性校验——默认值为 nil 的切片字段（如 dns_servers）在序列化结果里同样
// 是 nil，不能用来判定路径缺失。
package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestRestartFieldPathsResolveToConfigFields(t *testing.T) {
	t.Parallel()

	fields := configJSONPaths(reflect.TypeFor[Config]())
	seen := make(map[string]bool, len(restartFieldPaths))

	for _, path := range restartFieldPaths {
		if seen[path] {
			t.Errorf("restartFieldPaths 重复项: %q", path)
		}

		seen[path] = true

		if !fields[path] {
			t.Errorf("restartFieldPaths 中的 %q 在 Config 里不存在（字段改名或路径拼错？）", path)
		}
	}
}

// configJSONPaths 递归枚举结构体类型的 JSON 叶子路径（匿名结构体按嵌套处理，
// 与 configDoc 用 encoding/json 序列化后 digJSONPath 的取值形状一致）。
func configJSONPaths(typ reflect.Type) map[string]bool {
	out := make(map[string]bool)

	var walk func(prefix string, typ reflect.Type)

	walk = func(prefix string, typ reflect.Type) {
		for field := range typ.Fields() {
			if !field.IsExported() {
				continue
			}

			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}

			path := name
			if prefix != "" {
				path = prefix + "." + name
			}

			ft := field.Type
			if ft.Kind() == reflect.Struct {
				walk(path, ft)

				continue
			}

			out[path] = true
		}
	}

	walk("", typ)

	return out
}
