// Package forwarding implements the input contract, independently of account routing.
package forwarding

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"workbuddy_manager/internal/jsondoc"
	"workbuddy_manager/internal/media"
)

const MaxRequestBytes = 32 << 20

type InvalidRequest struct{ Param, Message string }

func (e *InvalidRequest) Error() string { return e.Param + ": " + e.Message }
func invalid(p, m string) error         { return &InvalidRequest{p, m} }

type Request struct {
	Object         map[string]any
	Model          string
	Stream         bool
	N              int
	ConversationID string
	// PromptCacheKey 是 body 顶层 prompt_cache_key 的原值（≤1024 字节，Parse 已
	// 校验）。语义是 OpenAI 系的"同一会话复用同一前缀"，与会话粘性同源：
	// pi-ai 驱动客户端（dsh 等）把会话 ID 放这个字段而非 conversation_id，
	// handler 据此作粘性键回退（session.BodyKey 的 prompt_cache_key 级，
	// 75c0a78 断链后恢复）。
	// 与 ConversationID 的语义边界：本字段只参与网关侧路由（粘性键），
	// **不**注入上游 X-Conversation-ID / 缓存键材料（上游按对话聚合的判据
	// 不受它污染——conversation 维度键优先且互不混用）。
	PromptCacheKey string
}

// Parse validates known fields without filtering unknown extensions or repairing history.
func Parse(raw []byte) (*Request, error) {
	obj, err := jsondoc.Object(raw)
	if err != nil {
		return nil, invalid("body", err.Error())
	}
	r := &Request{Object: obj, N: 1}
	r.Model, _ = obj["model"].(string)
	if strings.TrimSpace(r.Model) == "" {
		return nil, invalid("model", "non-empty string required")
	}
	if v, ok := obj["stream"]; ok {
		r.Stream, ok = v.(bool)
		if !ok {
			return nil, invalid("stream", "boolean required")
		}
	}
	if v, ok := obj["n"]; ok {
		n, good := jsondoc.Int(v)
		if !good || n < 1 || n > 128 {
			return nil, invalid("n", "integer in [1,128] required")
		}
		r.N = int(n)
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_thinking_tokens"} {
		if v, ok := obj[k]; ok && v != nil {
			n, good := jsondoc.Int(v)
			if !good || n <= 0 {
				return nil, invalid(k, "positive integer required")
			}
		}
	}
	if a, ok := obj["max_tokens"]; ok {
		if b, yes := obj["max_completion_tokens"]; yes {
			aa, _ := json.Marshal(a)
			bb, _ := json.Marshal(b)
			if string(aa) != string(bb) {
				return nil, invalid("max_completion_tokens", "conflicts with max_tokens")
			}
		}
	}
	for _, k := range []string{"parallel_tool_calls", "logprobs", "store"} {
		if v, ok := obj[k]; ok && v != nil {
			if _, ok := v.(bool); !ok {
				return nil, invalid(k, "boolean required")
			}
		}
	}
	for _, k := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
		if v, ok := obj[k]; ok && v != nil {
			if _, ok := v.(json.Number); !ok {
				return nil, invalid(k, "number required")
			}
		}
	}
	if v, ok := obj["stream_options"]; ok {
		if !r.Stream {
			return nil, invalid("stream_options", "only valid for streaming requests")
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, invalid("stream_options", "object required")
		}
		if v, ok := m["include_usage"]; ok {
			if _, ok := v.(bool); !ok {
				return nil, invalid("stream_options.include_usage", "boolean required")
			}
		}
	}
	for _, k := range []string{"functions", "function_call"} {
		if _, ok := obj[k]; ok {
			return nil, invalid(k, "legacy function protocol is unsupported; use tools/tool_calls")
		}
	}
	if v, ok := obj["prompt_cache_key"]; ok {
		s, good := v.(string)
		if !good || len(s) > 1024 {
			return nil, invalid("prompt_cache_key", "string up to 1024 bytes required")
		}
		r.PromptCacheKey = s
	}
	if err := reasoning(obj); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	if v, ok := obj["tools"]; ok {
		tools, ok := v.([]any)
		if !ok {
			return nil, invalid("tools", "array required")
		}
		for i, v := range tools {
			m, ok := v.(map[string]any)
			if !ok || m["type"] != "function" {
				return nil, invalid(fmt.Sprintf("tools[%d]", i), "function tool required")
			}
			fn, _ := m["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if name == "" || names[name] {
				return nil, invalid("tools", "missing or duplicate function name")
			}
			names[name] = true
		}
	}
	if v, ok := obj["tool_choice"]; ok {
		name := ""
		switch tc := v.(type) {
		case string:
			if tc != "auto" && tc != "none" && tc != "required" {
				name = tc
			}
		case map[string]any:
			typ, _ := tc["type"].(string)
			switch typ {
			case "auto", "none", "required":
			case "function":
				fn, _ := tc["function"].(map[string]any)
				name, _ = fn["name"].(string)
				if name == "" {
					return nil, invalid("tool_choice", "function name required")
				}
			default:
				return nil, invalid("tool_choice", "unsupported choice")
			}
		default:
			return nil, invalid("tool_choice", "string or function choice object required")
		}
		if name != "" && !names[name] {
			return nil, invalid("tool_choice", "selected function is not defined")
		}
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil, invalid("messages", "non-empty array required")
	}
	pending := map[string]bool{}
	for i, v := range msgs {
		p := fmt.Sprintf("messages[%d]", i)
		m, ok := v.(map[string]any)
		if !ok {
			return nil, invalid(p, "object required")
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return nil, invalid(p+".role", "unsupported role")
		}
		if _, ok := m["function_call"]; ok {
			return nil, invalid(p+".function_call", "use tool_calls")
		}
		if role == "tool" {
			id, _ := m["tool_call_id"].(string)
			if !pending[id] {
				return nil, invalid(p+".tool_call_id", "no matching unresolved tool call")
			}
			delete(pending, id)
		} else if len(pending) > 0 {
			return nil, invalid(p, "previous tool calls require results before continuing")
		}
		if content, ok := m["content"]; ok && content != nil {
			switch content.(type) {
			case string, []any:
			default:
				return nil, invalid(p+".content", "string, content array or null required")
			}
		}
		if calls, exists := m["tool_calls"]; exists {
			if role != "assistant" {
				return nil, invalid(p+".tool_calls", "only assistant can call tools")
			}
			a, ok := calls.([]any)
			if !ok {
				return nil, invalid(p+".tool_calls", "array required")
			}
			for _, v := range a {
				c, ok := v.(map[string]any)
				if !ok {
					return nil, invalid(p+".tool_calls", "object required")
				}
				id, _ := c["id"].(string)
				if id == "" || pending[id] {
					return nil, invalid(p+".tool_calls", "missing or duplicate call id")
				}
				fn, _ := c["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, ok := fn["arguments"].(string)
				if c["type"] != "function" || name == "" || !ok {
					return nil, invalid(p+".tool_calls", "function name and arguments required")
				}
				if _, err := jsondoc.Decode([]byte(args)); err != nil {
					return nil, invalid(p+".tool_calls.arguments", "valid JSON string required")
				}
				pending[id] = true
			}
		}
	}
	if len(pending) > 0 {
		return nil, invalid("messages", "unresolved tool calls")
	}
	// 媒体形状归一与资源校验（internal/media）：字符串 image_url → 对象形态、
	// 外链/非 data URL/超限图明确 400。这里同时覆盖原生 Chat 与跨协议桥接
	// （跨协议先转成 Chat 形态再走本函数），上游 prepareBody 的二次 Parse 幂等。
	if _, err := media.Normalize(obj); err != nil {
		var me *media.Error
		if errors.As(err, &me) {
			return nil, &InvalidRequest{Param: me.Param, Message: me.Message}
		}
		return nil, invalid("messages", err.Error())
	}
	ids := []any{obj["conversation_id"], obj["conversationId"]}
	if m, ok := obj["metadata"].(map[string]any); ok {
		ids = append(ids, m["conversation_id"], m["conversationId"])
	}
	for _, v := range ids {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok || !ValidID(s) {
			return nil, invalid("conversation_id", "invalid identifier")
		}
		if r.ConversationID != "" && r.ConversationID != s {
			return nil, invalid("conversation_id", "conflicting conversation identifiers")
		}
		r.ConversationID = s
	}
	return r, nil
}

