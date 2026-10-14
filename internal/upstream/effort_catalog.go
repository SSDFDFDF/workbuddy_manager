// effort_catalog.go 推理档位（reasoning effort）产品级静态兜底表。
//
// 数据来源三级（吸收参考仓库 reconcileWithFallback/buddy-adapter.ts:499-523 语义）：
//   - 远端 FetchModels / global 探测已解析的 supportedEfforts/defaultEffort 桶（权威，优先）；
//   - 本文件按 realm 分开的产品级静态兜底表（远端缺失时补齐）；
//   - 两者皆无 → 不在 /v1/models 输出 effort 字段（omitted，不是空数组）。
//
// 档位值照抄参考仓库 dsh-codearts：CN/CodeBuddy 面取 src/product.ts CODEBUDDY_FALLBACK_MODELS
// ∪ src/buddy-adapter.ts:126-137 REASONING_EFFORTS；global/WorkBuddy 面取 src/product.ts
// WORKBUDDY_FALLBACK_MODELS。两个 realm 对同一模型给出不同档位（如 deepseek-v4.1-flash：
// CN 三档 ['low','high','max']、global 仅 ['high']），故**按 realm 分表**，绝不混用。
//
// 只列**可枚举**档位的模型；仅有固定默认档（glm-5.1/kimi-* 的 medium）同样入表，
// 但其档位是「可枚举单档」而非「无选择器」，照抄参考仓库如实暴露。
package upstream

import "strings"

// effortCap 一个模型的档位能力（对齐产品兜底表条目 reasoningEfforts + defaultReasoningEffort）。
type effortCap struct {
	efforts       []string
	defaultEffort string
}

