// load.go 从文件/JSON 加载配置、环境变量覆盖、首次运行生成推荐配置。
//
// 来源分层（优先级由低到高）：Default → File → Env(WB2A_*)。
// -config 只决定读哪个文件，不参与字段覆盖。
//
// 读取路径（Load）：读文件 → 反序列化到 Default()（**只认已知键**）→ env 覆盖 → normalize。
//
// 口径（刻意从简）：
//   - 不做版本迁移、不归一化旧形态、不在读时回写磁盘；文件里不认识的键直接
//     忽略（只告警），下次保存时随全量覆盖自然消失；
//   - 唯一的版本守卫：文件声明的 config_version 比本程序新 → 拒绝启动（避免旧
//     程序读新配置后把不认识的键覆盖掉）；更旧的版本不做任何转换。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"workbuddy_manager/internal/jsondoc"
	"workbuddy_manager/internal/upstream"
)

func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// 目录检查：Docker bind mount 在宿主机文件缺失时会静默创建同名目录，
		// 直接 ReadFile 会报 "Incorrect function" 之类晦涩错误，这里给出可操作提示。
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请先 `cp config.example.json config.json` 或删除该目录（程序会自动生成配置）", path)
		}
		// 只读：读盘不改盘，因此不需要配置锁（写入走 WriteFileAtomic 的原子替换）。
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, fmt.Errorf("read config: %w", rerr)
		}
		obj, oerr := jsondoc.Object(raw)
		if oerr != nil {
			return nil, fmt.Errorf("parse config: %w", oerr)
		}
		if _, err := parseObject(obj, c); err != nil {
			return nil, err
		}
		// 首次成功加载顺手种一份兜底备份（已存在则不动）：把可用回滚点
		// 提前到"用户第一次改配置"之前。
		seedBackupOnce(path)
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// seedBackupOnce 每个配置文件只尝试种一次 .bak（面板保存路径也会用到它）。
var seedBackupOnce = func() func(string) {
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(path string) {
		mu.Lock()
		defer mu.Unlock()
		if seen[path] {
			return
		}
		seen[path] = true
		if err := SeedBackup(path); err != nil {
			LogWarnings(&Config{Warnings: []string{"备份 .bak 写入失败（不影响运行）: " + err.Error()}})
		}
	}
}()

// parseObject 把已解析的配置对象覆盖到 c 上（不做 env、不读文件、不迁移）。
func parseObject(obj map[string]any, c *Config) (*Config, error) {
	if err := checkVersionGate(obj); err != nil {
		return nil, err
	}
	if err := decodeInto(obj, c); err != nil {
		return nil, err
	}
	// 运行期元数据不是配置输入：文件里若残留 _warnings，不读进来，否则会污染当期告警。
	c.Warnings = nil
	c.ConfigVersion = CurrentVersion
	// 未知键（含旧键/拼错的键）只告警不阻断：读取时直接忽略，保存时随全量
	// 覆盖消失。旧键不再有任何兼容处理——它们的值不参与配置语义。
	for _, k := range unknownConfigKeys(obj) {
		c.addWarning(k + " —— 未知配置项，已忽略（检查拼写）")
	}
	for realm, p := range c.Upstream.Profiles {
		if realm != "cn" && realm != "global" {
			c.addWarning("upstream.profiles." + realm + " —— 未知 realm，已忽略")
			delete(c.Upstream.Profiles, realm)
			continue
		}
		if err := upstream.ValidateIdentity(p); err != nil {
			return nil, err
		}
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// checkVersionGate 唯一的版本守卫：声明得比本程序新的配置文件一律拒绝。
//
// 不做迁移、不做降级：旧版本号（或没有版本号）照当前结构读，认识的键生效、
// 不认识的键忽略并在下次保存时被覆盖。但反过来（旧程序读新配置）会静默丢掉
// 新版字段，所以那一种情况必须 fail fast 并告诉用户换新版程序。
func checkVersionGate(obj map[string]any) error {
	v, ok := obj["config_version"]
	if !ok || v == nil {
		return nil
	}
	var declared int
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return fmt.Errorf("config_version: %q 不是整数版本号", n.String())
		}
		declared = int(i)
	case float64:
		if n != float64(int(n)) {
			return fmt.Errorf("config_version: %v 不是整数版本号", n)
		}
		declared = int(n)
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return fmt.Errorf("config_version: %q 不是整数版本号", n)
		}
		declared = i
	default:
		return fmt.Errorf("config_version: 不认识的取值类型 %T（应为整数）", v)
	}
	if declared > CurrentVersion {
		return fmt.Errorf("config_version: %d 由更新版本的程序写入（本程序支持到 %d）：请用新版程序启动", declared, CurrentVersion)
	}
	return nil
}

