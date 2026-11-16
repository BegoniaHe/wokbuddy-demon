// overview_credits_test.go 钉住面板 overview 的积分汇总（issue #571）：
// 统计条用服务端 available/quota，不再自己遍历 accounts 求和，禁用号的冻结额度
// 不进可用合计但仍随 accounts[].credits_raw 可见。
package panel

import (
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
	"github.com/BegoniaHe/wokbuddy-demon/internal/pool"
)

func TestOverviewCreditsSummary(t *testing.T) {
	t.Parallel()

	testPool := pool.New("")
	t.Cleanup(testPool.Close)
	testPool.Add(&auth.Auth{UID: "live", AccessToken: "at", ExpiresAt: 9999999999})
	testPool.Add(&auth.Auth{UID: "dead", AccessToken: "at", ExpiresAt: 9999999999})
	testPool.SetCredits("live", 10, 20)
	testPool.SetCredits("dead", 30, 40)
	testPool.Disable("dead", "12153 session dead")

	panel := New(&Config{Version: "test", APIKey: "k", Pool: testPool})

	credits, ok := getOverview(t, panel)["credits"].(map[string]any)
	if !ok {
		t.Fatal("overview 缺 credits 汇总")
	}

	if credits["available"] != float64(10) || credits["snapshot"] != float64(40) || credits["quota"] != float64(20) {
		t.Errorf("credits=%+v want available=10 snapshot=40 quota=20", credits)
	}
}
