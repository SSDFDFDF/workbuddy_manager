// save_config_test.go 面板保存路径的端到端测试（校验 → 合并 → 落盘 → 热应用）。
//
// 这些用例需要真实的 pool/scheduler/upstream 实例与 config/runtime 快照，因此留在
// cmd/server；纯配置解析/校验的用例见 internal/config/config_test.go。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/config"
	"workbuddy_manager/internal/config/runtime"
	"workbuddy_manager/internal/media"
	"workbuddy_manager/internal/panel"
	"workbuddy_manager/internal/pool"
	"workbuddy_manager/internal/prompt"
	"workbuddy_manager/internal/reqlog"
	"workbuddy_manager/internal/scheduler"
	"workbuddy_manager/internal/upstream"
)

// testHotTargets 保存路径的最小热应用目标集：零值 upstream.Client / 空归档配置
// 必须不 panic（真实装配由 main 构造，这里覆盖测试路径的健壮性）。
func testHotTargets() *hotTargets {
	return &hotTargets{
		live:       runtime.New(runtime.Snapshot{}),
		pool:       pool.New(""),
		upstream:   &upstream.Client{},
		schedule:   scheduler.New(scheduler.Config{}),
		requestLog: reqlog.New(reqlog.Config{}),
		prompt:     prompt.NewHolder(nil),
	}
}

// TestSaveConfigOverwritesUnknownKeys 保存是**全量覆盖当前 schema**：
// 磁盘上的未知/历史键随这次覆盖消失，提交里塞的未知键也不会落盘。
// （这是刻意的：未知键在读取时就已不参与语义，留着只会让文件带着死键越滚越大。）
func TestSaveConfigOverwritesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacy := `{"listen":":1","my_note":"drop me","features":{"sanitize":true}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	// 提交里也带一个未知键（模拟旧前端/手写脚本）。
	payload := []byte(`{"listen":":2","another_note":"also dropped"}`)
	if _, err := saveConfig(payload, path, testHotTargets()); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"my_note", "features", "another_note"} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("未知键 %s 不该出现在落盘内容里:\n%s", gone, raw)
		}
	}
	c, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatalf("落盘内容必须可解析: %v", err)
	}
	if c.Listen != ":2" {
		t.Errorf("listen=%q want :2", c.Listen)
	}
}

// TestSaveConfigPartialPayloadKeepsOtherKeys 部分提交只改它提到的键：
// 没出现的键保持磁盘现值（不会被重置成默认值），写出去的是完整当前结构。
func TestSaveConfigPartialPayloadKeepsOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 磁盘上已有非默认值（sleep 两个键都是「面板表单里没有」或「本次没提交」的）。
	start := `{
		"listen": ":1",
		"state_file": "./data/custom-state.json",
		"logging": {"request_archive_enabled": false, "request_retention_days": 30},
		"upstream": {"tail_seconds": 42},
		"pool": {"max_in_flight": 7}
	}`
	if err := os.WriteFile(path, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	// 只提交一个键。
	if _, err := saveConfig([]byte(`{"pool":{"max_in_flight":9}}`), path, testHotTargets()); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.MaxInFlight != 9 {
		t.Errorf("提交的键没生效: max_in_flight=%d", c.Pool.MaxInFlight)
	}
	// 以下都是「本次没提交」，必须原样保留（若退化成整体重置会变回默认值）。
	if c.StateFile != "./data/custom-state.json" {
		t.Errorf("state_file=%q 被重置了（部分提交不得影响未提交的键）", c.StateFile)
	}
	if c.Logging.RequestArchiveEnabled || c.Logging.RequestRetentionDays != 30 {
		t.Errorf("logging 段被重置: %+v", c.Logging)
	}
	if c.Upstream.TailSeconds != 42 {
		t.Errorf("upstream.tail_seconds=%d 被重置（该键不在面板表单里）", c.Upstream.TailSeconds)
	}
	// 写出去的是完整结构：不在提交里、也不在磁盘原文里显式出现的键，会以
	// 当前 schema 的形态出现（例如 server.read_timeout 的默认值）。
	if !strings.Contains(string(raw), "read_timeout") {
		t.Errorf("落盘内容应是完整当前结构（缺 read_timeout）:\n%s", raw)
	}
}

// TestSaveConfigPrunesEmptyPromptProfiles 分域提示词覆盖全空 = 继承顶层：
// 落盘时不留空壳对象（面板把「继承顶层」表达为三项全空）。
func TestSaveConfigPrunesEmptyPromptProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	start := `{"listen":":1","prompt":{"mode":"inject","profiles":{"cn":{"preset":"official-craft"}}}}`
	if err := os.WriteFile(path, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveConfig([]byte(`{"prompt":{"profiles":{"cn":{"mode":"","preset":"","text":""}}}}`), path, testHotTargets()); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Prompt.Profiles["cn"]; ok {
		t.Errorf("全空的分域覆盖应被剪掉，实际 %+v\n%s", c.Prompt.Profiles, raw)
	}
	// prompt 段里不该留下任何 profiles 键（upstream.profiles 是另一回事，始终存在）。
	var doc struct {
		Prompt map[string]any `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Prompt["profiles"]; ok {
		t.Errorf("落盘内容的 prompt 段不该出现空 profiles 壳:\n%s", raw)
	}
}

