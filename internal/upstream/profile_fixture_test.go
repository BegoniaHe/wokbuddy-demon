package upstream

import (
	"path/filepath"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/clientprofile"
)

// testProfileSet 加载 testdata/profile 的假值快照（版本 9.9.x，仅供测试）。
func testProfileSet(t *testing.T) *clientprofile.Set {
	t.Helper()

	set, err := clientprofile.LoadDir(filepath.Join("testdata", "profile"))
	if err != nil {
		t.Fatalf("load test profile: %v", err)
	}

	return set
}

// newProfileClient 返回挂载测试 profile 的 Client（生产路径要求 profile 必需）。
func newProfileClient(t *testing.T) *Client {
	t.Helper()

	client := New()
	client.Profile = testProfileSet(t)

	return client
}
