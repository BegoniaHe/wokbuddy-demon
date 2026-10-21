package livecfg

import (
	"reflect"
	"testing"
)

// TestNormalizeHiddenModels models.hidden 归一：裸名展开两域、带前缀只藏该域、
// 空白/重复丢弃、大小写敏感（与 splitRealm 同口径）。
func TestNormalizeHiddenModels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []string
		want map[string]struct{}
	}{
		{
			name: "裸名两域都隐藏",
			in:   []string{"glm-5.2"},
			want: map[string]struct{}{"cn:glm-5.2": {}, "global:glm-5.2": {}},
		},
		{
			name: "cn 前缀只藏 cn",
			in:   []string{"cn:kimi-k3"},
			want: map[string]struct{}{"cn:kimi-k3": {}},
		},
		{
			name: "global 前缀只藏 global",
			in:   []string{"global:kimi-k3"},
			want: map[string]struct{}{"global:kimi-k3": {}},
		},
		{
			name: "空白与重复丢弃",
			in:   []string{"  glm-5.2  ", "", "   ", "glm-5.2"},
			want: map[string]struct{}{"cn:glm-5.2": {}, "global:glm-5.2": {}},
		},
		{
			name: "大小写敏感：CN 前缀视为裸模型名",
			in:   []string{"CN:kimi-k3"},
			want: map[string]struct{}{"cn:CN:kimi-k3": {}, "global:CN:kimi-k3": {}},
		},
		{
			name: "前缀后为空则忽略",
			in:   []string{"cn:", "  "},
			want: nil,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := NormalizeHiddenModels(testCase.in)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("NormalizeHiddenModels(%v)=%v want %v", testCase.in, got, testCase.want)
			}
		})
	}
}

// TestSnapshotHiddenModels 快照整体替换语义：Store 后 Load 看到新集合。
func TestSnapshotHiddenModels(t *testing.T) {
	t.Parallel()

	holder := New(Snapshot{HiddenModels: NormalizeHiddenModels([]string{"cn:mm"})})
	if _, ok := holder.Load().HiddenModels["cn:mm"]; !ok {
		t.Fatal("初始快照应包含 cn:mm")
	}

	holder.Store(Snapshot{HiddenModels: NormalizeHiddenModels([]string{"global:mm"})})

	loaded := holder.Load()
	if _, ok := loaded.HiddenModels["cn:mm"]; ok {
		t.Error("Store 后旧条目应消失（整体替换）")
	}

	if _, ok := loaded.HiddenModels["global:mm"]; !ok {
		t.Error("Store 后应包含 global:mm")
	}
}
