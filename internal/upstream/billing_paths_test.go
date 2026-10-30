// billing_paths_test.go /billing/meter 族 5.9.3 路径与 Accept-Language 口径。
package upstream

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestBillingSummaryAndPackagesAcceptLanguage 只有 summary / packages 带
// Accept-Language，且是语言短码：CN → zh，global → en。
func TestBillingSummaryAndPackagesAcceptLanguage(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		realm string
		want  string
	}{
		{"cn", "cn", "zh"},
		{"global", "global", "en"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			assertBillingAcceptLanguage(t, testCase.realm, testCase.want)
		})
	}
}

// assertBillingAcceptLanguage 拉一次 summary + 一页 packages，断言两者都带
// 期望的语言短码。
func assertBillingAcceptLanguage(t *testing.T, realm, want string) {
	t.Helper()

	account := &auth.Auth{AccessToken: "at", UID: "u1"}
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(account, "global"); err != nil {
			t.Fatalf("BackfillRealmFor: %v", err)
		}
	}

	var seen []string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Accept-Language"))

		if strings.HasSuffix(r.URL.Path, "/billing/meter/get-user-resource-summary") {
			return jsonResp(200, `{"code":0,"data":{"packages":[{"remain":900,"total":1000}]}}`), nil
		}

		return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
	})
	if realm == "global" {
		client.GlobalEnabled = true
		client.BillingBaseGlobal = "https://billing.example"
	}

	if _, err := client.fetchResourceSummary(context.Background(), account); err != nil {
		t.Fatalf("summary: %v", err)
	}

	if _, err := client.fetchPackagePage(
		account, "/billing/meter/get-user-resource-paid-packages", map[string]any{}, 1,
	); err != nil {
		t.Fatalf("packages: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("requests=%v want 2（summary + packages）", seen)
	}

	for _, entry := range seen {
		if !strings.HasSuffix(entry, " "+want) {
			t.Errorf("Accept-Language=%q want %q (entry=%q)", entry, want, entry)
		}
	}
}

// TestBillingSummaryAndPackagesNoPrefixFirst summary / packages 与 get-user-resource
// 同口径：无前缀优先，404 回落 /v2（官方 resourcePrefix 为空串）。
func TestBillingSummaryAndPackagesNoPrefixFirst(t *testing.T) {
	t.Parallel()

	var summaryPaths, packagePaths []string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/meter/get-user-resource-summary"):
			summaryPaths = append(summaryPaths, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/v2/") {
				return jsonResp(200, `{"code":0,"data":{"packages":[{"remain":900,"total":1000}]}}`), nil
			}

			return jsonResp(404, `{"code":404}`), nil
		case strings.HasSuffix(r.URL.Path, "/billing/meter/get-user-resource-paid-packages"):
			packagePaths = append(packagePaths, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/v2/") {
				return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
			}

			return jsonResp(404, `{"code":404}`), nil
		default:
			return jsonResp(404, `{"code":404}`), nil
		}
	})

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	if _, err := client.fetchResourceSummary(context.Background(), account); err != nil {
		t.Fatalf("summary: %v", err)
	}

	if _, err := client.fetchPackagePage(
		account, "/billing/meter/get-user-resource-paid-packages", map[string]any{}, 1,
	); err != nil {
		t.Fatalf("packages: %v", err)
	}

	wantSummary := []string{
		"/billing/meter/get-user-resource-summary",
		"/v2/billing/meter/get-user-resource-summary",
	}
	if !slices.Equal(summaryPaths, wantSummary) {
		t.Errorf("summary paths=%v want %v", summaryPaths, wantSummary)
	}

	wantPackages := []string{
		"/billing/meter/get-user-resource-paid-packages",
		"/v2/billing/meter/get-user-resource-paid-packages",
	}
	if !slices.Equal(packagePaths, wantPackages) {
		t.Errorf("packages paths=%v want %v", packagePaths, wantPackages)
	}
}

// TestCheckinKeepsV2First 签到是路径例外：daily-checkin 仍 /v2 优先、404 回落
// 无前缀；checkin-activity-status 只有 /v2 单路径。
func TestCheckinKeepsV2First(t *testing.T) {
	t.Parallel()

	var checkinPaths, statusPaths []string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/billing/meter/daily-checkin"):
			checkinPaths = append(checkinPaths, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/v2/") {
				return jsonResp(404, `{"code":404}`), nil
			}

			return jsonResp(200, `{"code":0}`), nil
		case strings.HasSuffix(r.URL.Path, "/billing/meter/checkin-activity-status"):
			statusPaths = append(statusPaths, r.URL.Path)

			return jsonResp(200, `{"code":0,"data":{"today_checked_in":false}}`), nil
		default:
			return jsonResp(404, `{"code":404}`), nil
		}
	})

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	if err := client.DailyCheckin(context.Background(), account); err != nil {
		t.Fatalf("daily-checkin: %v", err)
	}

	if _, err := client.CheckinActivityStatus(account); err != nil {
		t.Fatalf("checkin-activity-status: %v", err)
	}

	wantCheckin := []string{
		"/v2/billing/meter/daily-checkin",
		"/billing/meter/daily-checkin",
	}
	if !slices.Equal(checkinPaths, wantCheckin) {
		t.Errorf("daily-checkin paths=%v want %v", checkinPaths, wantCheckin)
	}

	if !slices.Equal(statusPaths, []string{"/v2/billing/meter/checkin-activity-status"}) {
		t.Errorf("checkin-activity-status paths=%v want 单条 /v2 路径", statusPaths)
	}
}
