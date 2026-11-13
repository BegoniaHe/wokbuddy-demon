// roundcap_test.go 钉住「单轮补足次数上限」：need 来自上游 JSON（判据差额），
// 每条上报前还要按真人节奏 sleep ≈45-55s。上游一旦下发 target=1000，该账号会被
// per-account 锁占住十几个小时、期间其它面板动作全部跳过。
package panel

import "testing"

func TestClampRoundReports(t *testing.T) {
	t.Parallel()

	cases := []struct {
		need int64
		want int64
	}{
		{need: -5, want: -5}, // 负差额不放大，交由调用方判定「无需补足」
		{need: 0, want: 0},
		{need: 1, want: 1},
		{need: maxRoundReportsPerRun, want: maxRoundReportsPerRun},
		{need: maxRoundReportsPerRun + 1, want: maxRoundReportsPerRun},
		{need: 100000, want: maxRoundReportsPerRun},
	}

	for _, tc := range cases {
		if got := clampRoundReports(tc.need); got != tc.want {
			t.Fatalf("clampRoundReports(%d)=%d want %d", tc.need, got, tc.want)
		}
	}
}
