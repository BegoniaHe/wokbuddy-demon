package server

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/upstream"
)

// TestModelsFetchSingleflight 缓存过期瞬间的高并发只触发一轮上游探测：所有等待者
// 复用首次结果，既不惊群，也不会让慢结果覆盖快结果（此前检查与提交分两把锁）。
func TestModelsFetchSingleflight(t *testing.T) {
	t.Parallel()

	const body = `{"code":0,"data":{"models":[{"id":"glm-5.2"}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`

	var probes atomic.Int32

	entered := make(chan struct{})
	release := make(chan struct{})

	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			// 首路探测挂住，制造「缓存已判定过期、结果尚未提交」的并发窗口。
			if probes.Add(1) == 1 {
				close(entered)
				<-release
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}

	state := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	handler := NewHandler(&Config{Pool: state, Upstream: client, modelsCache: &modelsCache{}})

	const concurrent = 16

	results := make(chan int, concurrent)
	start := make(chan struct{})

	for range concurrent {
		go func() {
			<-start

			results <- len(handler.fetchDynamicModels())
		}()
	}

	close(start)
	<-entered
	close(release)

	for range concurrent {
		if got := <-results; got == 0 {
			t.Fatal("单飞等待者必须复用首次拉取结果")
		}
	}

	// 一轮探测是少量固定次数的 RoundTrip；无单飞时每个并发请求各打一轮，
	// 16 路会成倍放大。上界留足余量，只要证明没有 N 倍穿透即可。
	if got := probes.Load(); got > 8 {
		t.Fatalf("并发 %d 请求触发 %d 次上游探测，疑似惊群（单飞应只打一轮）", concurrent, got)
	}
}
