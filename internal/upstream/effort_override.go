// effort_override.go 模型思考档位的「本地默认档覆盖」（配置键
// upstream.effort_default_overrides）。
//
// 为什么需要：模型目录里的 reasoning.defaultEffort（远端探测）与 effort_catalog.go
// 的静态兜底默认档都是**上游/编译期**口径，用户改不了——而「默认档」正是客户端
// 不指定档位（或指定了一个该模型不支持的档位）时实际执行的档位。本表是这份口径的
// 唯一本地真相：命中即压过上面两者（**本地的覆盖上游配置**）。
//
// 生效面三处，共用同一份表（表为空 = 零行为变化）：
//  1. 宣告：/v1/models 与面板的 reasoning_default_effort 用本地值——先钳进该模型的
//     支持集（绝不宣告一个模型不接受的档：客户端照宣告选档是硬前提）；
//  2. 注入：客户端一个档位别名都没带、也没显式关闭思考时，出站补上本地默认档
//     （server 侧写进归档 rewritten，可见）；
//  3. 越界回退目标：请求档位合法但本模型不支持时回退到本地默认档，而不是
//     「≤请求档位的最高支持档」（见 ClampEffortWithDefault）。
//
// 键（config 侧已归一，这里只做防御性归一）：
//
//	"model"                      两个域都生效
//	"cn:model" / "global:model"  只该域生效（同名模型在两个域的档位能力可以不同）
//	"*" / "cn:*" / "global:*"    通配；只作用于**档位能力已知**的模型
//	                             （不把档位注入给不认识/不思考的模型）
//
// 值：真实档位名（minimal/low/medium/high/xhigh/max）。off/none 是「关闭思考」的
// 开关值而不是档位，配置侧已拒绝——本地默认档不允许静默打开/关掉思考（与 ClampEffort
// 的两条「不归一」原则同源）。
//
// 并发：读路径（/v1/models 每个模型一次、出站每个 attempt 一次）远多于写路径
// （启动装配 + 配置保存），用 atomic 指针整体换快照，写方永不原地改已发布的 map。
package upstream

import (
	"strings"
	"sync/atomic"
)

// effortDefaultOverrides 当前覆盖表快照（键已小写归一；nil = 无覆盖）。
var effortDefaultOverrides atomic.Pointer[map[string]string]

// SetEffortDefaultOverrides 整体替换本地默认档覆盖表（启动装配与配置保存热应用调用）。
// 空表 / 全空条目 = 清空（回到「完全沿用上游」）。幂等，可被反复调用。
func SetEffortDefaultOverrides(m map[string]string) {
	if len(m) == 0 {
		effortDefaultOverrides.Store(nil)
		return
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.ToLower(strings.TrimSpace(v))
		if key == "" || val == "" {
			continue
		}
		// off/none 是「关闭思考」的开关值，不是档位：默认档永不允许把思考关掉
		// （与 clampEffort 的同一条原则同源）——表里不存，resolve 层再防一次。
		if isEffortOff(val) {
			continue
		}
		cp[key] = val
	}
	if len(cp) == 0 {
		effortDefaultOverrides.Store(nil)
		return
	}
	effortDefaultOverrides.Store(&cp)
}

// resetEffortDefaultOverrides 测试隔离：清空覆盖表。
func resetEffortDefaultOverrides() {
	effortDefaultOverrides.Store(nil)
}

// EffortDefaultOverride 返回 realm+model 命中的本地默认档覆盖原始值（未钳位）。
//
// knownCapability：该模型的档位能力是否已知（远端桶或静态兜底表命中）。通配键
// （"*" / "<realm>:*"）只对能力已知的模型生效——模型名由客户端任意指定，拿通配去给
// 一个本仓不认识的模型注入档位等于替上游编造能力（显式逐模型配置不受此限：那是用户
// 对具体模型的明确声明）。
//
// 优先级：<realm>:<model> > <model> > <realm>:* > *。
func EffortDefaultOverride(realm, model string, knownCapability bool) (string, bool) {
	snap := effortDefaultOverrides.Load()
	if snap == nil || model == "" {
		return "", false
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return "", false
	}
	k := realmKey(realm)
	if v, ok := (*snap)[k+":"+m]; ok {
		return v, true
	}
	if v, ok := (*snap)[m]; ok {
		return v, true
	}
	if knownCapability {
		if v, ok := (*snap)[k+":*"]; ok {
			return v, true
		}
		if v, ok := (*snap)["*"]; ok {
			return v, true
		}
	}
	return "", false
}

// IsOffEffortSwitch 报告字符串是否是「关闭思考」的开关值（off/none，大小写与空白不敏感）。
// 配置侧用它拒绝把开关值写成默认档，运行期用它防呆。
func IsOffEffortSwitch(s string) bool {
	return isEffortOff(strings.ToLower(strings.TrimSpace(s)))
}

// IsKnownEffortTier 报告字符串是否是已知的真实档位名（rank 表内且不是 off/none）。
// 配置侧用它区分「拼错了」与「上游新档位」：不在表内不是语法错误，只告警不拦截
// （档位名集合由上游定义，本仓不该把它变成封闭枚举）。
func IsKnownEffortTier(s string) bool {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" || isEffortOff(v) {
		return false
	}
	_, ok := effortRank[v]
	return ok
}

// declaredEffort 在支持集里找与 want 同形的条目，返回**声明形态**（上游可能大小写敏感，
// 回写必须用它的写法——与 ClampEffort 的形态归一同一约定）。
func declaredEffort(supported []string, want string) (string, bool) {
	for _, s := range supported {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return s, true
		}
	}
	return "", false
}

// resolveDefaultOverride 把本地默认档（原始值）落到支持集上，返回可直接出站/宣告的档位：
//   - 命中支持集 → 按声明形态返回；
//   - 未命中（或形态无法排序）→ 按档位序钳位（≤配置值的最高支持档，全部高于则最低支持档），
//     与请求侧 ClampEffort 同一规则：宁可钳到最近的可执行档，也不把越界值原样发出（必 400）；
//   - supported 为空（能力未知）→ 原样采信（逐模型显式配置就是用户对能力的声明）；
//   - 无法落地（值非法 / 支持集里全是关闭值）→ ""（调用方按「无覆盖」处理）。
func resolveDefaultOverride(value string, supported []string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || isEffortOff(v) {
		return ""
	}
	if len(supported) == 0 {
		return v
	}
	if declared, ok := declaredEffort(supported, v); ok {
		return declared
	}
	if to, changed := ClampEffort(v, supported); changed && to != "" {
		return to
	}
	return ""
}

// defaultOverrideForModel 返回 realm+model 生效的本地默认档（已落到支持集上）。
// supported 为 nil = 能力未知（逐模型配置仍生效，通配不生效）。未命中 / 无法落地 → ok=false。
func defaultOverrideForModel(realm, model string, supported []string) (string, bool) {
	raw, ok := EffortDefaultOverride(realm, model, len(supported) > 0)
	if !ok {
		return "", false
	}
	v := resolveDefaultOverride(raw, supported)
	if v == "" {
		return "", false
	}
	return v, true
}
