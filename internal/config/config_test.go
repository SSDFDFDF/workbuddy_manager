package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy_manager/internal/prompt"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 600 {
		t.Errorf("soft=%v want 600s", c.SoftRateDur)
	}
}

func TestPanelPackageDetailLimit(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Panel.PackageDetailLimit != 5 {
		t.Fatalf("default package_detail_limit=%d want 5", c.Panel.PackageDetailLimit)
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
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Logging.RequestArchiveEnabled || c.Logging.RequestRetentionDays != 7 || c.Logging.RequestArchiveMaxMB != 100 {
		t.Fatalf("logging defaults = %+v", c.Logging)
	}
	// 来源记录（IP/UA）缺省开启：键缺席时必须保持 true，只有显式 false 才关闭。
	if !c.Logging.RequestClientInfo {
		t.Fatalf("request_client_info default = false, want true: %+v", c.Logging)
	}
	configured, err := ParseConfig([]byte(`{"logging":{"request_archive_enabled":false,"request_retention_days":30,"request_archive_max_mb":500}}`))
	if err != nil {
		t.Fatal(err)
	}
	if configured.Logging.RequestArchiveEnabled || configured.Logging.RequestRetentionDays != 30 || configured.Logging.RequestArchiveMaxMB != 500 {
		t.Fatalf("configured logging = %+v", configured.Logging)
	}
	off, err := ParseConfig([]byte(`{"logging":{"request_client_info":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if off.Logging.RequestClientInfo {
		t.Fatalf("explicit false ignored: %+v", off.Logging)
	}
	fallback, err := ParseConfig([]byte(`{"logging":{"request_retention_days":0,"request_archive_max_mb":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Logging.RequestRetentionDays != 7 || fallback.Logging.RequestArchiveMaxMB != 100 {
		t.Fatalf("logging fallback = %+v", fallback.Logging)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

// TestUnknownKeysIgnoredOnLoad 未知/退役键在读取时直接忽略：不阻断启动、
// 不参与语义、只告警，并且**读盘不改盘**（没有迁移回写这回事）。
// 它们要等到下一次保存（全量覆盖）才会从文件里消失。
func TestUnknownKeysIgnoredOnLoad(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		goneKey string
		check   func(t *testing.T, c *Config)
	}{
		{
			name:    "退役的 cooldown.hard_credit",
			body:    `{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`,
			goneKey: `"hard_credit"`,
			check: func(t *testing.T, c *Config) {
				if c.SoftRateDur.Seconds() != 30 {
					t.Errorf("soft_rate=%v want 30s（同段其余键照常生效）", c.SoftRateDur)
				}
			},
		},
		{
			name:    "退役的 schedule.travel_interval_minutes",
			body:    `{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`,
			goneKey: `"travel_interval_minutes"`,
			check: func(t *testing.T, c *Config) {
				if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
					t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", c.Schedule.CheckinHours)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			if err := os.WriteFile(fp, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(fp)
			c, err := Load(fp)
			if err != nil {
				t.Fatalf("未知键不得阻断启动: %v", err)
			}
			if !hasWarning(c.Warnings, strings.Trim(tc.goneKey, `"`)) {
				t.Errorf("未知键必须出现在告警里: %v", c.Warnings)
			}
			tc.check(t, c)
			after, _ := os.ReadFile(fp)
			if string(before) != string(after) {
				t.Errorf("读盘不得改盘（已无迁移回写）：\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if !c.Pool.PreferExpiring || c.ExpiringSoonDur != 7*24*time.Hour {
		t.Errorf("expiring defaults: enabled=%v window=%v", c.Pool.PreferExpiring, c.ExpiringSoonDur)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h", c.SoftRateMaxDur)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
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
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.Pool.PreferExpiring || c.ExpiringSoonDur != 72*time.Hour {
		t.Errorf("expiring override: enabled=%v window=%v", c.Pool.PreferExpiring, c.ExpiringSoonDur)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestSoftRateMaxParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"5m","soft_rate_max":"45m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateDur.Minutes() != 5 {
		t.Errorf("soft_rate=%v want 5m", c.SoftRateDur)
	}
	if c.SoftRateMaxDur.Minutes() != 45 {
		t.Errorf("soft_rate_max=%v want 45m", c.SoftRateMaxDur)
	}
}

func TestLegacyConfigKeepsPreferExpiringEnabled(t *testing.T) {
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
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"expiring_soon":"-1h"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpiringSoonDur != 0 || c.Pool.ExpiringSoon != "0" {
		t.Fatalf("negative window=%v/%q want 0/0", c.ExpiringSoonDur, c.Pool.ExpiringSoon)
	}
}

func TestSoftRateMaxEmptyFallsBackToDefault(t *testing.T) {
	// 键缺席 → Default() 的 2h 保留（空串无法 ParseDuration）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"90s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h fallback", c.SoftRateMaxDur)
	}
}

func TestBadSoftRateMax(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate_max":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad soft_rate_max")
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestScheduleEnabledByDefault 四个任务的 enabled 开关默认均为 true：
// 老 config 不写这些键，行为必须与从前完全一致。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("travel/activity enabled defaults want true/true, got %v/%v",
			c.Schedule.TravelEnabled, c.Schedule.ActivityEnabled)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写签到/保活小时数组，无新键）加载后仍是启用态，
// 新开关缺省 true、新 hours 回落默认——对老配置零影响。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("new switches must default true on legacy config: %+v", c.Schedule)
	}
	if len(c.Schedule.CheckinHours) != 2 {
		t.Errorf("checkin_hours=%v", c.Schedule.CheckinHours)
	}
	// 新 hours 缺省 → 回落默认（非空）。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", c.Schedule.KeepaliveHours)
	}
}

// TestScheduleTravelActivityExplicitDisable 显式关闭旅行/活跃上报开关。
func TestScheduleTravelActivityExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_enabled":false,"activity_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.TravelEnabled || c.Schedule.ActivityEnabled {
		t.Errorf("want travel/activity disabled: %+v", c.Schedule)
	}
	// 签到/保活开关缺省 true（互不干扰）。
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("checkin/keepalive should stay enabled: %+v", c.Schedule)
	}
	// hours 仍回落默认。
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want default [9,21] even when disabled", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want default [10] even when disabled", c.Schedule.ActivityHours)
	}
}

// TestScheduleTravelActivityInvalidHoursRejected 旅行/活跃非法小时报错并指向正确开关。
func TestScheduleTravelActivityInvalidHoursRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"travel_hours":[25]}}`, "travel_enabled"},
		{`{"schedule":{"travel_hours":[-1]}}`, "travel_enabled"},
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"activity_hours":[-1]}}`, "activity_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

