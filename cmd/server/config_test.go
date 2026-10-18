package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Listen != ":7863" {
		t.Errorf("listen=%s", config.Listen)
	}

	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if config.SoftRateDur.Seconds() != 600 {
		t.Errorf("soft=%v want 600s", config.SoftRateDur)
	}
}

func TestPanelPackageDetailLimit(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if config.Panel.PackageDetailLimit != 5 {
		t.Fatalf("default package_detail_limit=%d want 5", config.Panel.PackageDetailLimit)
	}

	configured, err := ParseConfig([]byte(`{"panel":{"package_detail_limit":8}}`))
	if err != nil {
		t.Fatalf("parse configured limit: %v", err)
	}

	if configured.Panel.PackageDetailLimit != 8 {
		t.Fatalf("configured package_detail_limit=%d want 8", configured.Panel.PackageDetailLimit)
	}

	fallback, err := ParseConfig([]byte(`{"panel":{"package_detail_limit":0}}`))
	if err != nil {
		t.Fatalf("parse fallback limit: %v", err)
	}

	if fallback.Panel.PackageDetailLimit != 5 {
		t.Fatalf("fallback package_detail_limit=%d want 5", fallback.Panel.PackageDetailLimit)
	}
}

func TestLoggingDefaults(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	checkLoggingArchive(t, "default",
		config.Logging.RequestArchiveEnabled, config.Logging.RequestRetentionDays, config.Logging.RequestArchiveMaxMB,
		true, 7, 100)
	// 来源记录（IP/UA）缺省开启：键缺席时必须保持 true，只有显式 false 才关闭。
	checkBool(t, "request_client_info default", config.Logging.RequestClientInfo, true)

	configured := mustParseConfig(t, `{"logging":{"request_archive_enabled":false,"request_retention_days":30,"request_archive_max_mb":500}}`)
	checkLoggingArchive(t, "configured",
		configured.Logging.RequestArchiveEnabled, configured.Logging.RequestRetentionDays, configured.Logging.RequestArchiveMaxMB,
		false, 30, 500)

	off := mustParseConfig(t, `{"logging":{"request_client_info":false}}`)
	checkBool(t, "explicit false request_client_info", off.Logging.RequestClientInfo, false)

	fallback := mustParseConfig(t, `{"logging":{"request_retention_days":0,"request_archive_max_mb":0}}`)
	checkLoggingArchive(t, "fallback",
		fallback.Logging.RequestArchiveEnabled, fallback.Logging.RequestRetentionDays, fallback.Logging.RequestArchiveMaxMB,
		true, 7, 100)
}

func TestLoadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Listen != ":9999" || config.APIKey != "k" {
		t.Errorf("c=%+v", config)
	}
}

//nolint:paralleltest // 该用例改写进程级状态，必须串行执行
func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")

	config, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if config.Listen != ":7777" || config.APIKey != "envkey" {
		t.Errorf("c=%+v", config)
	}
}