// TestSaveConfigRejectsLegacyPromptMode 旧 mode 取值（v2 时代的 custom/passthrough）
// 没有迁移：保存时直接报错并把合法取值列出来，不静默降级。
func TestSaveConfigRejectsLegacyPromptMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	_, err := saveConfig([]byte(`{"prompt":{"mode":"custom"}}`), path, testHotTargets())
	if err == nil || !strings.Contains(err.Error(), "prompt.mode") {
		t.Fatalf("旧 mode 取值必须报错，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "replace") {
		t.Errorf("报错信息应列出合法取值，实际: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("被拒绝的保存不得改写配置文件:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestSaveConfigMediaPolicyRoundTrip 面板保存 + 热生效往返（media 段）。
//
// 后端走「叠加到磁盘现值 → ParseConfig → 整份覆盖落盘 → 热应用」。这里用与前端 collectConfig
// 相同形态的 payload（bool + number）钉住 media 段：
//   - 三个键都不是未知键（不被剪掉）；
//   - 数值档位落到 int 字段（不是字符串）；
//   - 热应用后进程级策略真的生效。
func TestSaveConfigMediaPolicyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1","media":{"tool_images":"auto"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prevTool := media.CurrentToolPolicy()
	prevImage := media.CurrentImagePolicy()
	t.Cleanup(func() {
		_ = media.SetToolPolicy(prevTool)
		_ = media.SetImagePolicy(prevImage)
	})

	payload := []byte(`{"listen":":1","media":{"tool_images":"hoist","image_transcode":true,"image_max_dimension":1080}}`)
	if _, err := saveConfig(payload, path, testHotTargets()); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Media.ToolImages != "hoist" || !parsed.Media.ImageTranscode || parsed.Media.ImageMaxDimension != 1080 {
		t.Fatalf("media policy not persisted: %+v", parsed.Media)
	}
	if got := media.CurrentToolPolicy(); got != media.ToolPolicyHoist {
		t.Fatalf("tool policy not hot-applied: %q", got)
	}
	if got := media.CurrentImagePolicy(); !got.Transcode || got.MaxDimension != 1080 {
		t.Fatalf("image policy not hot-applied: %+v", got)
	}
}

// TestSaveConfigRejectsInvalidMediaPolicy 非法档位保存时报错且不改动热状态。
func TestSaveConfigRejectsInvalidMediaPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	prevImage := media.CurrentImagePolicy()
	t.Cleanup(func() { _ = media.SetImagePolicy(prevImage) })
	if err := media.SetImagePolicy(media.ImagePolicy{MaxDimension: 2000}); err != nil {
		t.Fatal(err)
	}
	_, err := saveConfig([]byte(`{"listen":":1","media":{"image_max_dimension":4096}}`), path, testHotTargets())
	if err == nil || !strings.Contains(err.Error(), "media.image_max_dimension") {
		t.Fatalf("invalid dimension accepted: %v", err)
	}
	if got := media.CurrentImagePolicy(); got.MaxDimension != 2000 {
		t.Fatalf("rejected save must not change the live policy: %+v", got)
	}
}

// TestSaveConfigIngressRoundTrip 面板保存路径的类型往返（server.* 入站准入四项）：
// 面板提交的是 JSON（number → int，时长 → 字符串），后端叠加到磁盘现值后校验落盘。
// 钉住三件事：
//   - 四项都落进当前 schema（不被丢弃）；
//   - 数值落到 int 字段（不是字符串），且 **0 是合法取值**（"不限制"）——前端把
//     0 与空串区分开正是为此（见 internal/panel/frontend_test.go 的 CollectIngressPolicy）；
//   - 时长字面量（含裸 "0"）被解析成对应 duration，而不是被当成缺省值回落。
func TestSaveConfigIngressRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 与前端 collectConfig 相同形态：两个上限是 Number，两个时长是字符串。
	payload := []byte(`{"listen":":1","server":{"max_inflight_requests":0,"max_inflight_bytes_mb":64,"ingress_wait":"0","read_timeout":"600s"}}`)
	restart, err := saveConfig(payload, path, testHotTargets())
	if err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "max_inflight_unset") {
		t.Fatal("未知键混入落盘内容")
	}
	parsed, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 0 必须原样保留（显式关掉并发上限），不能回落成默认 64。
	if parsed.Server.MaxInflightRequests != 0 {
		t.Errorf("max_inflight_requests=%d want 0（显式 0 = 不限制，不得回落默认）", parsed.Server.MaxInflightRequests)
	}
	if parsed.Server.MaxInflightBytesMB != 64 {
		t.Errorf("max_inflight_bytes_mb=%d want 64", parsed.Server.MaxInflightBytesMB)
	}
	// 裸 "0" 解析为 0 时长（= 满载立即拒绝），不是缺省 5s。
	if parsed.Server.IngressWait != "0" || parsed.IngressWaitDur != 0 {
		t.Errorf("ingress_wait=%q dur=%v want \"0\"/0（裸 0 = 立即拒绝，非缺省 5s）",
			parsed.Server.IngressWait, parsed.IngressWaitDur)
	}
	if parsed.ServerReadTimeoutDur != 600*time.Second {
		t.Errorf("read_timeout dur=%v want 600s", parsed.ServerReadTimeoutDur)
	}
	// 入站准入三项已热化：不得再出现在 restart_required（改完即时生效）；
	// read_timeout 仍属重启项，必须提示。
	got := map[string]bool{}
	for _, f := range restart {
		got[f] = true
	}
	for _, f := range []string{"server.max_inflight_requests", "server.max_inflight_bytes_mb", "server.ingress_wait"} {
		if got[f] {
			t.Errorf("%s 已热化，不该提示需重启（实际 %v）", f, restart)
		}
	}
	if !got["server.read_timeout"] {
		t.Errorf("restart_required 缺 server.read_timeout（实际 %v）", restart)
	}
}