func ValidID(s string) bool {
	if len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func (r *Request) ResolveHeaders(h http.Header) error {
	for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Root-Request-ID", "X-Parent-Conversation-ID", "X-Agent-Type", "X-Trace-ID"} {
		vs := h.Values(key)
		if len(vs) > 1 {
			return invalid(key, "multiple values are not allowed")
		}
		if !ValidID(h.Get(key)) {
			return invalid(key, "invalid identifier")
		}
	}
	if id := h.Get("X-Conversation-ID"); id != "" {
		if r.ConversationID != "" && r.ConversationID != id {
			return invalid("X-Conversation-ID", "conflicts with body")
		}
		r.ConversationID = id
	}
	return nil
}

func reasoning(obj map[string]any) error {
	effort := ""
	vals := []any{obj["reasoning_effort"], obj["reasoningEffort"]}
	if v, ok := obj["reasoning"]; ok {
		m, good := v.(map[string]any)
		if !good {
			return invalid("reasoning", "object required")
		}
		vals = append(vals, m["effort"])
	}
	for _, v := range vals {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			return invalid("reasoning_effort", "non-empty string required")
		}
		if effort != "" && effort != s {
			return invalid("reasoning_effort", "conflicting reasoning aliases")
		}
		effort = s
	}
	typ := ""
	if v, ok := obj["thinking"]; ok {
		m, good := v.(map[string]any)
		if !good {
			return invalid("thinking", "object required")
		}
		typ, _ = m["type"].(string)
		if typ != "enabled" && typ != "disabled" {
			return invalid("thinking.type", "enabled or disabled required")
		}
	}
	off := effort == "off" || effort == "none"
	if typ == "disabled" && effort != "" && !off || typ == "enabled" && off {
		return invalid("thinking", "conflicts with reasoning effort")
	}
	if _, budget := obj["max_thinking_tokens"]; budget && (off || typ == "disabled") {
		return invalid("max_thinking_tokens", "conflicts with disabled reasoning")
	}
	return nil
}

