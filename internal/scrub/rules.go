// rules.go 自定义改写规则：让使用者把自己遇到的指纹词/句加进来。
//
// 与内置表（scrub.go / layer.go）的关系：
//
//   - 内置表由 BuiltinRules() 定义（各规则的默认匹配与替换）：默认全启用，
//     配置层可按 ID 停用或覆盖替换文本（fingerprint_disabled / fingerprint_overrides）；
//   - 自定义规则是**运行期可变**的独立一层：先跑内置、再跑自定义（可级联）。
//     规则集不可变，改规则 = 整体替换一个 *Rewriter（调用方用 atomic.Pointer）。
//
// 两种匹配模式、两种动作，刻意不做正则——正则既是配置注入面（灾难性回溯
// 会让一个请求吃满 CPU）也是性能坑，而实际需求只是"这些词/句要改掉"：
//
//	literal : 逐字匹配（大小写敏感）
//	fold    : 逐字匹配，忽略 ASCII 大小写
//	replace : 换成给定文本
//	remove  : 整段删除
//
// 先证伪再定位：请求体级 Sentinel 用每条规则的匹配串做字节预检，
// 未命中即跳过整次遍历；命中才进入定点替换。
package scrub

import (
	"bytes"
	"fmt"
	"strings"
)

// 上限：规则表参与请求体级预检（每条一次扫描），无上限会让"扫一遍请求体"
// 变成可观测的固定成本。超限直接拒绝——配置错误要在保存时报出来，
// 而不是运行期静默丢弃。
const (
	maxRuleCount = 256
	maxRuleLen   = 512
)

// 匹配模式。
const (
	ModeLiteral = "literal"
	ModeFold    = "fold"
)

// 动作。
const (
	ActionReplace = "replace"
	ActionRemove  = "remove"
)

// Rule 一条自定义改写规则。
type Rule struct {
	// Match 要匹配的词/句（非空，≤ maxRuleLen 字节）。
	Match string `json:"match"`
	// Replace 替换文本（replace 动作使用；remove 动作忽略）。
	Replace string `json:"replace,omitempty"`
	// Mode literal（默认）/ fold（忽略 ASCII 大小写）。
	Mode string `json:"mode,omitempty"`
	// Action replace（默认）/ remove（整段删除）。
	Action string `json:"action,omitempty"`
}

// normalize 归一化并校验；错误信息带上下文，便于面板直接展示。
func (r Rule) normalize() (Rule, error) {
	r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	if r.Mode == "" {
		r.Mode = ModeLiteral
	}
	r.Action = strings.ToLower(strings.TrimSpace(r.Action))
	if r.Action == "" {
		r.Action = ActionReplace
	}
	if r.Match == "" {
		return Rule{}, fmt.Errorf("match 不能为空")
	}
	// 纯空白匹配拒收：单个空格会命中几乎所有文本，把整段请求体改得面目全非。
	// （词/句清洗的意图是字面串，首尾带空格可以接受，但不能全是空白。）
	if strings.TrimSpace(r.Match) == "" {
		return Rule{}, fmt.Errorf("match 不能是纯空白（会命中几乎所有文本）")
	}
	if len(r.Match) > maxRuleLen {
		return Rule{}, fmt.Errorf("match 超过 %d 字节", maxRuleLen)
	}
	if len(r.Replace) > maxRuleLen {
		return Rule{}, fmt.Errorf("replace 超过 %d 字节", maxRuleLen)
	}
	switch r.Mode {
	case ModeLiteral, ModeFold:
	default:
		return Rule{}, fmt.Errorf("mode 只支持 literal / fold，得到 %q", r.Mode)
	}
	switch r.Action {
	case ActionReplace, ActionRemove:
	default:
		return Rule{}, fmt.Errorf("action 只支持 replace / remove，得到 %q", r.Action)
	}
	if r.Action == ActionRemove {
		r.Replace = ""
	}
	// 替换目标包含匹配串 → 替换后仍命中，而 Replacer 不做重复替换，
	// 结果等于没改。这种规则几乎肯定是配置笔误，直接拒绝。
	if r.Action == ActionReplace && r.Replace != "" {
		hay, needle := r.Replace, r.Match
		if r.Mode == ModeFold {
			hay, needle = strings.ToLower(hay), strings.ToLower(needle)
		}
		if strings.Contains(hay, needle) {
			return Rule{}, fmt.Errorf("replace 不能包含 match（会造成无效改写）")
		}
	}
	return r, nil
}

