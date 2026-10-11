// layer.go 出站指纹改写的**单一入口**：内置指纹表（支持按 ID 禁用与替换覆盖）+ 自定义规则。
//
// 为什么要有这一层而不是直接暴露 scrub.Text：
//
//  1. 改写是**整包级**决策。请求体级预检（Sentinel）一旦判定无指纹，
//     整个 messages 与 tools 遍历都可以跳过。Layer 将内置与自定义哨兵
//     统合为一次原始字节扫描（~13 GB/s）。
//  2. 自定义规则与内置覆盖热改需要原子换代。Layer 用 atomic.Pointer 持有不可变的
//     规则集，读路径无锁；改规则 = 构一个新的 Layer 整体替换。
//  3. 调用方（upstream.prepareBody）只需问一句 "要改写吗"，不必知道
//     内部有几层规则、各自怎么匹配。
//  4. 遍历单遍完成：messages 与 tools 只遍历一次，内置与自定义规则在同一遍中执行。
//
// 零值可用：Enabled 为 false 时所有方法都是空操作（默认严格逐字透传）。
package scrub

import (
	"bytes"
	"fmt"
	"strings"
	"sync/atomic"
)

// Layer 一次改写决策所需的全部规则（不可变；整体替换以实现热改）。
type Layer struct {
	// enabled 总开关。false → 所有方法零成本返回。
	enabled bool

	// customRules 自定义规则（面板展示用；规则本体已编入替换器）。
	customRules []Rule

	// pattern 规则开关与覆盖
	stripBillingKV   bool
	stripBillingBare bool
	bareHeaderShort  string
	stripCCKV        bool

	// 预检哨兵
	sentinels     [][]byte
	foldSentinels [][]byte

	// 编译后的替换器：内置与应用自定义**分两段**执行，保持"内置先改、
	// 自定义在内置结果之上继续改"的级联语义（历史实现是两次字段遍历，
	// 合并成单遍遍历时不能把级联一并丢掉）。
	builtinReplacer *strings.Replacer
	customReplacer  *strings.Replacer
	foldRules       []Rule
}

// BuildLayer 由配置构建出站指纹改写层。
//
//   - enabled: 总开关（false 则所有方法零成本返回）；
//   - disabled: 禁用的内置规则 ID 列表（如 ["err-11128", "hdr-billing"]）；
//   - overrides: 逐内置规则的替换参数覆盖（键 = rule_id，值为新替换文本）；
//   - custom: 自定义规则列表。
func BuildLayer(enabled bool, disabled []string, overrides map[string]string, custom []Rule) (*Layer, error) {
	if !enabled {
		return &Layer{enabled: false, customRules: custom}, nil
	}

	l := &Layer{enabled: true, customRules: custom}

	disSet := make(map[string]bool, len(disabled))
	for _, d := range disabled {
		t := strings.TrimSpace(d)
		if t != "" {
			disSet[t] = true
		}
	}

	// 1. Pattern 规则开关与参数
	l.stripBillingKV = !disSet["hdr-billing"]
	l.stripBillingBare = !disSet["hdr-billing-bare"]
	l.stripCCKV = !disSet["cc-kv"]
	l.bareHeaderShort = headerShort
	if ov, ok := overrides["hdr-billing-bare"]; ok && ov != "" {
		l.bareHeaderShort = ov
	}

	// 2. 收集字面量替换对与哨兵（内置与自定义分开编译，保持级联顺序）
	var builtinLiterals, customLiterals []string
	sentinelSet := make(map[string]bool)

	addSentinel := func(s []byte) {
		if len(s) == 0 {
			return
		}
		str := string(s)
		if !sentinelSet[str] {
			sentinelSet[str] = true
			l.sentinels = append(l.sentinels, s)
		}
	}

	if l.stripBillingKV || l.stripBillingBare {
		addSentinel([]byte("nthropic"))
		addSentinel([]byte("NTHROPIC"))
	}
	if l.stripCCKV {
		addSentinel([]byte(ccPrefix))
	}

	for _, def := range BuiltinRules() {
		if def.Kind != "literal" {
			continue
		}
		if disSet[def.ID] {
			continue
		}
		rep := def.Replace
		if ov, ok := overrides[def.ID]; ok && ov != "" {
			rep = ov
		}
		builtinLiterals = append(builtinLiterals, def.Match, rep)

		// 哨兵覆盖：特定规则使用紧凑超集哨兵，其余取 match 前缀
		switch def.ID {
		case "identity-claude-code", "identity-claude-agent-sdk", "feedback-issue":
			addSentinel([]byte("nthropic"))
			addSentinel([]byte("NTHROPIC"))
		case "pr-branch":
			addSentinel([]byte("Main branch ("))
		case "identity-codex":
			addSentinel([]byte("Codex CLI"))
		case "identity-codex-alt":
			addSentinel([]byte("You are Codex,"))
		case "err-11128":
			addSentinel([]byte("11128"))
		default:
			addSentinel([]byte(def.Match))
		}
	}

	// 3. 自定义规则校验与并入
	if len(custom) > 0 {
		if len(custom) > maxRuleCount {
			return nil, fmt.Errorf("规则数超过上限 %d", maxRuleCount)
		}
		for i, raw := range custom {
			r, err := raw.normalize()
			if err != nil {
				return nil, fmt.Errorf("第 %d 条规则：%w", i+1, err)
			}
			if r.Mode == ModeFold {
				l.foldRules = append(l.foldRules, r)
				l.foldSentinels = append(l.foldSentinels, []byte(strings.ToLower(r.Match)))
				continue
			}
			addSentinel([]byte(r.Match))
			customLiterals = append(customLiterals, r.Match, r.Replace)
		}
	}

	if len(builtinLiterals) > 0 {
		l.builtinReplacer = strings.NewReplacer(builtinLiterals...)
	}
	if len(customLiterals) > 0 {
		l.customReplacer = strings.NewReplacer(customLiterals...)
	}
	return l, nil
}

