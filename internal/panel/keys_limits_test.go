package panel

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/apikey"
)

func TestKeysCreateWithLimits(t *testing.T) {
	t.Parallel()

	pnl := panelWithKeys(apikey.New(""))

	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	body := `{"name":"limited","limits":{"quota_tokens":100,"ip_allowlist":["192.0.2.1"],"expires_at":"` + expires + `"}}`

	code, resp := pnl.doKeys(http.MethodPost, "/panel/api/keys", body)
	if code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", code, resp)
	}

	var created keysResp
	if err := json.Unmarshal([]byte(resp), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if created.Record.Limits.QuotaTokens != 100 || len(created.Record.Limits.IPAllowlist) != 1 ||
		created.Record.Limits.ExpiresAt == nil {
		t.Fatalf("limits 未回显: %+v", created.Record.Limits)
	}

	if created.Record.Limits.ExpiresAt.Location() != time.UTC {
		t.Fatalf("expires_at 不是 UTC: %v", created.Record.Limits.ExpiresAt)
	}
}

func TestKeysInvalidLimitsRejected(t *testing.T) {
	t.Parallel()

	pnl := panelWithKeys(apikey.New(""))

	for _, body := range []string{
		`{"name":"bad","limits":{"quota_tokens":-1}}`,
		`{"name":"bad","limits":{"ip_allowlist":["not-an-ip"]}}`,
		`{"name":"bad","limits":{"rpm":-1}}`,
	} {
		code, resp := pnl.doKeys(http.MethodPost, "/panel/api/keys", body)
		if code != http.StatusBadRequest {
			t.Fatalf("body=%s code=%d resp=%s", body, code, resp)
		}
	}

	if got := listKeys(t, pnl); len(got) != 0 {
		t.Fatalf("非法限制不应落盘: %+v", got)
	}
}

func TestKeysUpdateLimitsValidation(t *testing.T) {
	t.Parallel()

	pnl := panelWithKeys(apikey.New(""))
	_, rec := createKey(t, pnl, "k")

	code, resp := pnl.doKeys(http.MethodPost, "/panel/api/keys/"+rec.ID+"/limits",
		`{"limits":{"ip_allowlist":["999.1.1.1"]}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 IP code=%d resp=%s", code, resp)
	}

	got := listKeys(t, pnl)
	if len(got) != 1 || len(got[0].Limits.IPAllowlist) != 0 {
		t.Fatalf("非法值写入记录: %+v", got)
	}

	code, resp = pnl.doKeys(http.MethodPost, "/panel/api/keys/"+rec.ID+"/limits",
		`{"limits":{"quota_tokens":50,"ip_allowlist":["2001:db8::/32"]}}`)
	if code != http.StatusOK {
		t.Fatalf("合法限制 code=%d resp=%s", code, resp)
	}

	got = listKeys(t, pnl)
	if got[0].Limits.QuotaTokens != 50 || len(got[0].Limits.IPAllowlist) != 1 ||
		got[0].Limits.IPAllowlist[0] != "2001:db8::/32" {
		t.Fatalf("限制未保存: %+v", got[0].Limits)
	}
}

// TestKeysCreateLogHasNoPrefix 创建日志不得出现 key 明文或展示前缀。
//
//nolint:paralleltest // 测试临时替换全局 log 输出，不能并行。
func TestKeysCreateLogHasNoPrefix(t *testing.T) {
	var buf bytes.Buffer

	old := log.Writer()

	log.SetOutput(&buf)
	defer log.SetOutput(old)

	pnl := panelWithKeys(apikey.New(""))

	raw, rec := createKey(t, pnl, "logged")
	logs := buf.String()

	if strings.Contains(logs, raw) || strings.Contains(logs, rec.Prefix) {
		t.Fatalf("日志泄露 key/prefix: %s", logs)
	}

	if !strings.Contains(logs, rec.ID) {
		t.Fatalf("日志应保留 key id 便于审计: %s", logs)
	}
}