// TestScheduleTravelActivityExplicitHours 显式配置旅行/活跃小时。
func TestScheduleTravelActivityExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_hours":[9,21],"activity_hours":[11]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9 21]", c.Schedule.TravelHours)
	}
	if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 11 {
		t.Errorf("activity_hours=%v want [11]", c.Schedule.ActivityHours)
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 10 || c.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", c.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null,"travel_hours":null,"activity_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[],"travel_hours":[],"activity_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
				t.Errorf("checkin_hours=%v want default [9 21]", c.Schedule.CheckinHours)
			}
			if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
				t.Errorf("keepalive_hours=%v want default [22]", c.Schedule.KeepaliveHours)
			}
			if len(c.Schedule.TravelHours) != 2 || c.Schedule.TravelHours[0] != 9 || c.Schedule.TravelHours[1] != 21 {
				t.Errorf("travel_hours=%v want default [9 21]", c.Schedule.TravelHours)
			}
			if len(c.Schedule.ActivityHours) != 1 || c.Schedule.ActivityHours[0] != 10 {
				t.Errorf("activity_hours=%v want default [10]", c.Schedule.ActivityHours)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
			if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
		})
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

func TestWriteDefault(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "sub", "config.json") // 顺带验证父目录自动创建
	key, err := WriteDefault(fp)
	if err != nil {
		t.Fatal(err)
	}
	// key 形如 sk-<24字符随机串>，两次生成不重复
	if !strings.HasPrefix(key, "sk-") || len(key) < 20 {
		t.Errorf("key=%q want sk-<random>", key)
	}
	if key2, _ := WriteDefault(filepath.Join(dir, "another.json")); key2 == key {
		t.Errorf("two generated keys identical: %q", key)
	}
	// 落盘文件可被 Load 正常加载，推荐值齐备且 api_key 生效
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if c.APIKey != key {
		t.Errorf("api_key=%q want %q", c.APIKey, key)
	}
	if c.Listen != ":7863" || c.AuthDir != "./auths" || c.StateFile != "./data/state.json" {
		t.Errorf("generated defaults off: %+v", c)
	}
	if len(c.Schedule.CheckinHours) == 0 || !c.Schedule.CheckinEnabled {
		t.Errorf("generated schedule off: %+v", c.Schedule)
	}
	// 已存在的文件不覆盖：二次写入同一路径必须报错
	if _, err := WriteDefault(fp); err == nil {
		t.Error("WriteDefault must refuse to overwrite existing file")
	}
}

func TestBalanceRefreshDefaults(t *testing.T) {
	// 缺省：启用 + 30 分钟
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.BalanceRefreshEnabled || c.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("default balance refresh: enabled=%v interval=%v", c.Schedule.BalanceRefreshEnabled, c.BalanceRefreshInterval)
	}
	// 显式配置 10 分钟
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":10}}`), 0o600)
	c2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c2.BalanceRefreshInterval != 10*time.Minute {
		t.Errorf("interval=%v want 10m", c2.BalanceRefreshInterval)
	}
	// 显式关闭：interval 归零（不启动）
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_enabled":false}}`), 0o600)
	c3, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c3.BalanceRefreshInterval != 0 {
		t.Errorf("disabled interval=%v want 0", c3.BalanceRefreshInterval)
	}
	// 启用但 minutes<=0 → 回落默认 30
	os.WriteFile(fp, []byte(`{"schedule":{"balance_refresh_minutes":-5}}`), 0o600)
	c4, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c4.BalanceRefreshInterval != 5*time.Minute {
		t.Errorf("fallback interval=%v want 30m", c4.BalanceRefreshInterval)
	}
}