func TestBadDuration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	t.Parallel()
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}

	if config.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", config.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	checkInt(t, "max_in_flight", config.Pool.MaxInFlight, 3)
	checkInt(t, "breaker_threshold", config.Pool.BreakerThreshold, 3)
	checkDur(t, "breaker_cooldown", config.BreakerCooldownDur, 30*time.Minute)
	checkDur(t, "breaker_cooldown_max", config.BreakerCooldownMaxD, 6*time.Hour)
	checkFloat(t, "idle_weight_per_hour", config.Pool.IdleWeightPerHour, 0.5)
	checkFloat(t, "idle_weight_max", config.Pool.IdleWeightMax, 5.0)
	checkBool(t, "prefer_expiring", config.Pool.PreferExpiring, true)
	checkDur(t, "expiring_soon", config.ExpiringSoonDur, 7*24*time.Hour)
	checkDur(t, "soft_rate_max", config.SoftRateMaxDur, 2*time.Hour)
	checkBool(t, "session_sticky.enabled", config.SessionSticky.Enabled, true)
	checkDur(t, "session_sticky.ttl", config.SessionTTL, 30*time.Minute)
	checkDur(t, "session_sticky.gc_interval", config.SessionGCInterval, 5*time.Minute)
	checkStr(t, "upstash.url", config.Upstash.URL, "")
	checkStr(t, "upstash.token", config.Upstash.Token, "")
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	t.Parallel()
	config := loadTempConfig(t, `{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0,
			"prefer_expiring":false,
			"expiring_soon":"72h"
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`)
	checkStr(t, "upstash.url", config.Upstash.URL, "https://foo.upstash.io")
	checkStr(t, "upstash.token", config.Upstash.Token, "tok")
	checkInt(t, "max_in_flight", config.Pool.MaxInFlight, 5)
	checkInt(t, "breaker_threshold", config.Pool.BreakerThreshold, 4)
	checkDur(t, "breaker_cooldown", config.BreakerCooldownDur, 10*time.Minute)
	checkDur(t, "breaker_cooldown_max", config.BreakerCooldownMaxD, 2*time.Hour)
	checkFloat(t, "idle_weight_per_hour", config.Pool.IdleWeightPerHour, 0.7)
	checkFloat(t, "idle_weight_max", config.Pool.IdleWeightMax, 8.0)
	checkBool(t, "prefer_expiring", config.Pool.PreferExpiring, false)
	checkDur(t, "expiring_soon", config.ExpiringSoonDur, 72*time.Hour)
	checkBool(t, "session_sticky.enabled", config.SessionSticky.Enabled, false)
	checkDur(t, "session_sticky.ttl", config.SessionTTL, 1*time.Hour)
	checkDur(t, "session_sticky.gc_interval", config.SessionGCInterval, 2*time.Minute)
}

func TestSoftRateMaxParsedFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"5m","soft_rate_max":"45m"}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.SoftRateDur.Minutes() != 5 {
		t.Errorf("soft_rate=%v want 5m", config.SoftRateDur)
	}

	if config.SoftRateMaxDur.Minutes() != 45 {
		t.Errorf("soft_rate_max=%v want 45m", config.SoftRateMaxDur)
	}
}

