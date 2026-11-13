package pricing

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefreshAndEstimate(t *testing.T) {
	t.Parallel()

	body := `{"data":[{"id":"openai/gpt-5","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000005"}}]}`

	catalog := New("", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, ok := catalog.Estimate("openai/gpt-5", 100, 50, 60, 40, 1)
	if !ok {
		t.Fatal("model should be priced")
	}

	want := 40*0.000001 + 60*0.0000005 + 50*0.000002
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("estimate=%v want %v", got, want)
	}

	if _, ok := catalog.Estimate("unknown", 1, 1, 0, 0, 1); ok {
		t.Fatal("unknown model must be unpriced")
	}
}

func TestRefreshFailureKeepsSnapshot(t *testing.T) {
	t.Parallel()

	var fail bool

	body := `{"data":[{"id":"m","pricing":{"prompt":"1","completion":"2"}}]}`

	catalog := New("", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if fail {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader(""))}, nil
		}

		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	fail = true

	if err := catalog.Refresh(context.Background()); err == nil {
		t.Fatal("want failure")
	}

	snap := catalog.Snapshot()
	if len(snap.Prices) != 1 || snap.LastError == "" {
		t.Fatalf("snapshot=%+v", snap)
	}
}

// TestEstimateCountsUncachedPrompt 上游只回报部分缓存计数时，差额按普通输入价计入；
// 整段丢弃 prompt 会少算钱。
func TestEstimateCountsUncachedPrompt(t *testing.T) {
	t.Parallel()

	body := `{"data":[{"id":"openai/gpt-5","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000005"}}]}`

	catalog := New("", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// prompt=100，但只回报 hit=20 / miss=30：剩余 50 必须按普通输入价计。
	got, ok := catalog.Estimate("openai/gpt-5", 100, 0, 20, 30, 1)
	if !ok {
		t.Fatal("model should be priced")
	}

	want := 80*0.000001 + 20*0.0000005
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("estimate=%v want %v", got, want)
	}
}

// TestRefreshEmptyCatalogKeepsSnapshot 上游返回 200 但目录为空（或全部条目非法）时
// 必须保留上一份成功快照：用空表覆盖会让面板估算金额瞬间归零且毫无提示。
func TestRefreshEmptyCatalogKeepsSnapshot(t *testing.T) {
	t.Parallel()

	catalog := New("", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
		}, nil
	})})

	// 先塞一份成功快照（直接写内部状态，避免依赖两次取数顺序）。
	catalog.mu.Lock()
	catalog.prices = map[string]Price{"m": {Prompt: 1, Completion: 2}}
	catalog.mu.Unlock()

	if err := catalog.Refresh(context.Background()); err == nil {
		t.Fatal("空目录应返回错误")
	}

	catalog.mu.Lock()
	defer catalog.mu.Unlock()

	if len(catalog.prices) != 1 {
		t.Fatalf("prices=%v want 保留旧快照", catalog.prices)
	}
}