// TestPromptDefaultNone 默认 prompt.mode=none：不改写客户端 system（零回归）。
func TestPromptDefaultNone(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != prompt.ModeNone {
		t.Errorf("prompt.mode=%q want none", c.Prompt.Mode)
	}
	// none 不解析正文（热路径不需要）；但分域规则仍在（mode 已定），
	// 切到 replace/append/after 后重启即生效。
	if c.PromptRules[""].Mode != prompt.ModeNone {
		t.Errorf("default rule mode=%q want none", c.PromptRules[""].Mode)
	}
}

// TestPromptExplicitNone none 模式不加载文本（透传客户端原始 system）。
func TestPromptExplicitNone(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"none"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != prompt.ModeNone {
		t.Errorf("mode=%q want none", c.Prompt.Mode)
	}
	if c.PromptText != "" || c.PromptRules[""].Text != "" {
		t.Errorf("none should not load text: %d / %d", len(c.PromptText), len(c.PromptRules[""].Text))
	}
}

// TestPromptInvalidMode 非法 mode 启动报错。
func TestPromptInvalidMode(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"bogus"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
}

// TestPromptFileMissing 文件路径非空但不存在 → 启动报错（fail fast）。
func TestPromptFileMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"replace","file":"/nonexistent/p.md"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file")
	}
}

// TestPromptFileOverride 自定义 file 覆盖内置默认。
func TestPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my.md")
	want := "我的自定义人格入口"
	os.WriteFile(pf, []byte(want), 0o600)
	cf := filepath.Join(dir, "c.json")
	// 用 json.Marshal 拼路径：Windows 反斜杠必须转义，手工字符串拼接会产出非法 JSON。
	cfgJSON, err := json.Marshal(map[string]any{"prompt": map[string]any{"mode": "replace", "file": pf}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cf, cfgJSON, 0o600)
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptRules["cn"].Text != want || c.PromptText != want {
		t.Errorf("file text=%q want %q", c.PromptRules["cn"].Text, want)
	}
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
func TestPromptEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "none")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != prompt.ModeNone {
		t.Errorf("mode=%q want none", c.Prompt.Mode)
	}
}

// TestPromptLegacyConfigNoImpact 旧 config（无 prompt 段）零影响：mode 缺省 passthrough。
func TestPromptLegacyConfigNoImpact(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "none" {
		t.Errorf("legacy config should default to passthrough, got %q", c.Prompt.Mode)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen=%q", c.Listen)
	}
}

