package scheduler

import "testing"

// TestClampLotteryDraws 抽奖次数来自上游 JSON：必须钳到 [0, 上限]，
// 否则一次异常响应就能让 drawAllLottery 对同一账号连打上万次 POST。
func TestClampLotteryDraws(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		chances   int
		want      int
		truncated bool
	}{
		{name: "负数归零", chances: -5, want: 0, truncated: false},
		{name: "零", chances: 0, want: 0, truncated: false},
		{name: "正常值", chances: 3, want: 3, truncated: false},
		{name: "恰好上限", chances: maxLotteryDrawsPerRound, want: maxLotteryDrawsPerRound, truncated: false},
		{name: "超上限被钳", chances: maxLotteryDrawsPerRound + 1, want: maxLotteryDrawsPerRound, truncated: true},
		{name: "异常大值", chances: 1 << 30, want: maxLotteryDrawsPerRound, truncated: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, truncated := clampLotteryDraws(testCase.chances)
			if got != testCase.want || truncated != testCase.truncated {
				t.Fatalf("clampLotteryDraws(%d) = (%d,%v) want (%d,%v)",
					testCase.chances, got, truncated, testCase.want, testCase.truncated)
			}
		})
	}
}