// Rewriter 一组预编译的自定义规则。不可变；改规则 = 整体替换实例。
// nil 表示"无自定义规则"（所有方法对 nil 安全，热路径零成本）。
type Rewriter struct {
	rules []Rule
	// literal 规则的匹配/替换对（交给 strings.Replacer，trie 单遍扫描）。
	literals *strings.Replacer
	// sentinels 预检哨兵：literal 规则用匹配串原文；fold 规则用其小写形态
	// （匹配时按大小写不敏感查找，等价于覆盖全部大小写变体）。
	litSentinels  [][]byte
	foldSentinels [][]byte
	// foldRules literal 之外的规则（逐条定点替换；数量少，命中才走）。
	foldRules []Rule
}

// Build 预编译规则集。规则非法或超量 → 返回错误（调用方 fail fast）。
// 空规则集返回 (nil, nil)：热路径据此完全跳过自定义层。
func Build(rules []Rule) (*Rewriter, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	if len(rules) > maxRuleCount {
		return nil, fmt.Errorf("规则数超过上限 %d", maxRuleCount)
	}
	w := &Rewriter{rules: make([]Rule, 0, len(rules))}
	literals := make([]string, 0, len(rules)*2)
	for i, raw := range rules {
		r, err := raw.normalize()
		if err != nil {
			return nil, fmt.Errorf("第 %d 条规则：%w", i+1, err)
		}
		w.rules = append(w.rules, r)
		if r.Mode == ModeFold {
			w.foldRules = append(w.foldRules, r)
			w.foldSentinels = append(w.foldSentinels, []byte(strings.ToLower(r.Match)))
			continue
		}
		w.litSentinels = append(w.litSentinels, []byte(r.Match))
		literals = append(literals, r.Match, r.Replace)
	}
	if len(literals) > 0 {
		w.literals = strings.NewReplacer(literals...)
	}
	return w, nil
}

// Rules 返回规则副本（面板展示用）。
func (w *Rewriter) Rules() []Rule {
	if w == nil {
		return nil
	}
	out := make([]Rule, len(w.rules))
	copy(out, w.rules)
	return out
}

// Len 返回规则条数。
func (w *Rewriter) Len() int {
	if w == nil {
		return 0
	}
	return len(w.rules)
}

// Sentinel 报告请求体是否可能命中任一自定义规则。
// 与内置哨兵同一口径：超集判定，false 等价于"确定无命中"。
func (w *Rewriter) Sentinel(body []byte) bool {
	if w == nil {
		return false
	}
	for _, s := range w.litSentinels {
		if bytes.Contains(body, s) {
			return true
		}
	}
	for _, s := range w.foldSentinels {
		if indexFoldBytes(body, s) >= 0 {
			return true
		}
	}
	return false
}

// Apply 对单段文本执行全部自定义规则。无命中返回**同一字符串**（零分配）。
func (w *Rewriter) Apply(s string) string {
	if w == nil || !w.hasSentinel(s) {
		return s
	}
	if w.literals != nil {
		s = w.literals.Replace(s)
	}
	for _, r := range w.foldRules {
		s = replaceFold(s, r.Match, r.Replace)
	}
	return s
}

// hasSentinel 字符串版预检（与 Sentinel 同一套哨兵）。
func (w *Rewriter) hasSentinel(s string) bool {
	for _, m := range w.litSentinels {
		if strings.Contains(s, string(m)) {
			return true
		}
	}
	for _, m := range w.foldSentinels {
		if indexFold(s, string(m)) >= 0 {
			return true
		}
	}
	return false
}

// replaceFold 大小写不敏感（ASCII）地把 s 中的 match 全部换成 repl。
//
// 用"逐次定位 + 按段拼接"实现，替换文本原样插入（不做大小写模仿）：
// 这样语义可预测（面板里写什么就得到什么），也不需要维护大小写变体表。
// 只在已知存在命中时调用（预检先行），因此一次扫描失败不会白跑。
func replaceFold(s, match, repl string) string {
	if match == "" {
		return s
	}
	// 快速路径：无命中直接返回原串。
	if indexFold(s, match) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := indexFold(s, match)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s = s[i+len(match):]
	}
}

// indexFoldBytes 大小写不敏感（ASCII）的字节子串查找。
func indexFoldBytes(b, sub []byte) int {
	n, m := len(b), len(sub)
	if m == 0 || n < m {
		return -1
	}
	c0 := lowerASCII(sub[0])
	for i := 0; i+m <= n; i++ {
		if lowerASCII(b[i]) != c0 {
			continue
		}
		j := 1
		for j < m && lowerASCII(b[i+j]) == lowerASCII(sub[j]) {
			j++
		}
		if j == m {
			return i
		}
	}
	return -1
}