// TestUpstreamUserAgentConfig 配置 upstream.user_agent 与 env WB2A_USER_AGENT 均生效，
// 缺省空串保持现状（headers 层回落到 clientUA）。
func TestUpstreamUserAgentConfig(t *testing.T) {
	// 旧的全局 UA 键忽略并告警（新结构见 upstream.profiles）。
	legacy, err := ParseConfig([]byte(`{"upstream":{"user_agent":"retired"}}`))
	if err != nil {
		t.Fatalf("retired global UA key must not block startup: %v", err)
	}
	if !hasWarning(legacy.Warnings, "upstream.user_agent") {
		t.Errorf("retired key must be reported: %v", legacy.Warnings)
	}
	c, err := ParseConfig([]byte(`{"config_version":3,"upstream":{"profiles":{"global":{"client_version":"6.0.0","cli_version":"3.0.0","user_agents":{"chat":"custom/1"}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.Profiles["global"].UserAgents["chat"] != "custom/1" || c.Upstream.Profiles["cn"].ClientVersion != "5.7.6" {
		t.Fatal(c.Upstream.Profiles)
	}
}

// hasWarning 报告告警列表是否含指定片段（告警文案含点分路径）。
func hasWarning(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}

// TestUnknownConfigKeyTolerated 未知/笔误键只告警，不阻断：读取时直接忽略
// （不参与语义），保存时随全量覆盖消失。若在这里拒收，用户手改配置里残留一个
// 旧键就再也启动不了。
func TestUnknownConfigKeyTolerated(t *testing.T) {
	c, err := ParseConfig([]byte(`{"config_version":3,"upstream":{"profiles":{"cn":{"client_verison":"9.9.9"}}},"listen":":1234"}`))
	if err != nil {
		t.Fatalf("unknown key must not fail parse: %v", err)
	}
	if c.Listen != ":1234" {
		t.Errorf("known keys must still apply: %q", c.Listen)
	}
	if !hasWarning(c.Warnings, "upstream.profiles.cn.client_verison") {
		t.Errorf("typo inside nested profile must be reported: %v", c.Warnings)
	}
}

// TestPromptLegacyModeRejected 旧 mode 取值不兼容、不迁移：直接报错。
func TestPromptLegacyModeRejected(t *testing.T) {
	for _, v := range []string{"passthrough", "custom", "client_after", "prepend"} {
		raw := []byte(`{"prompt":{"mode":"` + v + `"}}`)
		if _, err := ParseConfig(raw); err == nil {
			t.Errorf("prompt.mode=%q must be rejected (no migration)", v)
		}
	}
}

// TestPromptRealmProfiles 分域配置：mode 逐项回落，素材整体覆盖。
func TestPromptRealmProfiles(t *testing.T) {
	c, err := ParseConfig([]byte(`{
	  "prompt": {
	    "mode": "after",
	    "preset": "default",
	    "profiles": {
	      "cn": {"preset": "official-quick", "text": "CN 内联"},
	      "global": {"mode": "replace", "preset": "official-quick"}
	    }
	  }
	}`))
	if err != nil {
		t.Fatal(err)
	}
	// 默认域：顶层素材（default 预设）；mode=after 非 inject → 注入锚点在配置期剔除。
	if r := c.PromptRules[""]; r.Mode != prompt.ModeAfter || r.Text != prompt.StripSlot(prompt.Builtin("default", "cn")) {
		t.Errorf("default rule=%+v", r)
	}
	if strings.Contains(c.PromptRules[""].Text, prompt.ClientSlot) {
		t.Error("after 模式下锚点标记不得出现在规则正文里")
	}
	// CN：素材被 realm 整体覆盖（text 胜过 preset），mode 回落顶层。
	if r := c.PromptRules["cn"]; r.Mode != prompt.ModeAfter || r.Text != "CN 内联" {
		t.Errorf("cn rule=%+v", r)
	}
	// Global：mode 被覆盖，素材取 global 的 official-quick（mode=replace → 锚点剥离）。
	if r := c.PromptRules["global"]; r.Mode != prompt.ModeReplace || r.Text != prompt.StripSlot(prompt.Builtin("official-quick", "global")) {
		t.Errorf("global rule=%+v", r)
	}
	if c.PromptText != prompt.StripSlot(prompt.Builtin("default", "cn")) {
		t.Errorf("PromptText(legacy)=%d bytes", len(c.PromptText))
	}
}

// TestPromptInlineText 内联正文：无需落盘文件即可直接配置内容。
func TestPromptInlineText(t *testing.T) {
	c, err := ParseConfig([]byte(`{"prompt":{"mode":"replace","text":"你是一个测试助手"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptRules["cn"].Text != "你是一个测试助手" || c.PromptText != "你是一个测试助手" {
		t.Errorf("inline text not applied: %+v", c.PromptRules)
	}
}

// TestPromptUnknownRealmDropped 未知 realm 告警并忽略（不阻断启动）。
func TestPromptUnknownRealmDropped(t *testing.T) {
	c, err := ParseConfig([]byte(`{"prompt":{"mode":"replace","profiles":{"eu":{"preset":"minimal"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Prompt.Profiles["eu"]; ok {
		t.Error("unknown realm must be dropped")
	}
	if !hasWarning(c.Warnings, "prompt.profiles.eu") {
		t.Errorf("unknown realm must be reported: %v", c.Warnings)
	}
}

// TestPromptUnknownModeRejected 非法 mode（顶层/分域）都报错。
func TestPromptUnknownModeRejected(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"prompt":{"preset":"bogus"}}`)); err == nil {
		t.Error("invalid preset must be rejected")
	}
	if _, err := ParseConfig([]byte(`{"prompt":{"mode":"replace","profiles":{"cn":{"mode":"bogus"}}}}`)); err == nil {
		t.Error("invalid realm mode must be rejected")
	}
}

// TestLoadConfigPathIsDirectory config 路径是目录时给出可操作提示（Docker bind mount 陷阱）。
// 复现：compose 挂载 ./config.json 但宿主机缺该文件 → Docker 创建同名目录 → 启动失败。
// 旧行为只报 "read config: ... Incorrect function" 之类晦涩错误，无从排查。
func TestLoadConfigPathIsDirectory(t *testing.T) {
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

// TestServerReadTimeout 入站读取上限（issue #100）：空值回落默认 300s；
// "0" = 显式不限制（0 是合法值不回落）；负值 fail fast（静默钳 0 会把保护悄悄关掉）。
func TestServerReadTimeout(t *testing.T) {
	c := Default()
	if c.Server.ReadTimeout != "300s" {
		t.Errorf("default read_timeout=%q want 300s", c.Server.ReadTimeout)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.ServerReadTimeoutDur != 300*time.Second {
		t.Errorf("default dur=%v want 300s", c.ServerReadTimeoutDur)
	}

	c = Default()
	c.Server.ReadTimeout = "" // 显式清空 = 未配置 → 回落默认
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize empty: %v", err)
	}
	if c.ServerReadTimeoutDur != 300*time.Second {
		t.Errorf("empty dur=%v want 300s", c.ServerReadTimeoutDur)
	}

	c = Default()
	c.Server.ReadTimeout = "0" // 显式 0 = 不限制（http.Server ReadTimeout 0 即无超时）
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize zero: %v", err)
	}
	if c.ServerReadTimeoutDur != 0 {
		t.Errorf("zero dur=%v want 0", c.ServerReadTimeoutDur)
	}

	c = Default()
	c.Server.ReadTimeout = "-5s"
	if err := c.normalize(); err == nil {
		t.Error("negative read_timeout should fail fast")
	}

	c = Default()
	c.Server.ReadTimeout = "bogus"
	if err := c.normalize(); err == nil {
		t.Error("unparsable read_timeout should fail fast")
	}
}

func TestResinConfig(t *testing.T) {
	// 空 URL = 未接入：ProxyClient 为 nil，出站零改动。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize default: %v", err)
	}
	if c.ProxyClient != nil {
		t.Error("default resin must be disabled (nil client)")
	}
	if c.ResinMode != "reverse" || c.ResinAuthVersion != "V1" {
		t.Errorf("default resin mode/auth = %q/%q", c.ResinMode, c.ResinAuthVersion)
	}

	// 完整配置 → 解析成功，模式/认证版本落定。
	c = Default()
	c.ResinURL = "http://127.0.0.1:2260/my-token"
	c.ResinPlatformName = "Default"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize resin: %v", err)
	}
	if c.ProxyClient == nil || !c.ProxyClient.Enabled() {
		t.Fatal("resin should be enabled")
	}
	if c.ProxyClient.Mode() != "reverse" || c.ProxyClient.AuthVersion() != "V1" {
		t.Errorf("resin mode/auth = %q/%q", c.ProxyClient.Mode(), c.ProxyClient.AuthVersion())
	}

	// 非法配置 fail fast：缺 platform。
	c = Default()
	c.ResinURL = "http://127.0.0.1:2260/my-token"
	if err := c.normalize(); err == nil {
		t.Error("missing platform should fail")
	}
	// platform 含 '.'（会被正向代理凭证拆分语义污染）。
	c = Default()
	c.ResinURL = "http://127.0.0.1:2260/my-token"
	c.ResinPlatformName = "a.b"
	if err := c.normalize(); err == nil {
		t.Error("platform with dot should fail")
	}
}

func TestModelDefaultRealm(t *testing.T) {
	// 缺省 cn（历史行为零回归）。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.ModelDefaultRealm != "cn" {
		t.Errorf("default model_default_realm=%q want cn", c.ModelDefaultRealm)
	}
	// 合法值：global / auto / auto:global,cn（大小写与空白归一，支持别名）。
	for _, tc := range []struct{ in, want string }{
		{"global", "global"},
		{" Auto ", "auto"},
		{"CN", "cn"},
		{"auto:cn,global", "auto"},
		{"cn,global", "auto"},
		{"auto:global,cn", "auto:global,cn"},
		{"global,cn", "auto:global,cn"},
		{" Auto: Global, CN ", "auto:global,cn"},
	} {
		cc, err := ParseConfig([]byte(`{"model_default_realm":"` + tc.in + `"}`))
		if err != nil {
			t.Fatalf("parse %q: %v", tc.in, err)
		}
		if cc.ModelDefaultRealm != tc.want {
			t.Errorf("model_default_realm %q -> %q want %q", tc.in, cc.ModelDefaultRealm, tc.want)
		}
	}
	// 非法值回落 cn，不启动失败（向后兼容：旧 config 无此键/手写错值）。
	bad, err := ParseConfig([]byte(`{"model_default_realm":"bogus"}`))
	if err != nil {
		t.Fatalf("illegal value must not fail: %v", err)
	}
	if bad.ModelDefaultRealm != "cn" {
		t.Errorf("illegal value -> %q want cn", bad.ModelDefaultRealm)
	}
}

func TestModelDefaultRealmEnv(t *testing.T) {
	t.Setenv("WB2A_MODEL_DEFAULT_REALM", "auto")
	c := Default()
	applyEnv(c)
	if c.ModelDefaultRealm != "auto" {
		t.Errorf("env override = %q want auto", c.ModelDefaultRealm)
	}
}

func TestPlainProxyConfig(t *testing.T) {
	// 缺省：未接入（nil client）。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.ProxyClient != nil {
		t.Error("default proxy must be disabled (nil client)")
	}

	// proxy_url → 普通正向代理。
	for _, url := range []string{
		"http://user:pass@127.0.0.1:8080",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	} {
		cc, err := ParseConfig([]byte(`{"proxy_url":"` + url + `"}`))
		if err != nil {
			t.Fatalf("parse %q: %v", url, err)
		}
		if cc.ProxyClient == nil || !cc.ProxyClient.Enabled() || cc.ProxyClient.Mode() != "plain" {
			t.Errorf("proxy_url %q -> %+v", url, cc.ProxyClient)
		}
	}

	// 非法 scheme fail fast。
	if _, err := ParseConfig([]byte(`{"proxy_url":"ftp://127.0.0.1:21"}`)); err == nil {
		t.Error("bad proxy scheme should fail")
	}

	// proxy_url 与 resin_url 互斥。
	if _, err := ParseConfig([]byte(`{"proxy_url":"http://127.0.0.1:8080","resin_url":"http://127.0.0.1:2260/tok","resin_platform_name":"P"}`)); err == nil {
		t.Error("proxy_url + resin_url should fail (mutually exclusive)")
	}
}

