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