func TestLegacyConfigKeepsPreferExpiringEnabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"idle_weight_max":3}}`), 0o600)

	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if !c.Pool.PreferExpiring {
		t.Fatal("missing prefer_expiring must preserve default true")
	}
}

func TestNegativeExpiringSoonClampsToDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"expiring_soon":"-1h"}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.ExpiringSoonDur != 0 || config.Pool.ExpiringSoon != "0" {
		t.Fatalf("negative window=%v/%q want 0/0", config.ExpiringSoonDur, config.Pool.ExpiringSoon)
	}
}

func TestSoftRateMaxEmptyFallsBackToDefault(t *testing.T) {
	t.Parallel()
	// 键缺席 → Default() 的 2h 保留（空串无法 ParseDuration）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"90s"}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h fallback", config.SoftRateMaxDur)
	}
}

func TestBadSoftRateMax(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate_max":"oops"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad soft_rate_max")
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	t.Parallel()
	// 默认：header 回落 timeout，idle 回落 300。
	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if config.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", config.Upstream.TimeoutSeconds)
	}

	if config.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", config.Upstream.HeaderTimeoutSeconds)
	}

	if config.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", config.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	t.Parallel()
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", config.Upstream.HeaderTimeoutSeconds)
	}

	if config.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", config.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", config.Upstream.HeaderTimeoutSeconds)
	}

	if config.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", config.Upstream.IdleTimeoutSeconds)
	}
}

//nolint:paralleltest // 该用例改写进程级状态，必须串行执行
func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")

	config, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if config.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", config.Upstream.HeaderTimeoutSeconds)
	}

	if config.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", config.Upstream.IdleTimeoutSeconds)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}

	if len(config.Schedule.CheckinHours) != 1 || config.Schedule.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", config.Schedule.CheckinHours)
	}
}

// TestScheduleEnabledByDefault 四个任务的 enabled 开关默认均为 true：
// 老 config 不写这些键，行为必须与从前完全一致。
func TestScheduleEnabledByDefault(t *testing.T) {
	t.Parallel()

	config := Default()
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	checkBool(t, "checkin_enabled", config.Schedule.CheckinEnabled, true)
	checkBool(t, "keepalive_enabled", config.Schedule.KeepaliveEnabled, true)
	checkBool(t, "travel_enabled", config.Schedule.TravelEnabled, true)
	checkBool(t, "activity_enabled", config.Schedule.ActivityEnabled, true)
	checkHours(t, "travel_hours", config.Schedule.TravelHours, []int{9, 21})
	checkHours(t, "activity_hours", config.Schedule.ActivityHours, []int{10})
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写签到/保活小时数组，无新键）加载后仍是启用态，
// 新开关缺省 true、新 hours 回落默认——对老配置零影响。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	t.Parallel()
	config := loadTempConfig(t, `{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`)
	checkBool(t, "checkin_enabled", config.Schedule.CheckinEnabled, true)
	checkBool(t, "keepalive_enabled", config.Schedule.KeepaliveEnabled, true)
	checkBool(t, "travel_enabled", config.Schedule.TravelEnabled, true)
	checkBool(t, "activity_enabled", config.Schedule.ActivityEnabled, true)
	checkHours(t, "checkin_hours", config.Schedule.CheckinHours, []int{9, 21})
	// 新 hours 缺省 → 回落默认（非空）。
	checkHours(t, "travel_hours", config.Schedule.TravelHours, []int{9, 21})
	checkHours(t, "activity_hours", config.Schedule.ActivityHours, []int{10})
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Schedule.CheckinEnabled || config.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", config.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(config.Schedule.CheckinHours) != 2 || config.Schedule.CheckinHours[0] != 9 || config.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", config.Schedule.CheckinHours)
	}

	if len(config.Schedule.KeepaliveHours) != 1 || config.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", config.Schedule.KeepaliveHours)
	}
}

// TestScheduleTravelActivityExplicitDisable 显式关闭旅行/活跃上报开关。
func TestScheduleTravelActivityExplicitDisable(t *testing.T) {
	t.Parallel()
	config := loadTempConfig(t, `{"schedule":{"travel_enabled":false,"activity_enabled":false}}`)
	checkBool(t, "travel_enabled", config.Schedule.TravelEnabled, false)
	checkBool(t, "activity_enabled", config.Schedule.ActivityEnabled, false)
	// 签到/保活开关缺省 true（互不干扰）。
	checkBool(t, "checkin_enabled", config.Schedule.CheckinEnabled, true)
	checkBool(t, "keepalive_enabled", config.Schedule.KeepaliveEnabled, true)
	// hours 仍回落默认。
	checkHours(t, "travel_hours", config.Schedule.TravelHours, []int{9, 21})
	checkHours(t, "activity_hours", config.Schedule.ActivityHours, []int{10})
}

// TestScheduleTravelActivityInvalidHoursRejected 旅行/活跃非法小时报错并指向正确开关。
func TestScheduleTravelActivityInvalidHoursRejected(t *testing.T) {
	t.Parallel()

	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"travel_hours":[25]}}`, "travel_enabled"},
		{`{"schedule":{"travel_hours":[-1]}}`, "travel_enabled"},
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"activity_hours":[-1]}}`, "activity_enabled"},
	}
	for _, testCase := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(testCase.body), 0o600)

		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", testCase.body)
		}

		if !strings.Contains(err.Error(), testCase.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", testCase.body, testCase.wantSwitch, err)
		}
	}
}

// TestScheduleTravelActivityExplicitHours 显式配置旅行/活跃小时。
func TestScheduleTravelActivityExplicitHours(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_hours":[9,21],"activity_hours":[11]}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if len(config.Schedule.TravelHours) != 2 || config.Schedule.TravelHours[0] != 9 || config.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9 21]", config.Schedule.TravelHours)
	}

	if len(config.Schedule.ActivityHours) != 1 || config.Schedule.ActivityHours[0] != 11 {
		t.Errorf("activity_hours=%v want [11]", config.Schedule.ActivityHours)
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}

	if len(config.Schedule.CheckinHours) != 2 || config.Schedule.CheckinHours[0] != 10 || config.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", config.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null,"travel_hours":null,"activity_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[],"travel_hours":[],"activity_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertEmptyHoursDefaults(t, loadTempConfig(t, body))
		})
	}
}

// assertEmptyHoursDefaults 断言未配置小时数组时的回落结果：四个小时数组回默认，
// 且四个开关保持缺省 true（空 hours 不蕴含禁用）。
func assertEmptyHoursDefaults(t *testing.T, config *Config) {
	t.Helper()
	checkHours(t, "checkin_hours", config.Schedule.CheckinHours, []int{9, 21})
	checkHours(t, "keepalive_hours", config.Schedule.KeepaliveHours, []int{22})
	checkHours(t, "travel_hours", config.Schedule.TravelHours, []int{9, 21})
	checkHours(t, "activity_hours", config.Schedule.ActivityHours, []int{10})
	checkBool(t, "checkin_enabled", config.Schedule.CheckinEnabled, true)
	checkBool(t, "keepalive_enabled", config.Schedule.KeepaliveEnabled, true)
	checkBool(t, "travel_enabled", config.Schedule.TravelEnabled, true)
	checkBool(t, "activity_enabled", config.Schedule.ActivityEnabled, true)
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	t.Parallel()

	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, testCase := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(testCase.body), 0o600)

		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", testCase.body)
		}

		if !strings.Contains(err.Error(), testCase.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", testCase.body, testCase.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

func TestWriteDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "sub", "config.json") // 顺带验证父目录自动创建

	key, err := WriteDefault(filePath)
	if err != nil {
		t.Fatal(err)
	}
	// key 形如 sk-<24字符随机串>，两次生成不重复
	if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
		t.Errorf("key=%q want sk-<random>", key)
	}

	key2, err := WriteDefault(filepath.Join(dir, "another.json"))
	if err != nil {
		t.Fatal(err)
	}

	if key2 == key {
		t.Errorf("two generated keys identical: %q", key)
	}
	// 落盘文件可被 Load 正常加载，推荐值齐备且 api_key 生效
	assertWrittenDefaults(t, filePath, key)
	// 已存在的文件不覆盖：二次写入同一路径必须报错
	if _, err := WriteDefault(filePath); err == nil {
		t.Error("WriteDefault must refuse to overwrite existing file")
	}
}

func TestReplacePlaceholderAPIKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "config.json")

	raw := `{"api_key":"test_key","listen":":9000","custom":{"keep":true}}`
	if err := os.WriteFile(filePath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	key, replaced, err := ReplacePlaceholderAPIKey(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if !replaced || key == placeholderAPIKey || !strings.HasPrefix(key, "sk-") {
		t.Fatalf("replaced=%v key=%q want random sk- key", replaced, key)
	}

	config, err := Load(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if config.APIKey != key || config.Listen != ":9000" {
		t.Fatalf("loaded api_key=%q listen=%q", config.APIKey, config.Listen)
	}

	assertConfigDocAPIKey(t, filePath, key)
}

func TestReplacePlaceholderAPIKeyIdempotent(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(filePath, []byte(`{"api_key":"test_key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := ReplacePlaceholderAPIKey(filePath); err != nil {
		t.Fatal(err)
	}

	if _, replaced, err := ReplacePlaceholderAPIKey(filePath); err != nil || replaced {
		t.Fatalf("second run replaced=%v err=%v", replaced, err)
	}
}