func TestPlainProxyEnv(t *testing.T) {
	t.Setenv("WB2A_PROXY_URL", "socks5://127.0.0.1:1080")
	c := Default()
	applyEnv(c)
	if c.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Errorf("env override = %q", c.ProxyURL)
	}
}

// 未知键识别必须只认"真的不认识"的键：
// 嵌套 map 值类型里的合法字段（prompt.profiles.cn.mode）不能被误删。
// 回归：早期实现把零值 Config 序列化成已知树，nil map → null，导致这些
// 子键全部被判未知并 prune 掉。
// 未知键只告警，不影响解析结果：合法嵌套键（含 upstream.profiles / prompt.profiles
// 这类 map 内部键）照常生效；未知键与拼错的键既不入配置语义，也不会让解析失败。
// （写侧不再需要剪枝：保存是全量覆盖，未知键自然消失——见 cmd/server 的保存测试。）
func TestUnknownKeysDoNotAffectConfig(t *testing.T) {
	raw := []byte(`{
		"listen": ":1",
		"media":  {"tool_images": "hoist"},
		"prompt": {
			"mode":   "after",
			"preset": "official-ask",
			"profiles": {
				"cn":     {"mode": "replace", "preset": "official-plan", "text": "x"},
				"global": {"text": "y"}
			}
		},
		"upstream": {"profiles": {"cn": {"client_version": "9.9.9", "client_verison": "typo"}}},
		"features":  {"sanitize_blacklist_fingerprints": true},
		"bogus_top": 1
	}`)
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("未知键不得让解析失败: %v", err)
	}

	// 合法嵌套键必须全部生效。
	if c.Listen != ":1" {
		t.Errorf("listen=%q", c.Listen)
	}
	if c.Media.ToolImages != "hoist" {
		t.Errorf("media.tool_images=%q（已知键不得被忽略）", c.Media.ToolImages)
	}
	if c.Prompt.Mode != "after" || c.Prompt.Preset != "official-ask" {
		t.Errorf("prompt mode/preset = %q/%q", c.Prompt.Mode, c.Prompt.Preset)
	}
	cn := c.Prompt.Profiles["cn"]
	if cn.Mode != "replace" || cn.Preset != "official-plan" || cn.Text != "x" {
		t.Errorf("prompt.profiles.cn = %+v", cn)
	}
	if gl := c.Prompt.Profiles["global"]; gl.Text != "y" {
		t.Errorf("prompt.profiles.global = %+v", gl)
	}
	if got := c.Upstream.Profiles["cn"].ClientVersion; got != "9.9.9" {
		t.Errorf("upstream.profiles.cn.client_version=%q", got)
	}

	// 未知键只告警（顶层未知段、拼错的嵌套键都要能被发现）。
	for _, want := range []string{"features", "bogus_top", "upstream.profiles.cn.client_verison"} {
		if !hasWarning(c.Warnings, want) {
			t.Errorf("缺少对 %q 的告警: %v", want, c.Warnings)
		}
	}
}

