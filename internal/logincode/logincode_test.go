package logincode

import (
	"errors"
	"strings"
	"testing"
)

func TestCodeClassification(t *testing.T) {
	t.Parallel()

	pending := []int{RetryFetchToken, RetryFetchAccount}
	terminal := []int{LicenseSeatLimit, LicenseExpired, TrialExpired}

	for _, code := range pending {
		if !IsPending(code) {
			t.Errorf("code=%d want pending", code)
		}

		if IsTerminal(code) {
			t.Errorf("code=%d 不应判终态", code)
		}
	}

	for _, code := range terminal {
		if !IsTerminal(code) {
			t.Errorf("code=%d want terminal", code)
		}

		if IsPending(code) {
			t.Errorf("code=%d 不应判 pending", code)
		}
	}

	if IsPending(0) || IsTerminal(0) {
		t.Error("code=0 既非 pending 也非终态")
	}
}

func TestErrorCarriesUpstreamMsg(t *testing.T) {
	t.Parallel()

	err := &Error{Code: LicenseSeatLimit, Msg: "席位已满", Status: 400}
	if !strings.Contains(err.Error(), "席位已满") {
		t.Errorf("error=%q 未透出上游 msg", err.Error())
	}

	var target *Error
	if !errors.As(error(err), &target) || target.Code != LicenseSeatLimit {
		t.Errorf("errors.As 未取回业务码: %v", err)
	}
}