// assertConfigDocAPIKey 断言文件中 api_key 已写成 want，且未知顶层键仍保留。
func assertConfigDocAPIKey(t *testing.T, fp, want string) {
	t.Helper()

	written, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(written, &doc); err != nil {
		t.Fatal(err)
	}

	if _, ok := doc["custom"]; !ok {
		t.Error("unknown key custom dropped")
	}

	var got string
	if err := json.Unmarshal(doc["api_key"], &got); err != nil || got != want {
		t.Fatalf("file api_key=%q err=%v want %q", got, err, want)
	}
}

func TestReplacePlaceholderAPIKeyKeepsExplicitValues(t *testing.T) {
	t.Parallel()

	for _, explicit := range []string{"", "my-own-key"} {
		t.Run(explicit, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			filePath := filepath.Join(dir, "config.json")

			raw, err := json.Marshal(map[string]any{"api_key": explicit, "listen": ":9000"})
			if err != nil {
				t.Fatal(err)
			}

			if werr := os.WriteFile(filePath, raw, 0o600); werr != nil {
				t.Fatal(werr)
			}

			key, replaced, rerr := ReplacePlaceholderAPIKey(filePath)
			if rerr != nil {
				t.Fatal(rerr)
			}

			if replaced || key != "" {
				t.Fatalf("replaced=%v key=%q want untouched", replaced, key)
			}

			after, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatal(err)
			}

			if string(after) != string(raw) {
				t.Errorf("file changed for explicit value %q", explicit)
			}
		})
	}
}