// media.tool_images：默认 auto（按入口默认策略）；显式取值大小写/空白不敏感；
// 非法值在装载阶段 fail fast（不留到请求时才报错）。
func TestMediaToolImagesPolicy(t *testing.T) {
	if c := Default(); c.Media.ToolImages != "auto" {
		t.Fatalf("default tool_images=%q want auto", c.Media.ToolImages)
	}
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`{"media":{"tool_images":"hoist"}}`, "hoist"},
		{`{"media":{"tool_images":" HoiSt "}}`, "hoist"},
		{`{"media":{"tool_images":"passthrough"}}`, "passthrough"},
		{`{"media":{"tool_images":"reject"}}`, "reject"},
		{`{"media":{"tool_images":"auto"}}`, "auto"},
		{`{"media":{"tool_images":""}}`, ""},
	} {
		c, err := ParseConfig([]byte(tc.raw))
		if err != nil {
			t.Fatalf("parse %s: %v", tc.raw, err)
		}
		if got := c.Media.ToolImages; got != tc.want {
			t.Fatalf("%s → %q want %q", tc.raw, got, tc.want)
		}
	}
	if _, err := ParseConfig([]byte(`{"media":{"tool_images":"hoist-all"}}`)); err == nil || !strings.Contains(err.Error(), "media.tool_images") {
		t.Fatalf("invalid policy accepted: %v", err)
	}
}