// NewLayer 构建一层改写器（兼容旧测试与单一自定义入口）。
func NewLayer(enabled bool, custom *Rewriter) *Layer {
	var rules []Rule
	if custom != nil {
		rules = custom.Rules()
	}
	l, err := BuildLayer(enabled, nil, nil, rules)
	if err != nil || l == nil {
		// Rewriter 已校验过规则，这里理论上不可达；仍回退为非 nil 层避免调用方踩空指针。
		return &Layer{enabled: false}
	}
	return l
}

// Enabled 报告该层是否启用。
func (l *Layer) Enabled() bool { return l != nil && l.enabled }

// CustomRules 返回自定义规则副本（面板展示用）。
func (l *Layer) CustomRules() []Rule {
	if l == nil || len(l.customRules) == 0 {
		return nil
	}
	out := make([]Rule, len(l.customRules))
	copy(out, l.customRules)
	return out
}

// Dirty 报告请求体是否可能含指纹（超集判定：false 等价于"确定无指纹"）。
//
// 内置哨兵与自定义哨兵在此合并预检，整包原始字节单遍扫描（~13 GB/s），
// 典型干净请求在这里直接返回，后续 JSON 解析、对象遍历全部省掉。
func (l *Layer) Dirty(body []byte) bool {
	if !l.Enabled() {
		return false
	}
	for _, s := range l.sentinels {
		if bytes.Index(body, s) >= 0 {
			return true
		}
	}
	for _, s := range l.foldSentinels {
		if indexFoldBytes(body, s) >= 0 {
			return true
		}
	}
	return false
}

// Object 就地改写请求体文档中的指纹承载字段（单遍遍历完成内置与自定义规则）。
func (l *Layer) Object(obj map[string]any) bool {
	if !l.Enabled() {
		return false
	}
	return objectWith(obj, l.apply)
}

