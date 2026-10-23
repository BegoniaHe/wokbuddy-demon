package upstream

import (
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestHeatmapMissedDays 只返回区间内 score==0 的日期，升序（issue #250）。
func TestHeatmapMissedDays(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/activity/growth/heatmap" && r.URL.Path != "/activity/growth/heatmap" {
			t.Errorf("path=%s", r.URL.Path)
		}

		return jsonResp(200, `{"code":0,"data":{"cells":[
			{"date":"2026-10-01","score":1},
			{"date":"2026-10-03","score":0},
			{"date":"2026-10-02T00:00:00Z","score":0},
			{"date":"2026-09-30","score":0},
			{"date":"2026-10-04","score":0}
		]}}`), nil
	})

	days, err := client.HeatmapMissedDays(&auth.Auth{UID: "u1", AccessToken: "at"}, "2026-10-01", "2026-10-03")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"2026-10-02", "2026-10-03"}
	if len(days) != len(want) {
		t.Fatalf("days=%v want %v", days, want)
	}

	for i := range want {
		if days[i] != want[i] {
			t.Fatalf("days=%v want %v", days, want)
		}
	}
}