// TestSaveConfigWritesCurrentVersion 保存路径写出的文件永远是当前版本号 +
// 当前 schema，不认识的键（历史遗留段、退役键）随全量覆盖消失。
func TestSaveConfigWritesCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 磁盘上是带历史遗留键的旧文件（无版本号）。
	legacy := `{"cooldown":{"hard_credit":"1h","soft_rate":"30s"},"my_note":"keep","prompt":{"mode":"replace"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"pool":{"max_in_flight":9}}`)
	if _, err := saveConfig(payload, path, testHotTargets()); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatalf("落盘文件必须能被当前版本解析: %v\n%s", err, raw)
	}
	if disk.ConfigVersion != config.CurrentVersion {
		t.Errorf("config_version=%d want %d", disk.ConfigVersion, config.CurrentVersion)
	}
	for _, gone := range []string{"hard_credit", "my_note"} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("历史/未知键 %s 仍在落盘内容里:\n%s", gone, raw)
		}
	}
	// 已知的旧文件取值原样保留（没有迁移，也没有被重置）。
	if disk.Prompt.Mode != "replace" {
		t.Errorf("prompt.mode=%q want replace（已知取值应保持不变）", disk.Prompt.Mode)
	}
	if disk.Pool.MaxInFlight != 9 {
		t.Errorf("提交的值没生效: max_in_flight=%d", disk.Pool.MaxInFlight)
	}
}