func TestLoadConfigReplacesPlaceholder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	filePath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(filePath, []byte(`{"api_key":"test_key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := loadConfig(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if config.APIKey == placeholderAPIKey || config.APIKey == "" {
		t.Fatalf("api_key=%q want random", config.APIKey)
	}
	// 写回后的文件与运行时一致
	testCase, err := Load(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if testCase.APIKey != config.APIKey {
		t.Errorf("file api_key=%q runtime=%q", testCase.APIKey, config.APIKey)
	}
}

// assertWrittenDefaults 断言 WriteDefault 产出的文件可加载、api_key 生效且推荐值齐备。
func assertWrittenDefaults(t *testing.T, fp, key string) {
	t.Helper()

	config, err := Load(fp)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}

	checkStr(t, "api_key", config.APIKey, key)

	if config.Listen != ":7863" || config.AuthDir != "./auths" || config.StateFile != "./data/state.json" {
		t.Errorf("generated defaults off: %+v", config)
	}

	if len(config.Schedule.CheckinHours) == 0 || !config.Schedule.CheckinEnabled {
		t.Errorf("generated schedule off: %+v", config.Schedule)
	}
}

func TestBalanceRefreshDefaults(t *testing.T) {
	t.Parallel()
	// 缺省：启用 + 30 分钟
	config2 := Default()
	if err := config2.normalize(); err != nil {
		t.Fatal(err)
	}

	if !config2.Schedule.BalanceRefreshEnabled || config2.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("default balance refresh: enabled=%v interval=%v", config2.Schedule.BalanceRefreshEnabled, config2.BalanceRefreshInterval)
	}
	// 显式配置 10 分钟
	dir := t.TempDir()
	filePath := filepath.Join(dir, "c.json")
	os.WriteFile(filePath, []byte(`{"schedule":{"balance_refresh_minutes":10}}`), 0o600)

	testCase, err := Load(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if testCase.BalanceRefreshInterval != 10*time.Minute {
		t.Errorf("interval=%v want 10m", testCase.BalanceRefreshInterval)
	}
	// 显式关闭：interval 归零（不启动）
	os.WriteFile(filePath, []byte(`{"schedule":{"balance_refresh_enabled":false}}`), 0o600)

	testCase2, err := Load(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if testCase2.BalanceRefreshInterval != 0 {
		t.Errorf("disabled interval=%v want 0", testCase2.BalanceRefreshInterval)
	}
	// 启用但 minutes<=0 → 回落默认 30
	os.WriteFile(filePath, []byte(`{"schedule":{"balance_refresh_minutes":-5}}`), 0o600)

	config, err := Load(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if config.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("fallback interval=%v want 30m", config.BalanceRefreshInterval)
	}
}

// TestPromptDefaultPassthrough 默认 prompt.mode=passthrough（对齐上游：透传客户端
// 原始 system 是更保守的缺省）；custom 由用户显式选择，此时 PromptText 为内置默认（非空）。
func TestPromptDefaultPassthrough(t *testing.T) {
	t.Parallel()

	config, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if config.Prompt.Mode != "passthrough" {
		t.Errorf("prompt.mode=%q want passthrough", config.Prompt.Mode)
	}
	// passthrough 不加载提示词文本（透传客户端 system）；切 custom 时 normalize 会加载。
}

// TestPromptExplicitPassthrough passthrough 模式不加载文本（透传客户端原始 system）。
func TestPromptExplicitPassthrough(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"passthrough"}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", config.Prompt.Mode)
	}

	if config.PromptText != "" {
		t.Errorf("passthrough should not load PromptText, got len=%d", len(config.PromptText))
	}
}

// TestPromptInvalidMode 非法 mode 启动报错。
func TestPromptInvalidMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"bogus"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
}

// TestPromptFileMissing 文件路径非空但不存在 → 启动报错（fail fast）。
func TestPromptFileMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":"/nonexistent/p.md"}}`), 0o600)

	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file")
	}
}