// apply 对单段文本应用该层的全部规则。无命中返回原字符串（零分配）。
//
// 顺序（与历史实现一致）：pattern 剥离 → 内置字面改写 → 自定义字面改写 →
// 自定义 fold 改写。自定义规则能看到内置改写后的文本（可级联）。
func (l *Layer) apply(s string) string {
	if !l.hasSentinel(s) {
		return s
	}
	if l.stripBillingKV || l.stripBillingBare || l.stripCCKV {
		s = stripKeyValues(s, l.stripBillingKV, l.stripBillingBare, l.bareHeaderShort, l.stripCCKV)
	}
	if l.builtinReplacer != nil {
		s = l.builtinReplacer.Replace(s)
	}
	if l.customReplacer != nil {
		s = l.customReplacer.Replace(s)
	}
	for _, r := range l.foldRules {
		s = replaceFold(s, r.Match, r.Replace)
	}
	return s
}

// hasSentinel 字符串版预检。
func (l *Layer) hasSentinel(s string) bool {
	for _, m := range l.sentinels {
		if strings.Contains(s, string(m)) {
			return true
		}
	}
	for _, m := range l.foldSentinels {
		if indexFold(s, string(m)) >= 0 {
			return true
		}
	}
	return false
}

// objectWith 用给定的单段改写函数单遍遍历全部指纹承载通道：
//   - messages[].content（字符串与多模态 text 块）
//   - messages[].reasoning_content / reasoning
//   - messages[].tool_calls[].function.arguments
//   - tools[].function.description 及 parameters 属性描述/标题
func objectWith(obj map[string]any, apply func(string) string) bool {
	if obj == nil {
		return false
	}
	changed := false
	if msgs, ok := obj["messages"].([]any); ok && len(msgs) > 0 {
		for _, raw := range msgs {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if c, ok := m["content"]; ok {
				switch v := c.(type) {
				case string:
					if n := apply(v); n != v {
						m["content"] = n
						changed = true
					}
				case []any:
					for _, p := range v {
						part, ok := p.(map[string]any)
						if !ok {
							continue
						}
						if t, ok := part["text"].(string); ok {
							if nt := apply(t); nt != t {
								part["text"] = nt
								changed = true
							}
						}
					}
				}
			}
			for _, k := range [2]string{"reasoning_content", "reasoning"} {
				if s, ok := m[k].(string); ok {
					if ns := apply(s); ns != s {
						m[k] = ns
						changed = true
					}
				}
			}
			if tc, ok := m["tool_calls"].([]any); ok {
				for _, rawCall := range tc {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					fn, ok := call["function"].(map[string]any)
					if !ok {
						continue
					}
					args, ok := fn["arguments"].(string)
					if !ok {
						continue
					}
					if na := apply(args); na != args {
						fn["arguments"] = na
						changed = true
					}
				}
			}
		}
	}
	if tools, ok := obj["tools"].([]any); ok && len(tools) > 0 {
		if scrubTools(tools, apply) {
			changed = true
		}
	}
	return changed
}

// scrubTools 遍历 tools 声明中的描述与标题字段。
func scrubTools(list []any, apply func(string) string) bool {
	changed := false
	for _, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if desc, ok := fn["description"].(string); ok {
			if nd := apply(desc); nd != desc {
				fn["description"] = nd
				changed = true
			}
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		props, ok := params["properties"].(map[string]any)
		if !ok {
			continue
		}
		for _, rawP := range props {
			prop, ok := rawP.(map[string]any)
			if !ok {
				continue
			}
			if pdesc, ok := prop["description"].(string); ok {
				if npd := apply(pdesc); npd != pdesc {
					prop["description"] = npd
					changed = true
				}
			}
			if ptitle, ok := prop["title"].(string); ok {
				if npt := apply(ptitle); npt != ptitle {
					prop["title"] = npt
					changed = true
				}
			}
		}
	}
	return changed
}

// Holder 原子持有当前 Layer（面板热改 + chat 热路径并发）。
type Holder struct {
	p atomic.Pointer[Layer]
}

// NewHolder 构建 holder；nil 初始层等价于"未启用"。
func NewHolder(l *Layer) *Holder {
	h := &Holder{}
	h.Store(l)
	return h
}

// Load 返回当前层（未 Store / nil holder 时返回 nil，调用方判空即可）。
func (h *Holder) Load() *Layer {
	if h == nil {
		return nil
	}
	return h.p.Load()
}

// Store 整体替换当前层。
func (h *Holder) Store(l *Layer) {
	if h == nil {
		return
	}
	h.p.Store(l)
}
