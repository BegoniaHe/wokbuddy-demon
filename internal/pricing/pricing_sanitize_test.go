// pricing_sanitize_test.go 钉住价格条目净化：ParseFloat 接受 "NaN"/"Inf"/"-1"，
// 若放进快照，面板 /panel/api/pricing 的 json.Marshal 会因 NaN 直接 500，
// 且在下一次成功刷新前一直坏着。
package pricing

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRefreshDropsNonFiniteAndNegativePrices(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"ok-model","pricing":{"prompt":"0.001","completion":"0.002","input_cache_read":"0.0001"}},
			{"id":"nan-model","pricing":{"prompt":"NaN","completion":"0.002"}},
			{"id":"inf-model","pricing":{"prompt":"Inf","completion":"0.002"}},
			{"id":"negative-model","pricing":{"prompt":"-1","completion":"0.002"}},
			{"id":"bad-cache","pricing":{"prompt":"0.001","completion":"0.002","input_cache_read":"NaN"}}
		]}`))
	}))
	defer srv.Close()

	catalog := New(srv.URL, srv.Client())
	if err := catalog.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	prices := catalog.Snapshot().Prices

	if _, ok := prices["ok-model"]; !ok {
		t.Fatalf("合法条目被丢弃: %v", prices)
	}

	for _, id := range []string{"nan-model", "inf-model", "negative-model"} {
		if _, ok := prices[id]; ok {
			t.Fatalf("%s 的非法价格未被丢弃", id)
		}
	}

	// 非法 cache_read 降级为 0，条目本身仍可用（缺失缓存价不该丢掉整个模型）。
	if got := prices["bad-cache"]; got.CacheRead != 0 {
		t.Fatalf("cache_read=%v want 0", got.CacheRead)
	}
}