// TestSaveConfigDropsRuntimeMetaAndHandlesCorruptOld 两个边界：
//   - 运行期元数据（_warnings）不得被保存路径持久化（否则每次启动都把上一轮的
//     告警写回文件，越滚越多）；
//   - 旧文件是坏 JSON 时保存仍可成功（从 Default 基准覆盖），且重启清单退回保守全量。
func TestSaveConfigDropsRuntimeMetaAndHandlesCorruptOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 磁盘上是带运行期元数据的正常文件。
	if err := os.WriteFile(path, []byte(`{"listen":":1","_warnings":["stale"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveConfig([]byte(`{"pool":{"max_in_flight":4}}`), path, testHotTargets()); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stale") {
		t.Errorf("运行期元数据被持久化了:\n%s", raw)
	}

	// 旧文件坏 JSON：保存仍应成功，且重启清单为保守全量（listen 等装配期字段在内）。
	if err := os.WriteFile(path, []byte(`{ this is not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	restart, err := saveConfig([]byte(`{"pool":{"max_in_flight":5}}`), path, testHotTargets())
	if err != nil {
		t.Fatalf("坏旧文件不该阻断保存: %v", err)
	}
	if len(restart) < 3 {
		t.Errorf("旧文件不可解析时重启清单应退回保守全量（非空 Restart 叶子），实际 %v", restart)
	}
	got := map[string]bool{}
	for _, f := range restart {
		got[f] = true
	}
	// 注意：upstash.* 空值在“零值旧配置”下不构成差异，故这里只要求固定的三项。
	for _, want := range []string{"listen", "state_file", "server.read_timeout"} {
		if !got[want] {
			t.Errorf("保守全量清单缺 %s（实际 %v）", want, restart)
		}
	}
	// 落盘内容必须是完整可解析的当前版本配置。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseConfig(after)
	if err != nil {
		t.Fatalf("落盘内容必须可解析: %v\n%s", err, after)
	}
	if c.ConfigVersion != config.CurrentVersion || c.Pool.MaxInFlight != 5 {
		t.Errorf("落盘内容不对: version=%d max_in_flight=%d", c.ConfigVersion, c.Pool.MaxInFlight)
	}
}

// TestSaveConfigHotReloadAuthDir auth_dir 热重载（P3c）：
//   - 改目录后账号池立即对齐到新目录（旧目录账号移除、新目录账号加入）；
//   - 面板落盘目录同步切换（登录/导入写新目录）；
//   - 目标路径已存在且是文件 → 保存失败且配置文件不被改写（预检在落盘前）。
func TestSaveConfigHotReloadAuthDir(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeAuth := func(dir, name, uid string) {
		t.Helper()
		raw := `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"` + uid + `"}}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAuth(dir1, "workbuddy-a.json", "uid-a")
	writeAuth(dir2, "workbuddy-b.json", "uid-b")

	path := filepath.Join(dir1, "config.json")
	initial := []byte(`{"listen":":1","auth_dir":` + strconv.Quote(dir1) + `}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	hot := testHotTargets()
	hot.pool.SyncToDir(mustLoadAuths(t, dir1))

	pn := panel.New(panel.Config{AuthDir: dir1})
	hot.panel = pn

	payload := []byte(`{"listen":":1","auth_dir":` + strconv.Quote(dir2) + `}`)
	if _, err := saveConfig(payload, path, hot); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	total, _, _, _, _ := hot.pool.CountsDetailed()
	if total != 1 {
		t.Fatalf("账号池应只保留新目录的 1 个账号，实际 total=%d", total)
	}

	// 路径是文件 → 拒绝且不改写磁盘配置。
	badPath := filepath.Join(dir1, "not-a-dir")
	if err := os.WriteFile(badPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := []byte(`{"listen":":1","auth_dir":` + strconv.Quote(badPath) + `}`)
	if _, err := saveConfig(bad, path, hot); err == nil {
		t.Fatal("auth_dir 指向文件应保存失败")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AuthDir != dir2 {
		t.Fatalf("失败的保存不得改写 auth_dir，实际 %q", parsed.AuthDir)
	}
}

func mustLoadAuths(t *testing.T, dir string) []*auth.Auth {
	t.Helper()
	list, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// TestSaveConfigHotReloadProxy proxy_url / resin_* 热重载：保存后上游出站与
// 面板登录链路都换成新代理实例；再清空配置恢复直连（nil）。
func TestSaveConfigHotReloadProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hot := testHotTargets()
	hot.upstream = upstream.New()
	pn := panel.New(panel.Config{})
	hot.panel = pn

	if _, err := saveConfig([]byte(`{"listen":":1","proxy_url":"http://127.0.0.1:8080"}`), path, hot); err != nil {
		t.Fatalf("save proxy: %v", err)
	}
	pc := hot.upstream.Proxy()
	if pc == nil || !pc.Enabled() {
		t.Fatal("保存 proxy_url 后上游应接入代理")
	}
	if pc.Mode() != "plain" {
		t.Fatalf("普通代理 mode=%q", pc.Mode())
	}

	if _, err := saveConfig([]byte(`{"listen":":1","proxy_url":""}`), path, hot); err != nil {
		t.Fatalf("clear proxy: %v", err)
	}
	if got := hot.upstream.Proxy(); got != nil {
		t.Fatalf("清空 proxy_url 后应恢复直连，实际 %+v", got)
	}
}

// TestSaveConfigFingerprintRoundTrip 指纹配置的后端回环：保存 → 归一化落盘 → 热应用。
//
// 钉住面板「管理内置指纹表」的完整保存链路：禁用黑名单 / 替换覆盖 / 自定义规则
// 都真的进入进程级改写层（而不只是写进 config.json）。
func TestSaveConfigFingerprintRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1","fingerprint_rewrite":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 与前端 collectConfig 同形态的 payload（含一个未知 ID，必须被剔除并告警）。
	payload := []byte(`{
		"listen":":1",
		"fingerprint_rewrite": true,
		"fingerprint_disabled": ["err-11128", "no-such-rule"],
		"fingerprint_overrides": {"identity-claude-code": "You are Claude Code, masked"},
		"fingerprint_rules": [{"match":"MY-TOKEN","replace":"X"}]
	}`)
	ht := testHotTargets()
	if _, err := saveConfig(payload, path, ht); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	// 落盘即归一化：未知 ID 不进文件，有效项保留。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.FingerprintDisabled) != 1 || parsed.FingerprintDisabled[0] != "err-11128" {
		t.Fatalf("fingerprint_disabled 未归一化落盘: %v", parsed.FingerprintDisabled)
	}
	if parsed.FingerprintOverrides["identity-claude-code"] != "You are Claude Code, masked" {
		t.Fatalf("fingerprint_overrides 未落盘: %v", parsed.FingerprintOverrides)
	}

	// 热生效：层已替换，且三条配置语义都在层里生效。
	l := ht.upstream.Fingerprints.Load()
	if !l.Enabled() {
		t.Fatal("指纹层未热应用")
	}
	obj := map[string]any{"messages": []any{
		map[string]any{"content": "MY-TOKEN and code=11128"},
		map[string]any{"content": "You are Claude Code, Anthropic's official CLI for Claude."},
	}}
	if !l.Object(obj) {
		t.Fatal("层未报告改动")
	}
	got, _ := json.Marshal(obj)
	s := string(got)
	if !strings.Contains(s, "X and code=11128") {
		t.Errorf("自定义规则或被禁用规则行为不对（MY-TOKEN→X、11128 应保留）: %s", s)
	}
	if !strings.Contains(s, "You are Claude Code, masked.") {
		t.Errorf("替换覆盖未生效: %s", s)
	}
	if strings.Contains(s, "Anthropic's official CLI for Claude") {
		t.Errorf("内置身份句残留: %s", s)
	}

	// 关闭总开关后再保存：层退回逐字透传（开关与其他字段同一保存链路）。
	if _, err := saveConfig([]byte(`{"listen":":1","fingerprint_rewrite":false}`), path, ht); err != nil {
		t.Fatalf("saveConfig(off): %v", err)
	}
	if ht.upstream.Fingerprints.Load().Enabled() {
		t.Fatal("关闭后层应停用")
	}
}

// TestSaveConfigEffortOverridesRoundTrip 面板「模型档位」页的完整保存链路：
// 落盘归一化 + 热生效（进程级快照）+ **删键/清空真的生效**。
//
// 最后一点是本用例的重点：map 字段在 encoding/json 里是逐键合并语义，
// 若保存路径不先清基准，「删掉一行 / 清空清单」会写回旧值（静默 no-op），
// 而面板显示的是「已保存」——用户会以为改回去了、实际还在生效。
func TestSaveConfigEffortOverridesRoundTrip(t *testing.T) {
	upstream.ResetLookupChainForTest()
	t.Cleanup(upstream.ResetLookupChainForTest)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	start := `{"listen":":1","upstream":{"effort_default_overrides":{"glm-5.3-flash":"low","kimi-k2.6":"medium"}}}`
	if err := os.WriteFile(path, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	ht := testHotTargets()

	// 面板整表下发：改一条、删一条（kimi-k2.6 从提交里消失 = 用户删掉了那一行）。
	// 取值取 max：既能与磁盘现值 low 区分，也能与静态兜底默认档 high 区分。
	payload := []byte(`{"listen":":1","upstream":{"effort_default_overrides":{"GLM-5.3-flash":"max"}}}`)
	if _, err := saveConfig(payload, path, ht); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Upstream.EffortDefaultOverrides; len(got) != 1 || got["glm-5.3-flash"] != "max" {
		t.Fatalf("整表替换未生效（删键/改值应该是替换而不是合并）: %v", got)
	}
	// 热生效：进程级快照已换（无需重启）。
	if _, def := (&upstream.Client{}).EffortSupport("cn", "glm-5.3-flash"); def != "max" {
		t.Fatalf("覆盖未热应用到查找链: %q", def)
	}
	if _, ok := upstream.EffortDefaultOverride("cn", "kimi-k2.6", true); ok {
		t.Fatal("被删掉的条目仍在生效（热应用的是旧表？）")
	}

	// 同次提交归一后重名：明确拒绝，既不落盘也不替换热快照。
	if _, err := saveConfig([]byte(`{"upstream":{"effort_default_overrides":{"glm-5.3-flash":"low"," GLM-5.3-flash ":"high"}}}`), path, ht); err == nil {
		t.Fatal("归一后重名的覆盖表必须拒绝")
	}
	afterRejected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterRejected) != string(raw) {
		t.Fatal("被拒绝的提交改写了配置文件")
	}
	if _, def := (&upstream.Client{}).EffortSupport("cn", "glm-5.3-flash"); def != "max" {
		t.Fatalf("被拒绝的提交改变了热快照: %q", def)
	}

	// 清空整张表（面板清空文本域）→ 落盘不留空壳 map，行为完全沿用上游。
	if _, err := saveConfig([]byte(`{"listen":":1","upstream":{"effort_default_overrides":{}}}`), path, ht); err != nil {
		t.Fatalf("saveConfig(clear): %v", err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "effort_default_overrides") {
		t.Fatalf("清空后不该留下空壳 map:\n%s", raw)
	}
	// 清空后回到上游/静态口径（glm-5.3-flash 的 CN 静态默认档是 high）。
	if _, def := (&upstream.Client{}).EffortSupport("cn", "glm-5.3-flash"); def != "high" {
		t.Fatalf("清空后应回落静态默认档 high，实际 %q", def)
	}

	// 不提交该键 → 磁盘现值原样保留（部分提交语义不受本次改动影响）。
	if err := os.WriteFile(path, []byte(start), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveConfig([]byte(`{"listen":":2"}`), path, ht); err != nil {
		t.Fatalf("saveConfig(untouched): %v", err)
	}
	raw, _ = os.ReadFile(path)
	parsed, err = config.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Upstream.EffortDefaultOverrides; len(got) != 2 {
		t.Fatalf("未提交该键时磁盘现值必须保留: %v", got)
	}
}