// TestPromptFileOverride 自定义 file 覆盖内置默认。
func TestPromptFileOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	str2 := filepath.Join(dir, "my.md")
	want := "我的自定义人格入口"
	os.WriteFile(str2, []byte(want), 0o600)

	str := filepath.Join(dir, "c.json")
	// 用 json.Marshal 拼路径：Windows 反斜杠必须转义，手工字符串拼接会产出非法 JSON。
	cfgJSON, err := json.Marshal(map[string]any{"prompt": map[string]any{"mode": "custom", "file": str2}})
	if err != nil {
		t.Fatal(err)
	}

	os.WriteFile(str, cfgJSON, 0o600)

	config, err := Load(str)
	if err != nil {
		t.Fatal(err)
	}

	if config.PromptText != want {
		t.Errorf("PromptText=%q want %q", config.PromptText, want)
	}
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
//
//nolint:paralleltest // 该用例改写进程级状态，必须串行执行
func TestPromptEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "passthrough")

	config, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if config.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", config.Prompt.Mode)
	}
}

// TestPromptLegacyConfigNoImpact 旧 config（无 prompt 段）零影响：mode 缺省 passthrough。
func TestPromptLegacyConfigNoImpact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Prompt.Mode != "passthrough" {
		t.Errorf("legacy config should default to passthrough, got %q", config.Prompt.Mode)
	}

	if config.Listen != ":9999" {
		t.Errorf("listen=%q", config.Listen)
	}
}

