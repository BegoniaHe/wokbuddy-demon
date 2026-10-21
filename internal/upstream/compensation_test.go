package upstream

import (
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestCompensationPending 三与门：active 且未领且额度 > 0。
func TestCompensationPending(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status CompensationStatus
		want   bool
	}{
		{CompensationStatus{Active: true, CreditNum: 500}, true},
		{CompensationStatus{Active: false, CreditNum: 500}, false},
		{CompensationStatus{Active: true, Claimed: true, CreditNum: 500}, false},
		{CompensationStatus{Active: true, CreditNum: 0}, false},
	}

	for _, testCase := range cases {
		if got := testCase.status.Pending(); got != testCase.want {
			t.Errorf("%+v Pending()=%v want %v", testCase.status, got, testCase.want)
		}
	}
}

// TestGetCompensationStatusParsesFields 全字段解析（含 end_time 原样透传）。
func TestGetCompensationStatusParsesFields(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v2/billing/meter/compensation-status" && r.URL.Path != "/billing/meter/compensation-status" {
			return jsonResp(404, `{"code":404}`), nil
		}

		return jsonResp(200, `{"code":0,"msg":"OK","data":{"active":true,"claimed":false,"credit_num":500,"validity_period":1,"end_time":"2026-10-22 23:59:59"}}`), nil
	})

	status, err := client.GetCompensationStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if !status.Active || status.Claimed || status.CreditNum != 500 || status.ValidityPeriod != 1 {
		t.Errorf("status=%+v", status)
	}

	if status.EndTime != "2026-10-22 23:59:59" {
		t.Errorf("end_time=%q 应原样透传", status.EndTime)
	}

	if !status.Pending() {
		t.Error("该状态应判定为待领")
	}
}

// TestGetCompensationStatusInactiveIsNotError active=false 是正常 200，不是错误。
func TestGetCompensationStatusInactiveIsNotError(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"active":false}}`), nil
	})

	status, err := client.GetCompensationStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("无活动不应返回错误: %v", err)
	}

	if status.Pending() {
		t.Error("无活动不应判定为待领")
	}
}

// TestGetCompensationStatusServerError 5xx 必须返回错误（失败要显形）。
func TestGetCompensationStatusServerError(t *testing.T) {
	t.Parallel()

	client := testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(http.StatusInternalServerError, `{"code":500,"msg":"boom"}`), nil
	})

	if _, err := client.GetCompensationStatus(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("5xx 应返回错误")
	}
}
