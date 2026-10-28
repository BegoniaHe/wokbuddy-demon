// enterprise_model_path_test.go 企业模型目录 /console 路径按账号 enterpriseId 拼接。
package upstream

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

const enterpriseModelsBody = `{"code":0,"data":{"models":[{"id":"m1","maxOutputTokens":8192}],` +
	`"agents":[{"name":"cli","models":["m1"]}]}}`

// TestFetchEnterpriseModelsUsesAccountEnterpriseID /console 目录按账号 enterpriseId
// 拼接，个人号回落 personal；企业 ID 做路径转义（#341）。
func TestFetchEnterpriseModelsUsesAccountEnterpriseID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		eid      string
		wantPath string
	}{
		{"personal", "", "/console/enterprises/personal/models"},
		{"enterprise", "ent-123", "/console/enterprises/ent-123/models"},
		{"escaped", "a/b", "/console/enterprises/a%2Fb/models"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string

			client := testClient(func(r *http.Request) (*http.Response, error) {
				gotPath = r.URL.EscapedPath()

				return jsonResp(200, enterpriseModelsBody), nil
			})

			infos, _, err := client.fetchEnterpriseModels(&auth.Auth{
				AccessToken: "at", UID: "u1", EnterpriseID: testCase.eid,
			})
			if err != nil {
				t.Fatalf("fetchEnterpriseModels: %v", err)
			}

			if gotPath != testCase.wantPath {
				t.Errorf("path=%q want %q", gotPath, testCase.wantPath)
			}

			if len(infos) != 1 || infos[0].ID != "m1" {
				t.Errorf("infos=%+v want 单条 m1", infos)
			}
		})
	}
}

// TestFetchEnterpriseModelsEnterpriseFallsBackToV2 /console 企业路径失败时回落
// /v2/enterprises/personal/models（官方 daemon 侧固定 personal）。
func TestFetchEnterpriseModelsEnterpriseFallsBackToV2(t *testing.T) {
	t.Parallel()

	var paths []string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)

		if strings.HasPrefix(r.URL.Path, "/console/") {
			return jsonResp(500, `{"code":500}`), nil
		}

		return jsonResp(200, enterpriseModelsBody), nil
	})

	infos, _, err := client.fetchEnterpriseModels(&auth.Auth{
		AccessToken: "at", UID: "u1", EnterpriseID: "ent-123",
	})
	if err != nil {
		t.Fatalf("fetchEnterpriseModels: %v", err)
	}

	if len(infos) != 1 || infos[0].ID != "m1" {
		t.Errorf("infos=%+v want 单条 m1", infos)
	}

	want := []string{
		"/console/enterprises/ent-123/models",
		"/v2/enterprises/personal/models",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("paths=%v want %v", paths, want)
	}
}
