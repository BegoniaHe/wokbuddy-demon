package scheduler

import "testing"

// TestClampNightChats 待补对话数来自上游任务列表：必须钳到 [0, 上限]。
//
// 每次夜间对话是一条真实 chat + 4s 间隔，且全程占着该账号的在途租约：一次异常
// 或被篡改的响应（target 极大）就能把租约占住数天并对同一账号连打上万次上游。
func TestClampNightChats(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		need      int64
		want      int
		truncated bool
	}{
		{name: "负数归零", need: -3, want: 0, truncated: false},
		{name: "零", need: 0, want: 0, truncated: false},
		{name: "正常值", need: 3, want: 3, truncated: false},
		{name: "恰好上限", need: maxNightChatsPerRound, want: maxNightChatsPerRound, truncated: false},
		{name: "超上限被钳", need: maxNightChatsPerRound + 1, want: maxNightChatsPerRound, truncated: true},
		{name: "异常大值", need: 1 << 40, want: maxNightChatsPerRound, truncated: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, truncated := clampNightChats(testCase.need)
			if got != testCase.want || truncated != testCase.truncated {
				t.Fatalf("clampNightChats(%d) = (%d,%v) want (%d,%v)",
					testCase.need, got, truncated, testCase.want, testCase.truncated)
			}
		})
	}
}