// TestUpstreamUserAgentConfig 配置 upstream.user_agent 与 env WB2A_USER_AGENT 均生效，
// 缺省空串保持现状（headers 层回落到 clientUA）。
//
//nolint:paralleltest // 该用例改写进程级状态，必须串行执行
func TestUpstreamUserAgentConfig(t *testing.T) {
	// JSON 配置
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"user_agent":"WorkBuddy/1.2.3"}}`), 0o600)

	config, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	if config.Upstream.UserAgent != "WorkBuddy/1.2.3" {
		t.Errorf("user_agent=%q want WorkBuddy/1.2.3", config.Upstream.UserAgent)
	}
	// 缺省为空
	if c2, lerr := Load(""); lerr != nil || c2.Upstream.UserAgent != "" {
		t.Errorf("default user_agent=%q want empty (err=%v)", c2.Upstream.UserAgent, lerr)
	}
	// env 覆盖
	t.Setenv("WB2A_USER_AGENT", "EnvAgent/9")

	testCase, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if testCase.Upstream.UserAgent != "EnvAgent/9" {
		t.Errorf("env user_agent=%q want EnvAgent/9", testCase.Upstream.UserAgent)
	}
}

// TestLoadConfigPathIsDirectory config 路径是目录时给出可操作提示（Docker bind mount 陷阱）。
// 复现：compose 挂载 ./config.json 但宿主机缺该文件 → Docker 创建同名目录 → 启动失败。
// 旧行为只报 "read config: ... Incorrect function" 之类晦涩错误，无从排查。
func TestLoadConfigPathIsDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	asDir := filepath.Join(dir, "config.json")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Load(asDir)
	if err == nil {
		t.Fatal("want error when config path is a directory")
	}

	msg := err.Error()
	if !strings.Contains(msg, "是目录") {
		t.Errorf("error should explain it is a directory: %v", err)
	}

	if !strings.Contains(msg, "config.example.json") {
		t.Errorf("error should suggest the fix (cp config.example.json): %v", err)
	}
}

// TestMaxInFlightTotalConfig 全局在途上限默认 0（不限），负值 fail fast。
func TestMaxInFlightTotalConfig(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Pool.MaxInFlightTotal != 0 {
		t.Errorf("default max_in_flight_total=%d want 0", config.Pool.MaxInFlightTotal)
	}

	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	config = Default()

	config.Pool.MaxInFlightTotal = -1
	if err := config.normalize(); err == nil {
		t.Error("negative max_in_flight_total should fail fast")
	}
}

// TestServerReadTimeout 入站读取上限（issue #100）：空值回落默认 300s；
// "0" = 显式不限制（0 是合法值不回落）；负值 fail fast（静默钳 0 会把保护悄悄关掉）。
func TestServerReadTimeout(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Server.ReadTimeout != "300s" {
		t.Errorf("default read_timeout=%q want 300s", config.Server.ReadTimeout)
	}

	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if config.ServerReadTimeoutDur != 300*time.Second {
		t.Errorf("default dur=%v want 300s", config.ServerReadTimeoutDur)
	}

	config = Default()

	config.Server.ReadTimeout = "" // 显式清空 = 未配置 → 回落默认
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize empty: %v", err)
	}

	if config.ServerReadTimeoutDur != 300*time.Second {
		t.Errorf("empty dur=%v want 300s", config.ServerReadTimeoutDur)
	}

	config = Default()

	config.Server.ReadTimeout = "0" // 显式 0 = 不限制（http.Server ReadTimeout 0 即无超时）
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize zero: %v", err)
	}

	if config.ServerReadTimeoutDur != 0 {
		t.Errorf("zero dur=%v want 0", config.ServerReadTimeoutDur)
	}

	config = Default()

	config.Server.ReadTimeout = "-5s"
	if err := config.normalize(); err == nil {
		t.Error("negative read_timeout should fail fast")
	}

	config = Default()

	config.Server.ReadTimeout = "bogus"
	if err := config.normalize(); err == nil {
		t.Error("unparsable read_timeout should fail fast")
	}
}

// TestBackgroundTargetPreserved 自定义后台模型目标不被 aliases / 空 checkin_hours 重置。
func TestBackgroundTargetPreserved(t *testing.T) {
	t.Parallel()

	config := Default()
	config.Models.BackgroundTarget = "custom-target"
	config.Models.Aliases = map[string]string{"fast": "glm-5.2"}

	config.Schedule.CheckinHours = nil
	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	if config.Models.BackgroundTarget != "custom-target" {
		t.Fatalf("target=%q want custom-target", config.Models.BackgroundTarget)
	}
}

// TestDailyLimitsConfig 每日限额负值拒绝，0 = 关闭（issue #95）。
func TestDailyLimitsConfig(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Limits.DailyTokens != 0 || config.Limits.DailyCredits != 0 {
		t.Errorf("default limits=%+v want zero", config.Limits)
	}

	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	config.Limits.DailyTokens = -1
	if err := config.normalize(); err == nil {
		t.Error("negative daily_tokens should fail")
	}
}

// TestServerMaxBodyBytes 聊天入口请求体上限（issue #86）：默认 16 MiB，
// 0 = 不限制，负值 fail fast。
func TestServerMaxBodyBytes(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Server.MaxBodyBytes != 16<<20 {
		t.Errorf("default max_body_bytes=%d want %d", config.Server.MaxBodyBytes, 16<<20)
	}

	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	config = Default()

	config.Server.MaxBodyBytes = 0
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize zero: %v", err)
	}

	if config.Server.MaxBodyBytes != 0 {
		t.Errorf("zero max_body_bytes=%d want 0", config.Server.MaxBodyBytes)
	}

	config = Default()

	config.Server.MaxBodyBytes = -1
	if err := config.normalize(); err == nil {
		t.Error("negative max_body_bytes should fail fast")
	}
}

// TestCORSOrigins 校验 server.cors_origins 的规范化与非法值拒绝（issue #91）。
func TestCORSOrigins(t *testing.T) {
	t.Parallel()

	config := Default()

	config.Server.CORSOrigins = []string{" HTTP://LocalHost:3000/ ", "http://localhost:3000", "https://app.example.com"}
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	want := []string{"http://localhost:3000", "https://app.example.com"}
	if len(config.Server.CORSOrigins) != len(want) {
		t.Fatalf("origins=%v want %v", config.Server.CORSOrigins, want)
	}

	for i := range want {
		if config.Server.CORSOrigins[i] != want[i] {
			t.Errorf("origins[%d]=%q want %q", i, config.Server.CORSOrigins[i], want[i])
		}
	}

	bad := []string{"*", "localhost:3000", "ftp://example.com", "https://example.com/path", "https://example.com?q=1", "https://u:p@example.com"}
	for _, origin := range bad {
		c := Default()

		c.Server.CORSOrigins = []string{origin}
		if err := c.normalize(); err == nil {
			t.Errorf("origin %q should be rejected", origin)
		}
	}
}

// TestBackgroundModels 后台模型名单规范化与默认目标（issue #103）。
func TestBackgroundModels(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Models.BackgroundTarget != "deepseek-v4.1-flash" {
		t.Errorf("default target=%q", config.Models.BackgroundTarget)
	}

	config.Models.BackgroundNames = []string{" GPT-5-Background ", "gpt-5-background", "", "memory-gen"}
	if err := config.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	want := []string{"gpt-5-background", "memory-gen"}
	if len(config.Models.BackgroundNames) != len(want) {
		t.Fatalf("names=%v want %v", config.Models.BackgroundNames, want)
	}

	for i := range want {
		if config.Models.BackgroundNames[i] != want[i] {
			t.Errorf("names[%d]=%q want %q", i, config.Models.BackgroundNames[i], want[i])
		}
	}
}

// TestLocalWebToolRounds 本地上网工具轮数默认 3、上限 8（issue #104）。
func TestLocalWebToolRounds(t *testing.T) {
	t.Parallel()

	config := Default()
	if config.Features.LocalWebToolRounds != 3 {
		t.Errorf("default rounds=%d want 3", config.Features.LocalWebToolRounds)
	}

	config.Features.LocalWebToolRounds = 99
	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	if config.Features.LocalWebToolRounds != 8 {
		t.Errorf("clamped rounds=%d want 8", config.Features.LocalWebToolRounds)
	}

	config = Default()

	config.Features.LocalWebToolRounds = 0
	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	if config.Features.LocalWebToolRounds != 3 {
		t.Errorf("zero rounds=%d want 3", config.Features.LocalWebToolRounds)
	}
}

// mustParseConfig 解析一段配置 JSON（不读环境变量），失败即终止测试。
func mustParseConfig(t *testing.T, body string) *Config {
	t.Helper()

	c, err := ParseConfig([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	return c
}

// writeTempConfig 把 body 写入临时目录下的 c.json，返回文件路径。
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()

	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return fp
}

// loadTempConfig 写入临时配置并 Load，失败即终止测试。
func loadTempConfig(t *testing.T, body string) *Config {
	t.Helper()

	c, err := Load(writeTempConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func checkInt(t *testing.T, name string, got, want int) {
	t.Helper()

	if got != want {
		t.Errorf("%s=%d want %d", name, got, want)
	}
}

func checkFloat(t *testing.T, name string, got, want float64) {
	t.Helper()

	if got != want {
		t.Errorf("%s=%v want %v", name, got, want)
	}
}

func checkBool(t *testing.T, name string, got, want bool) {
	t.Helper()

	if got != want {
		t.Errorf("%s=%v want %v", name, got, want)
	}
}

func checkStr(t *testing.T, name, got, want string) {
	t.Helper()

	if got != want {
		t.Errorf("%s=%q want %q", name, got, want)
	}
}

func checkDur(t *testing.T, name string, got, want time.Duration) {
	t.Helper()

	if got != want {
		t.Errorf("%s=%v want %v", name, got, want)
	}
}

// checkHours 断言小时数组与期望逐元素相等（长度与顺序都校验）。
func checkHours(t *testing.T, name string, got, want []int) {
	t.Helper()

	if len(got) != len(want) {
		t.Errorf("%s=%v want %v", name, got, want)
		return
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s=%v want %v", name, got, want)
			return
		}
	}
}

// checkLoggingArchive 断言归档三项（开关 / 保留天数 / 上限 MiB）。
func checkLoggingArchive(t *testing.T, tag string, enabled bool, days, mb int, wantEnabled bool, wantDays, wantMB int) {
	t.Helper()

	if enabled != wantEnabled || days != wantDays || mb != wantMB {
		t.Fatalf("%s logging archive = enabled=%v days=%d mb=%d, want %v/%d/%d",
			tag, enabled, days, mb, wantEnabled, wantDays, wantMB)
	}
}

// TestCORSOriginDefaultPort 规范化时去掉默认端口，避免浏览器 Origin 精确匹配失败。
func TestCORSOriginDefaultPort(t *testing.T) {
	t.Parallel()

	config := Default()

	config.Server.CORSOrigins = []string{"https://Example.com:443", "http://Example.com:80", "http://[::1]:80"}
	if err := config.normalize(); err != nil {
		t.Fatal(err)
	}

	want := []string{"https://example.com", "http://example.com", "http://[::1]"}
	if len(config.Server.CORSOrigins) != len(want) {
		t.Fatalf("origins=%v want %v", config.Server.CORSOrigins, want)
	}

	for i := range want {
		if config.Server.CORSOrigins[i] != want[i] {
			t.Errorf("origins[%d]=%q want %q", i, config.Server.CORSOrigins[i], want[i])
		}
	}
}