// media.image_transcode / media.image_max_dimension：默认全关；显式取值生效；
// 非法档位在装载阶段 fail fast（不留到请求时才报错）。
func TestMediaImagePolicy(t *testing.T) {
	c := Default()
	if c.Media.ImageTranscode || c.Media.ImageMaxDimension != 0 {
		t.Fatalf("image policy must default to off: %+v", c.Media)
	}
	parsed, err := ParseConfig([]byte(`{"media":{"image_transcode":true,"image_max_dimension":1080}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Media.ImageTranscode || parsed.Media.ImageMaxDimension != 1080 {
		t.Fatalf("image policy not parsed: %+v", parsed.Media)
	}
	for _, raw := range []string{
		`{"media":{"image_max_dimension":1234}}`,
		`{"media":{"image_max_dimension":-1}}`,
	} {
		if _, err := ParseConfig([]byte(raw)); err == nil || !strings.Contains(err.Error(), "media.image_max_dimension") {
			t.Fatalf("%s accepted: %v", raw, err)
		}
	}
}

// TestIngressConfigDefaultsAndValidation 入站准入配置的缺省、显式值与非法值。
//
// 语义要点：0 是合法值（= 该项不限制），负值必须 fail fast——静默钳 0 会把
// 保护悄悄关掉，这正是 read_timeout 已经采用的处理风格。
func TestIngressConfigDefaultsAndValidation(t *testing.T) {
	// 缺省：三个键都给推荐值（配置里不写也应生效）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.MaxInflightRequests != 64 || c.Server.MaxInflightBytesMB != 256 {
		t.Errorf("缺省准入上限 = %d/%d，want 64/256", c.Server.MaxInflightRequests, c.Server.MaxInflightBytesMB)
	}
	if c.IngressWaitDur != 5*time.Second {
		t.Errorf("缺省 ingress_wait=%v want 5s", c.IngressWaitDur)
	}

	// 显式值：0 合法（关闭该项），"0" wait 合法（满载立即拒绝）。
	fp2 := filepath.Join(dir, "c2.json")
	os.WriteFile(fp2, []byte(`{"server":{"max_inflight_requests":0,"max_inflight_bytes_mb":0,"ingress_wait":"0"}}`), 0o600)
	c2, err := Load(fp2)
	if err != nil {
		t.Fatalf("0 是合法值，不应报错: %v", err)
	}
	if c2.Server.MaxInflightRequests != 0 || c2.Server.MaxInflightBytesMB != 0 || c2.IngressWaitDur != 0 {
		t.Errorf("显式 0 未被保留: %+v", c2.Server)
	}

	// 负值 fail fast；非法时长 fail fast。
	for _, body := range []string{
		`{"server":{"max_inflight_requests":-1}}`,
		`{"server":{"max_inflight_bytes_mb":-1}}`,
		`{"server":{"ingress_wait":"-1s"}}`,
		`{"server":{"ingress_wait":"abc"}}`,
	} {
		fp3 := filepath.Join(dir, "c3.json")
		os.WriteFile(fp3, []byte(body), 0o600)
		if _, err := Load(fp3); err == nil {
			t.Errorf("%s 应报错", body)
		}
	}
}

// TestConcurrentWarnings 守护告警去重表的并发安全：面板 GET/POST 配置会并发调用
// LogWarnings，普通 map 无锁读写会直接触发 concurrent map writes。
func TestConcurrentWarnings(t *testing.T) {
	printedWarningsMu.Lock()
	printedWarnings = map[string]bool{}
	printedWarningsMu.Unlock()

	cfg := &Config{Warnings: []string{"concurrent-warning-regression"}}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				LogWarnings(cfg)
			}
		}()
	}
	close(start)
	wg.Wait()
}

// ---- inject 包装合并 ----

// loadPromptConfig 写临时配置并 Load（prompt 测试共用小工具）。
func loadPromptConfig(t *testing.T, promptJSON string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{"prompt":` + promptJSON + `}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(fp)
}

// hasPromptWarning 报告告警列表里是否有包含 sub 的项。
func hasPromptWarning(c *Config, sub string) bool {
	for _, w := range c.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TestPromptInjectAllBuiltinPresets inject 与任一内置预设都可组合，且素材逐字进规则。
func TestPromptInjectAllBuiltinPresets(t *testing.T) {
	for _, name := range prompt.PresetNames() {
		c, err := loadPromptConfig(t, `{"mode":"inject","preset":"`+name+`"}`)
		if err != nil {
			t.Fatalf("preset=%s: %v", name, err)
		}
		for _, realm := range []string{"cn", "global"} {
			r := c.PromptRules[realm]
			if r.Mode != prompt.ModeInject {
				t.Errorf("preset=%s realm=%s: mode=%q", name, realm, r.Mode)
			}
			// 素材逐字进规则（不做任何改写）。
			if want := prompt.Builtin(name, realm); r.Text != want {
				t.Errorf("preset=%s realm=%s: rule text must be verbatim (%d vs %d bytes)",
					name, realm, len(r.Text), len(want))
			}
		}
	}
}

// TestPromptMaterialsVerbatim 三种素材（预设/文件/内联）在 replace/append/after/inject
// 下都逐字进规则正文，配置层不做任何拼接或剔除。
func TestPromptMaterialsVerbatim(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "p.md")
	if err := os.WriteFile(fp, []byte("FROM-FILE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"replace", "append", "after", "inject"} {
		// 预设
		c, err := loadPromptConfig(t, `{"mode":"`+mode+`","preset":"official-quick"}`)
		if err != nil {
			t.Fatalf("mode=%s preset: %v", mode, err)
		}
		want := prompt.Builtin("official-quick", "cn")
		if mode != prompt.ModeInject {
			want = prompt.StripSlot(want)
		}
		if got := c.PromptRules["cn"].Text; got != want {
			t.Errorf("mode=%s: preset text not verbatim", mode)
		}
		// 内联（优先于文件与预设）
		c, err = loadPromptConfig(t, `{"mode":"`+mode+`","text":"INLINE-1\nINLINE-2\n"}`)
		if err != nil {
			t.Fatalf("mode=%s text: %v", mode, err)
		}
		if got := c.PromptRules["cn"].Text; got != "INLINE-1\nINLINE-2\n" {
			t.Errorf("mode=%s: inline text=%q", mode, got)
		}
		// 文件
		c, err = loadPromptConfig(t, `{"mode":"`+mode+`","file":"`+fp+`"}`)
		if err != nil {
			t.Fatalf("mode=%s file: %v", mode, err)
		}
		if got := c.PromptRules["cn"].Text; got != "FROM-FILE\n" {
			t.Errorf("mode=%s: file text=%q", mode, got)
		}
	}
}

// TestPromptInjectProfileOverrides 分域覆盖：cn 用 inject、其余 append——
// 组合位置按各域自己的 mode 与素材逐域判定。
func TestPromptInjectProfileOverrides(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "c.json")
	body := `{"prompt":{"mode":"append","preset":"official-quick",
		"profiles":{"cn":{"mode":"inject"}}}}`
	if err := os.WriteFile(cf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptRules["cn"].Mode != prompt.ModeInject {
		t.Errorf("cn mode=%q", c.PromptRules["cn"].Mode)
	}
	if c.PromptRules["global"].Mode != prompt.ModeAppend {
		t.Errorf("global mode=%q", c.PromptRules["global"].Mode)
	}
	// 两域素材各自逐字（cn 用顶层素材，global 同）。
	if got, want := c.PromptRules["cn"].Text, prompt.Builtin("official-quick", "cn"); got != want {
		t.Error("cn text not verbatim")
	}
	wantGlobal := prompt.Builtin("official-quick", "global")
	if c.PromptRules["global"].Mode != prompt.ModeInject {
		wantGlobal = prompt.StripSlot(wantGlobal)
	}
	if got := c.PromptRules["global"].Text; got != wantGlobal {
		t.Error("global text not verbatim")
	}
}

// TestPromptPreviewVerbatim 预览与实际出站同口径：预览正文就是规则正文。
func TestPromptPreviewVerbatim(t *testing.T) {
	raw := []byte(`{"prompt":{"mode":"replace","preset":"official-quick"}}`)
	pv, err := PreviewPromptConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := prompt.StripSlot(prompt.Builtin("official-quick", "cn"))
	if pv.CN.Chars != len([]rune(want)) {
		t.Errorf("preview cn chars=%d want %d", pv.CN.Chars, len([]rune(want)))
	}
	if !pv.CN.Inherited || !pv.Global.Inherited {
		t.Error("no per-realm override: both realms must inherit top-level")
	}
	// 预览正文 = 规则正文前缀（未超限时全等）。
	if pv.CN.Truncated {
		if !strings.HasPrefix(want, pv.CN.Text) {
			t.Error("truncated preview must be a prefix of the preset text")
		}
	} else if pv.CN.Text != want {
		t.Error("preview text must equal the preset text")
	}

	// 分域覆盖：本域不再"继承顶层"。
	pv, err = PreviewPromptConfig([]byte(`{"prompt":{"mode":"replace","preset":"default","profiles":{"global":{"preset":"official-quick"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if pv.Global.Inherited {
		t.Error("global with preset override must not be marked inherited")
	}
	if !pv.CN.Inherited {
		t.Error("cn must inherit top-level")
	}
	if pv.Global.Preset != "official-quick" {
		t.Errorf("global preview preset=%q", pv.Global.Preset)
	}
	if pv.CN.Preset != "default" {
		t.Errorf("cn preview preset=%q", pv.CN.Preset)
	}
}

// TestFingerprintConfig 验证指纹配置的归一化与校验：禁用黑名单、替换覆盖与未知 ID 告警
func TestFingerprintConfig(t *testing.T) {
	raw := []byte(`{
		"fingerprint_rewrite": true,
		"fingerprint_disabled": ["err-11128", "unknown-rule-id", "hdr-billing", "hdr-billing-bare"],
		"fingerprint_overrides": {
			"identity-claude-code": "custom replacement",
			"bogus-override": "text",
			"pr-branch": "   ",
			"hdr-billing": "cannot-override-delete-rule",
			"hdr-billing-bare": "custom-hdr"
		},
		"fingerprint_rules": [
			{"match": "FOO", "replace": "BAR"}
		]
	}`)
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	// 1. 未知 ID 应被从 disabled 剔除，留下有效 ID
	if len(cfg.FingerprintDisabled) != 3 || cfg.FingerprintDisabled[0] != "err-11128" || cfg.FingerprintDisabled[1] != "hdr-billing" || cfg.FingerprintDisabled[2] != "hdr-billing-bare" {
		t.Errorf("FingerprintDisabled 归一化结果不对: %v", cfg.FingerprintDisabled)
	}

	// 2. 未知 ID、空串、不可覆盖的删除模式均应剔除；被停用规则的覆盖保留（供重启用后生效）
	if len(cfg.FingerprintOverrides) != 2 ||
		cfg.FingerprintOverrides["identity-claude-code"] != "custom replacement" ||
		cfg.FingerprintOverrides["hdr-billing-bare"] != "custom-hdr" {
		t.Errorf("FingerprintOverrides 归一化结果不对: %v", cfg.FingerprintOverrides)
	}

	// 3. 告警应包含 unknown-rule-id 与 err-11128 提示
	warns := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(warns, "unknown-rule-id") {
		t.Errorf("缺少 unknown-rule-id 告警: %s", warns)
	}
	if !strings.Contains(warns, "bogus-override") {
		t.Errorf("缺少 bogus-override 告警: %s", warns)
	}
	if !strings.Contains(warns, "err-11128 已被禁用") {
		t.Errorf("缺少 err-11128 禁用风险告警: %s", warns)
	}
	if !strings.Contains(warns, "整段删除模式") {
		t.Errorf("缺少不可覆盖模式告警: %s", warns)
	}
	if !strings.Contains(warns, "同时被 fingerprint_disabled 停用") {
		t.Errorf("缺少停用+覆盖重叠告警: %s", warns)
	}

	// 4. 超长替换文本应直接拒绝
	bad := []byte(`{
		"fingerprint_overrides": {
			"identity-claude-code": "` + strings.Repeat("A", 513) + `"
		}
	}`)
	if _, err := ParseConfig(bad); err == nil {
		t.Fatal("超长替换文本应报错")
	}
}