// EncodeObject 对 obj（调用方拥有的可变副本）就地应用声明的 WorkBuddy 线格式转换。
// 不序列化、不改动调用方原始文档。
//
// 为什么拆出对象版本：旧实现的 Encode 靠 json.Marshal → jsondoc.Object 拿一份副本，
// 等于为「不污染入参」这件小事付出了整包序列化 + 整包解析的代价；而调用方往往
// 紧接着还要再做一次改写（缓存键注入）并序列化——合并到同一次序列化里就省下一遍。
func EncodeObject(obj map[string]any, model string) {
	obj["model"] = model
	obj["stream"] = true
	if _, ok := obj["stream_options"]; !ok {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	if _, ok := obj["reasoning_summary"]; !ok {
		obj["reasoning_summary"] = "auto"
	}
	// WorkBuddy's native output budget is max_tokens. Retain exact numeric value.
	if v, ok := obj["max_completion_tokens"]; ok {
		obj["max_tokens"] = v
		delete(obj, "max_completion_tokens")
	}
	if tc, ok := obj["tool_choice"].(map[string]any); ok {
		if typ, _ := tc["type"].(string); typ == "function" {
			if fn, ok := tc["function"].(map[string]any); ok {
				obj["tool_choice"] = fn["name"]
			}
		} else {
			obj["tool_choice"] = typ
		}
	}
	effort, _ := obj["reasoning_effort"].(string)
	if effort == "" {
		effort, _ = obj["reasoningEffort"].(string)
	}
	if m, ok := obj["reasoning"].(map[string]any); ok {
		if effort == "" {
			effort, _ = m["effort"].(string)
		}
		delete(m, "effort")
		if len(m) == 0 {
			delete(obj, "reasoning")
		}
	}
	delete(obj, "reasoningEffort")
	if effort == "off" || effort == "none" {
		delete(obj, "reasoning_effort")
		if th, ok := obj["thinking"].(map[string]any); ok {
			th["type"] = "disabled"
		} else {
			obj["thinking"] = map[string]any{"type": "disabled"}
		}
	} else if effort != "" {
		obj["reasoning_effort"] = effort
	}
	// verbosity：与 reasoning_summary 同源、同门控——官方客户端只在**思考开启**时
	// 把 modelSettings.text.verbosity 置为 "high"（5.6.2 CLI：thinkingEnabled 分支里
	// 同时设 reasoning.summary="auto" 与 text.verbosity="high"），SDK 再映射成 body
	// 的 verbosity（5.7.6 实机抓包逐字可见）。网关此前只补了 reasoning_summary，
	// 少了这一半；补上后“思考请求的出站形态”与官方一致。
	//
	// 门控在思考开启（effort 非空且不是 off，或 thinking.type==enabled）且客户端没自带
	// verbosity 时注入：非思考请求零行为变化（官方对它们也不发这个字段）。
	if _, ok := obj["verbosity"]; !ok && thinkingEnabledForVerbosity(obj) {
		obj["verbosity"] = "high"
	}
}

// thinkingEnabledForVerbosity 报告出站 body 是否“思考开启”（官方客户端
// enableThinking() 的请求侧等价判据：带 reasoning_effort / reasoning.effort，
// 或 thinking.type == "enabled"）。
//
// 调用时机：EncodeObject 末尾（effort 已归一、thinking 已定形）——只看最终形态，
// 不再猜客户端意图；off/none 已在前面删掉 reasoning_effort 并置 disabled，天然为假。
func thinkingEnabledForVerbosity(obj map[string]any) bool {
	if v, ok := obj["reasoning_effort"].(string); ok && strings.TrimSpace(v) != "" {
		return true
	}
	if m, ok := obj["reasoning"].(map[string]any); ok {
		if v, ok := m["effort"].(string); ok && strings.TrimSpace(v) != "" {
			return true
		}
	}
	if m, ok := obj["thinking"].(map[string]any); ok {
		if v, _ := m["type"].(string); v == "enabled" {
			return true
		}
	}
	return false
}

// Encode 执行声明的 WorkBuddy 线格式转换。不做模型名启发式。
func (r *Request) Encode(model string) ([]byte, error) {
	// The document belongs to this request; each encoding starts from a fresh copy.
	obj := jsondoc.CopyObject(r.Object)
	EncodeObject(obj, model)
	return json.Marshal(obj)
}