// cnEffortFallback CN / CodeBuddy 面静态兜底表。
// 三档模型 defaultEffort 均为 high（product.ts CODEBUDDY_FALLBACK_MODELS 逐条 defaultReasoningEffort）。
var cnEffortFallback = map[string]effortCap{
	"deepseek-v4-flash":   {efforts: []string{"low", "high", "max"}},
	"deepseek-v4.1-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"deepseek-v4-pro":     {efforts: []string{"low", "high", "xhigh"}, defaultEffort: "high"},
	"hy4-preview":         {efforts: []string{"high"}, defaultEffort: "high"},
	"hy4-preview-x":       {efforts: []string{"high"}},
	"hy3":                 {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"hy3-x":               {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"glm-5.3":             {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.3-flash":       {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2":             {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"glm-5.1":             {efforts: []string{"medium"}},
	"glm-5v-turbo":        {efforts: []string{"medium"}},
	"kimi-k3-1":           {efforts: []string{"medium"}},
	"kimi-k2.7":           {efforts: []string{"medium"}},
	"kimi-k2.6":           {efforts: []string{"medium"}},
	"minimax-m3":          {efforts: []string{"medium"}},
}

// globalEffortFallback global / WorkBuddy 国际版面静态兜底表。
// 注意 deepseek-v4.1-flash 在国际版**只有 ['high']**（product.ts:190 实测 IDE 缓存），
// 与 CN 面的三档刻意不同——往 WorkBuddy 上游发 low/max 是非法参数 400。
var globalEffortFallback = map[string]effortCap{
	"fast-model":          {efforts: []string{"medium"}},
	"balanced-model":      {efforts: []string{"medium"}},
	"primary-model":       {efforts: []string{"high"}},
	"hy4-preview-f":       {efforts: []string{"high"}, defaultEffort: "high"},
	"hy3":                 {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"deepseek-v4.1-flash": {efforts: []string{"high"}},
	"gpt-6-astra":         {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-sol":         {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-terra":       {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-luna":        {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.5":             {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.4":             {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.3-codex":       {efforts: []string{"medium"}},
	"gemini-3.5-flash":    {efforts: []string{"medium"}},
	"glm-5.3":             {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2":             {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"kimi-k3":             {efforts: []string{"medium"}},
	"kimi-k2.6":           {efforts: []string{"medium"}},
}

// staticEffortCap 按 realm 取静态兜底条目；未命中返回 zero effortCap（efforts=nil）。
// realm 经 realmKey 归一化（空 → "cn"），与 efforts 缓存桶同口径。
func staticEffortCap(realm, model string) effortCap {
	table := cnEffortFallback
	if realmKey(realm) == "global" {
		table = globalEffortFallback
	}
	return table[model]
}

// EffortListing 计算模型在 /v1/models 应暴露的 effort 能力（四级查找 + 默认档防御）。
//
// remoteEfforts/remoteDefault 为远端（FetchModels / global 探测）已解析值；
// remoteEfforts 非空时以其为权威（不回落到静态表），否则落到产品级静态兜底表；
// 两者皆无 → efforts 返回 nil（调用方省略字段，不输出空数组）。
//
// defaultEffort 仅在「efforts 非空且 default 命中 efforts」时才返回
// （对齐参考仓库 resolveModel 的 `defaultEffort ∈ efforts` 防御：不宣称不支持的默认档）。
// remoteDefault 空串不回落到静态默认档——默认档随档位表同源：remote 有档位就用 remote 默认档，
// 静态兜底档位就用静态默认档，避免跨源拼接出「档位是静态、默认档是 remote」的矛盾组合。
//
// 唯一压过上述同源规则的是本地覆盖（upstream.effort_default_overrides，见
// effort_override.go）：命中时默认档换成本地值，且该值先被钳进本轮认定的支持集——
// 宣告与「可被钳位/注入的档位」仍是同一份答案，客户端照宣告选档永不被判越界。
func EffortListing(realm, model string, remoteEfforts []string, remoteDefault string) (efforts []string, defaultEffort string) {
	var src effortCap
	switch {
	case len(remoteEfforts) > 0:
		src = effortCap{efforts: remoteEfforts, defaultEffort: remoteDefault}
	default:
		src = staticEffortCap(realm, model)
	}
	if len(src.efforts) == 0 {
		return nil, ""
	}
	// 本地默认档覆盖：本地的覆盖上游 defaultEffort 与静态兜底默认档。
	if ov, ok := defaultOverrideForModel(realm, model, src.efforts); ok {
		src.defaultEffort = ov
	}
	efforts = append([]string(nil), src.efforts...)
	if src.defaultEffort != "" && containsEffort(efforts, src.defaultEffort) {
		defaultEffort = src.defaultEffort
	}
	return efforts, defaultEffort
}

// containsEffort 档位成员判定（精确匹配，对齐参考仓库 `efforts.includes(defaultEffort)`）。
func containsEffort(efforts []string, want string) bool {
	for _, e := range efforts {
		if e == want {
			return true
		}
	}
	return false
}

// effortRank 档位从低到高。none/off 是「关闭思考」的开关值而不是档位，排在最低但
// **不参与钳位**：把 off 归一到某个真实档位等于把用户关闭掉的思考又打开。
var effortRank = map[string]int{"none": 0, "off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// isEffortOff 判定关闭思考的开关值（与 forwarding.EncodeObject 的 off/none 口径一致）。
// 不导出：合法档位集与归一都在本包内闭环，外部不需要知道这个判据。
func isEffortOff(effort string) bool {
	return effort == "off" || effort == "none"
}

// ClampEffort 把请求档位归一到模型支持的档位集，返回 (出站值, 是否发生改写)。
//
// 语义（继承 fork 前 payload.go:normalizeReasoningEffort，见 FORK_CHANGES §15）：
//   - 请求档位 ∈ supported（忽略大小写/首尾空白）→ 若书写形态与声明不一致则按**声明
//     形态**回写（上游可能大小写敏感），否则不改写；
//   - 否则取 ≤请求档位的最高支持档（真正降级，偏离最小）；
//   - 支持档全部高于请求档 → 取最低支持档（升档，但比 400 更接近用户意图）；
//   - 请求档位未知 / supported 空 / 关闭思考值 → 不改写（透传，由上游裁决）。
//     supported 里的关闭值同样只被跳过（它不能充当降级目标，否则 "high" 会被改成 "off"）。
//
// 为什么不再直接 400：客户端可发现档位来自本网关自己输出的 reasoning_supported_efforts
// （effort_catalog.go 的静态表 ∪ 探测下发），断言「不可能被拒」；同一档位打原生 Chat
// 入口本来也只会透传。上游对越界档位返回 400 code=11133 会白白消耗一次轮转并污染账号
// 健康度（issue #84：deepseek-v4.1-flash 国际版只认 high）。
func ClampEffort(requested string, supported []string) (string, bool) {
	return clampEffort(requested, supported, "")
}

// ClampEffortWithDefault 与 ClampEffort 同语义，但**越界**（档位合法、只是本模型不支持）
// 时的回退目标换成 preferred＝本地默认档覆盖（upstream.effort_default_overrides）。
//
// 与 ClampEffort 的差别只有一处：请求档位本身受支持时一律不动它——包括「同一个档位的
// 书写形态归一」（"High"→"high"），那是受支持值的形态统一，不是越界回退。
// preferred 为空 / 不在支持集里 / 是关闭思考值 → 完全回落 ClampEffort 既有规则
// （降级优先、偏离最小）；未配置本地覆盖的部署行为逐字不变。
func ClampEffortWithDefault(requested string, supported []string, preferred string) (string, bool) {
	return clampEffort(requested, supported, preferred)
}

func clampEffort(requested string, supported []string, preferred string) (string, bool) {
	req := strings.ToLower(strings.TrimSpace(requested))
	if req == "" || len(supported) == 0 || isEffortOff(req) {
		return "", false
	}
	reqRank, known := effortRank[req]
	if !known {
		return "", false
	}
	// 越界回退目标只须是支持集声明的非关闭档位，不要求出现在静态排序表。
	// 上游新档位也能作为本地默认档；rank 仅用于下方「最近档位」计算。
	pref := strings.ToLower(strings.TrimSpace(preferred))
	if isEffortOff(pref) {
		pref = ""
	}
	best, bestRank := "", -1
	lowest, lowestRank := "", 1<<30
	for _, s := range supported {
		sn := strings.ToLower(strings.TrimSpace(s))
		if sn == "" || isEffortOff(sn) {
			continue
		}
		r, ok := effortRank[sn]
		if !ok {
			continue
		}
		if sn == req {
			if s != requested {
				return s, true // 受支持，但按声明形态统一书写（不是越界，回退目标不参与）
			}
			return "", false
		}
		if r <= reqRank && r > bestRank {
			best, bestRank = s, r
		}
		if r < lowestRank {
			lowest, lowestRank = s, r
		}
	}
	// 越界：本地默认档优先于「偏离最小」——用户把该模型的默认档显式配到那里，
	// 越界请求就落到它（同样只采信支持集里的声明形态）。
	if pref != "" {
		if s, ok := declaredEffort(supported, pref); ok {
			return s, true
		}
	}
	if best != "" {
		return best, true
	}
	if lowest != "" {
		return lowest, true
	}
	return "", false
}

// EffortSupport 返回 realm+model 的可用档位与默认档：远端探测桶（权威）→ 本 realm 的
// 产品级静态兜底表 → 无（nil = 未知，调用方必须透传不降级）。
//
// 与 /v1/models 的 EffortListing 同一函数、同一优先级，保证「宣告的档位」与
// 「可被钳位的档位」永不漂移（两处若各写一份，客户端就会选到一个会被改写的档位）。
//
// 只取本模型的条目，不整桶拷贝：本方法在每个请求的每个 attempt 上都会调
// （handler 的档位归一），走 effortsSnapshot 会把同域的模型档位表整个复制一遍。
func (c *Client) EffortSupport(realm, model string) ([]string, string) {
	if c == nil {
		return nil, ""
	}
	k := realmKey(realm)
	c.effortsMu.RLock()
	// 桶的值是「整体替换」（storeEfforts 换 map，从不原地改 slice），但仍按约定在锁内
	// 复制本模型的切片，避免把可变数据结构带出临界区。
	remote := append([]string(nil), c.efforts[k][model]...)
	def := c.defaultEfforts[k][model]
	c.effortsMu.RUnlock()
	return EffortListing(realm, model, remote, def)
}

// requestEffortKey 定位 obj 上的档位字段，返回 (key, value)。三种别名都认：跨协议桥
// 写 reasoning_effort、原生 OpenAI 系客户端可能写 camel 别名或 reasoning.effort 对象，
// 与 forwarding.reasoning 的读取口径一致（那里两者同读并判冲突）。
func requestEffortKey(obj map[string]any) (string, string) {
	if v, ok := obj["reasoning_effort"].(string); ok && v != "" {
		return "reasoning_effort", v
	}
	if v, ok := obj["reasoningEffort"].(string); ok && v != "" {
		return "reasoningEffort", v
	}
	if m, ok := obj["reasoning"].(map[string]any); ok {
		if v, ok := m["effort"].(string); ok && v != "" {
			return "reasoning.effort", v
		}
	}
	return "", ""
}

// RequestEffortClamp 按 realm+model 的档位能力归一请求档位，**只读不写**：返回
// (字段名, 原值, 出站值)，出站值等于原值（或字段缺失 / 无法归一）时无需改写。
//
// 只读的原因由调用方决定是否可以先拷贝再改写：出站文档在轮转循环里被多个 attempt
// 共享（提示词组合 / 指纹改写都遵守同一「深拷贝后再改」的约定），在这里就地改会污染
// 后续 attempt 与降级判定。
func (c *Client) RequestEffortClamp(obj map[string]any, realm, model string) (key, from, to string) {
	key, from = requestEffortKey(obj)
	if from == "" {
		return "", "", ""
	}
	supported, _ := c.EffortSupport(realm, model)
	// 越界回退目标 = 本地默认档覆盖（未配置该模型 → 空串，语义与历史逐字一致）。
	preferred, _ := defaultOverrideForModel(realm, model, supported)
	to, changed := ClampEffortWithDefault(from, supported, preferred)
	if !changed {
		return key, from, from
	}
	return key, from, to
}

// RequestEffortDefault 计算「客户端未指定档位」时应注入的本地默认档覆盖，返回
// (字段名, 出站档位)；不注入时两者皆空。
//
// 不注入的四类情形（前两类是客户端已表态，后两类是无依据/无配置）：
//   - obj 带任一种档位别名（含 off/none——off 是「关掉思考」，不是没表态）；
//   - obj 用 thinking.type=disabled 显式关闭思考（注入档位会与之冲突，且等于把用户
//     关掉的思考又打开，与 ClampEffort 的同一条原则同源）；
//   - 本地覆盖未命中该 realm+model（空表 = 零行为变化）；
//   - 覆盖值无法落到支持集上（不会为了「有配置」而发一个必 400 的值）。
//
// 与 RequestEffortClamp 同样的「只读不写」契约：调用方决定是否写时拷贝。
func (c *Client) RequestEffortDefault(obj map[string]any, realm, model string) (key, value string) {
	if c == nil || obj == nil || model == "" {
		return "", ""
	}
	if k, _ := requestEffortKey(obj); k != "" {
		return "", ""
	}
	if thinkingDisabled(obj) {
		return "", ""
	}
	supported, _ := c.EffortSupport(realm, model)
	v, ok := defaultOverrideForModel(realm, model, supported)
	if !ok {
		return "", ""
	}
	return "reasoning_effort", v
}

// thinkingDisabled 报告请求是否显式关闭思考（thinking.type=disabled）。
// forwarding.reasoning 已把「thinking.type=disabled 且 effort 非 off」判成非法，注入前
// 必须自证不制造这种组合。
func thinkingDisabled(obj map[string]any) bool {
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		return false
	}
	t, _ := th["type"].(string)
	return t == "disabled"
}

// SetRequestEffort 按 RequestEffortClamp 返回的字段名写回档位（reasoning.effort 走嵌套）。
func SetRequestEffort(obj map[string]any, key, effort string) {
	if key == "reasoning.effort" {
		m, _ := obj["reasoning"].(map[string]any)
		if m == nil {
			m = map[string]any{}
			obj["reasoning"] = m
		}
		m["effort"] = effort
		return
	}
	obj[key] = effort
}
