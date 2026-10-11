package protocol

import (
	"encoding/json"
	"fmt"

	"workbuddy_manager/internal/jsondoc"
)

func anthropicRequest(src map[string]any, dropped *[]string) (map[string]any, error) {
	// metadata/service_tier/top_k/cache_control 是客户端遥测、路由与缓存提示，
	// output_config.format 以外的字段无 Chat 等价物：接受并在 Dropped 里记账。
	ignored, _ := fieldsAccept(src, "", "model messages system max_tokens stream temperature top_p tools tool_choice thinking stop_sequences output_config", "*")
	*dropped = append(*dropped, ignored...)
	if n, ok := jsondoc.Int(src["max_tokens"]); !ok || n <= 0 {
		return nil, invalid("max_tokens", "positive integer required")
	}
	dst := map[string]any{"model": src["model"], "max_tokens": src["max_tokens"]}
	if v, ok := src["stream"]; ok {
		dst["stream"] = v
	}
	if err := sampling(src, dst, 1); err != nil {
		return nil, err
	}
	// output_config 是新版 Messages 的推理控制对象（pi 的 forceAdaptiveThinking /
	// supportsMidConvoEffort 路径会发 {effort:"high"|…}）：effort 是真正的生成参数，
	// 与 Responses 的 reasoning.effort 同义，映射为 Chat reasoning_effort（越界档位由
	// 出站钳位归一，见 responses_request.go 同处注释）。未知子字段仍 400：
	// output_config.format（结构化输出）本网关未验证，不得静默丢掉语义。
	if v := src["output_config"]; v != nil {
		m, err := object(v, "output_config")
		if err != nil {
			return nil, err
		}
		if err = fields(m, "output_config", "effort"); err != nil {
			return nil, err
		}
		if v := m["effort"]; v != nil {
			s, err := nonempty(v, "output_config.effort")
			if err != nil {
				return nil, err
			}
			dst["reasoning_effort"] = s
		}
	}
	if v := src["thinking"]; v != nil {
		m, err := object(v, "thinking")
		if err != nil {
			return nil, err
		}
		// display 只影响推理展示档（本桥不交付推理正文）；block_binding 是官方
		// 多轮块绑定调优（客户端侧行为，桥接不参与）：接受即丢，记账。
		thinkingIgnored, err := fieldsAccept(m, "thinking", "type budget_tokens", "display block_binding")
		if err != nil {
			return nil, err
		}
		for _, k := range thinkingIgnored {
			*dropped = append(*dropped, "thinking."+k)
		}
		switch m["type"] {
		case "disabled":
			dst["thinking"] = map[string]any{"type": "disabled"}
		case "enabled", "adaptive":
			// adaptive 是官方自适应思考模式（客户端不报 budget，由模型自行决定什么时候
			// 想）：与 enabled 同口径处理——只校验 budget 形态，状态一律丢弃。
			// Signed thinking state cannot be replayed by the Chat bridge (the
			// upstream emits unsigned reasoning text). The request is accepted so
			// thinking-enabled clients keep working, but the budget/state is
			// dropped and responses carry no thinking blocks. The model still
			// reasons per its own defaults.
			if v := m["budget_tokens"]; v != nil {
				if n, ok := jsondoc.Int(v); !ok || n <= 0 {
					return nil, invalid("thinking.budget_tokens", "positive integer required")
				}
			}
			// 只记字段路径不记值：面板列回答的是「客户端要了什么被我们丢了」，而
			// enabled / adaptive 的区别对下游排查无意义（两者都是状态被丢弃、模型重新推理）。
			*dropped = append(*dropped, "thinking")
		default:
			return nil, invalid("thinking.type", "disabled, enabled or adaptive expected")
		}
	}
	if v := src["stop_sequences"]; v != nil {
		a, ok := v.([]any)
		if !ok {
			return nil, invalid("stop_sequences", "array required")
		}
		if len(a) > 0 {
			stop := make([]any, 0, len(a))
			for i, item := range a {
				s, err := nonempty(item, fmt.Sprintf("stop_sequences[%d]", i))
				if err != nil {
					return nil, err
				}
				stop = append(stop, s)
			}
			// Pass the sequences through as Chat `stop`: honoring the client's
			// stop request matters more than the response-side approximation.
			// The upstream strips the matched sequence and only reports
			// finish_reason:stop, so the Messages response cannot set
			// stop_reason:"stop_sequence"/stop_sequence and maps it to end_turn
			// instead (see response.go stopReason). Claude Code auto_mode sends
			// stop_sequences:["</block>"].
			dst["stop"] = stop
		}
	}
	if v := src["tools"]; v != nil {
		a, ok := v.([]any)
		if !ok {
			return nil, invalid("tools", "array required")
		}
		tools := make([]any, 0, len(a))
		for i, v := range a {
			p := fmt.Sprintf("tools[%d]", i)
			m, err := object(v, p)
			if err != nil {
				return nil, err
			}
			toolIgnored, _ := fieldsAccept(m, p, "type name description input_schema strict cache_control", "*")
			for _, k := range toolIgnored {
				*dropped = append(*dropped, p+"."+k)
			}
			if v := m["type"]; v != nil && v != "custom" {
				*dropped = append(*dropped, p+".type")
			}
			if v, ok := m["strict"].(bool); ok && v {
				*dropped = append(*dropped, p+".strict")
			}
			t, err := function(m["name"], m["description"], m["input_schema"], p)
			if err != nil {
				return nil, err
			}
			tools = append(tools, t)
		}
		dst["tools"] = tools
	}
	if v := src["tool_choice"]; v != nil {
		m, err := object(v, "tool_choice")
		if err != nil {
			return nil, err
		}
		if err = fields(m, "tool_choice", "type name disable_parallel_tool_use"); err != nil {
			return nil, err
		}
		switch m["type"] {
		case "auto", "none":
			dst["tool_choice"] = m["type"]
		case "any":
			dst["tool_choice"] = "required"
		case "tool":
			n, err := nonempty(m["name"], "tool_choice.name")
			if err != nil {
				return nil, err
			}
			dst["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": n}}
		default:
			return nil, invalid("tool_choice.type", "auto, any, none or tool required")
		}
		if m["type"] != "tool" && m["name"] != nil {
			return nil, invalid("tool_choice.name", "only valid with type:tool")
		}
		if v, exists := m["disable_parallel_tool_use"]; exists {
			b, ok := v.(bool)
			if !ok {
				return nil, invalid("tool_choice.disable_parallel_tool_use", "boolean required")
			}
			dst["parallel_tool_calls"] = !b
		}
	}
	messages := []any{}
	if v := src["system"]; v != nil {
		content, err := textParts(v, "system", "text")
		if err != nil {
			return nil, err
		}
		messages = append(messages, map[string]any{"role": "system", "content": content})
	}
	a, ok := src["messages"].([]any)
	if !ok || len(a) == 0 {
		return nil, invalid("messages", "non-empty array required")
	}
	for i, v := range a {
		p := fmt.Sprintf("messages[%d]", i)
		m, err := object(v, p)
		if err != nil {
			return nil, err
		}
		if err = fields(m, p, "role content"); err != nil {
			return nil, err
		}
		role, _ := m["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, invalid(p+".role", "user or assistant required")
		}
		msg := map[string]any{"role": role}
		if s, ok := m["content"].(string); ok {
			msg["content"] = s
			messages = append(messages, msg)
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok || len(blocks) == 0 {
			return nil, invalid(p+".content", "string or non-empty content array required")
		}
		parts, calls := []any{}, []any{}
		seenCall := false
		for j, v := range blocks {
			bp := fmt.Sprintf("%s.content[%d]", p, j)
			b, err := object(v, bp)
			if err != nil {
				return nil, err
			}
			switch b["type"] {
			case "text":
				if seenCall {
					return nil, invalid(bp, "text after tool_use cannot preserve block order in Chat")
				}
				part, err := textPartFrom(b, bp, "text")
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			case "image":
				if role != "user" {
					return nil, invalid(bp, "image blocks are only supported in user messages")
				}
				part, err := anthropicImagePart(b, bp)
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			case "tool_use":
				if role != "assistant" {
					return nil, invalid(bp, "tool_use requires assistant role")
				}
				if err = fields(b, bp, "type id name input cache_control"); err != nil {
					return nil, err
				}
				args, err := object(b["input"], bp+".input")
				if err != nil {
					return nil, err
				}
				raw, err := json.Marshal(args)
				if err != nil {
					return nil, err
				}
				call, err := toolCall(b["id"], b["name"], string(raw), bp)
				if err != nil {
					return nil, err
				}
				calls = append(calls, call)
				seenCall = true
			case "tool_result":
				if role != "user" || len(parts) != 0 {
					return nil, invalid(bp, "tool_result must precede user text and images")
				}
				if err = fields(b, bp, "type tool_use_id content is_error cache_control"); err != nil {
					return nil, err
				}
				failed := false
				if v, exists := b["is_error"]; exists && v != nil {
					bv, ok := v.(bool)
					if !ok {
						return nil, invalid(bp+".is_error", "boolean required")
					}
					failed = bv
				}
				id, err := nonempty(b["tool_use_id"], bp+".tool_use_id")
				if err != nil {
					return nil, err
				}
				content := any("")
				if v, exists := b["content"]; exists {
					content, err = anthropicToolResult(v, bp+".content")
					if err != nil {
						return nil, err
					}
				}
				if failed {
					content = markToolError(content)
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
			case "thinking", "redacted_thinking":
				if role != "assistant" {
					return nil, invalid(bp, "thinking blocks require assistant role")
				}
				// Signed/opaque reasoning history cannot be represented in Chat;
				// accept and drop it so thinking-enabled clients can replay their
				// full transcript. The model re-reasons from the visible messages.
				*dropped = append(*dropped, bp)
			default:
				*dropped = append(*dropped, bp)
			}
		}
		if len(parts) > 0 || len(calls) > 0 {
			msg["content"] = nil
			if len(parts) > 0 {
				msg["content"] = parts
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)
		}
	}
	dst["messages"] = messages
	return dst, nil
}

// markToolError makes an Anthropic is_error:true tool result visible to the
// model. Chat has no structured tool-failure flag, so the failure is expressed
// as a text marker. This is an explicit, visible lossy translation: the client
// keeps its own is_error state locally and replays it on the next turn.
func markToolError(content any) any {
	switch v := content.(type) {
	case string:
		if v == "" {
			return "[tool error]"
		}
		return "[tool error] " + v
	case []any:
		return append([]any{map[string]any{"type": "text", "text": "[tool error]"}}, v...)
	default:
		return "[tool error]"
	}
}