// addWarning 追加去重后的配置告警（normalize 会被重复调用，同一告警不重复展示）。
func (c *Config) addWarning(w string) {
	if w == "" {
		return
	}
	for _, existing := range c.Warnings {
		if existing == w {
			return
		}
	}
	c.Warnings = append(c.Warnings, w)
}

// decodeInto 把配置对象解码到 c 上（就地覆盖 c 里出现的键）。
//
// 不做版本转换：版本号已由 checkVersionGate 单独把过关（只拒绝比程序新的）。
func decodeInto(obj map[string]any, c *Config) error {
	// 直接解到传入的 c 上：JSON 里没出现的键保持 c 的现有取值（保存路径正是靠
	// 这一点做部分更新——提交什么改什么，其余键不动）。
	// 不认识的键被 encoding/json 静默忽略（= 只读取可用键值）。
	b, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	// 默认档覆盖表是整表替换，不使用 encoding/json 对非 nil map 的逐键合并。
	// 放在配置解析层，文件加载、脚本提交和面板保存共用同一语义；未提交则保留。
	if up, ok := obj["upstream"].(map[string]any); ok {
		if _, submitted := up["effort_default_overrides"]; submitted {
			c.Upstream.EffortDefaultOverrides = nil
		}
	}
	if err := json.Unmarshal(b, c); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}

// ParseConfig 基于默认值解析一段配置 JSON（不读文件、不读环境变量）。
//
// 只拒绝“声明得比本程序新”的版本（见 checkVersionGate）；旧版本与无版本号的
// JSON 一律按当前结构读，认识的键生效、其余忽略——没有迁移。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不读文件、不读 env）。
//
// c 里已设置的值不会被未提交的键重置：这是保存路径“部分更新”的基础。
// upstream.effort_default_overrides 显式提交时整表替换（{} / null 均清空）；
// 其余 map 字段仍按子键合并。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	obj, err := jsondoc.Object(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return parseObject(obj, c)
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := MarshalConfig(c)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}

	if v := os.Getenv("WB2A_FIRST_MODEL_EVENT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Upstream.FirstModelEventSeconds = n
		}
	}
	if v := os.Getenv("WB2A_FIRST_GENERATION_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Upstream.FirstGenerationSeconds = n
		}
	}
	if v := os.Getenv("WB2A_TAIL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Upstream.TailSeconds = n
		}
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}

	if v := os.Getenv("WB2A_FINGERPRINT_REWRITE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.FingerprintRewrite = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_PREFER_EXPIRING"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Pool.PreferExpiring = b
		}
	}
	if v := os.Getenv("WB2A_PROXY_URL"); v != "" {
		c.ProxyURL = v
	}
	if v := os.Getenv("WB2A_RESIN_URL"); v != "" {
		c.ResinURL = v
	}
	if v := os.Getenv("WB2A_RESIN_PLATFORM_NAME"); v != "" {
		c.ResinPlatformName = v
	}
	if v := os.Getenv("WB2A_RESIN_MODE"); v != "" {
		c.ResinMode = v
	}
	if v := os.Getenv("WB2A_RESIN_AUTH_VERSION"); v != "" {
		c.ResinAuthVersion = v
	}
	if v := os.Getenv("WB2A_MODEL_DEFAULT_REALM"); v != "" {
		c.ModelDefaultRealm = v
	}
}
