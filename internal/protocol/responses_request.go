package protocol

import "fmt"

func responsesRequest(src map[string]any, tools *toolIndex, dropped *[]string) (map[string]any, error) {
	// The accepted-but-ignored list holds client telemetry, routing/caching
	// hints and newer official parameters with no Chat equivalent. Codex sends
	// client_metadata/service_tier/prompt_cache_key/stream_options on every
	// request; dropping them keeps real clients working without pretending the
	// bridge honors them.
	//
	// prompt_cache_key 例外：会话粘性键链路需要它（session.BodyKey 的
	// prompt_cache_key 级，pi-ai/Codex 类客户端把会话 ID 放这里而非 conversation
	// 维度）。在“忽略”之前把它提升为 Chat 顶层字段，forwarding.Parse 会照常校验
	// 并落到 Request.PromptCacheKey——否则跨协议请求丢键，同会话每笔独立轮换。
	ignored, _ := fieldsAccept(src, "", "model input instructions stream store previous_response_id conversation background tools tool_choice parallel_tool_calls max_output_tokens temperature top_p metadata text reasoning include truncation", "*")
	// prompt_cache_key 从“被忽略”名单除名（提升为路由线索，见下方 dst 构造处）；
	// 必须在 *dropped append 之前完成，否则归档事件仍把它记成丢弃。
	if v, ok := src["prompt_cache_key"].(string); ok && v != "" {
		for i, k := range ignored {
			if k == "prompt_cache_key" {
				ignored = append(ignored[:i], ignored[i+1:]...)
				break
			}
		}
	}
	*dropped = append(*dropped, ignored...)
	if v, ok := src["store"].(bool); ok && v {
		*dropped = append(*dropped, "store")
	}
	for _, k := range []string{"previous_response_id", "conversation"} {
		if src[k] != nil {
			return nil, invalid(k, "server-side conversation state is not implemented; send complete input history")
		}
	}
	if v, ok := src["background"].(bool); ok && v {
		*dropped = append(*dropped, "background")
	}
	if v := src["truncation"]; v != nil {
		s, err := stringValue(v, "truncation")
		if err != nil {
			return nil, err
		}
		if s != "auto" && s != "disabled" {
			return nil, invalid("truncation", "auto or disabled expected")
		}
		// The gateway never truncates server-side (stateless, no stored
		// history); auto is the official default and is ignored.
		if s == "auto" {
			*dropped = append(*dropped, "truncation")
		}
	}
	if v := src["include"]; v != nil {
		a, ok := v.([]any)
		if !ok {
			return nil, invalid("include", "array required")
		}
		for i, item := range a {
			if _, err := stringValue(item, fmt.Sprintf("include[%d]", i)); err != nil {
				return nil, err
			}
		}
		// include only requests extra output fields (for example
		// reasoning.encrypted_content). The stateless bridge can produce none of
		// them, so a non-empty list is accepted and ignored like an empty one.
		if len(a) > 0 {
			*dropped = append(*dropped, "include")
		}
	}
	verbosity := ""
	if v := src["text"]; v != nil {
		m, err := object(v, "text")
		if err != nil {
			return nil, err
		}
		if err = fields(m, "text", "format", "verbosity"); err != nil {
			return nil, err
		}
		if v := m["verbosity"]; v != nil {
			verbosity, err = stringValue(v, "text.verbosity")
			if err != nil {
				return nil, err
			}
			if verbosity != "low" && verbosity != "medium" && verbosity != "high" {
				return nil, invalid("text.verbosity", "low, medium or high required")
			}
		}
		if v := m["format"]; v != nil {
			f, err := object(v, "text.format")
			if err != nil {
				return nil, err
			}
			if f["type"] != "text" {
				*dropped = append(*dropped, "text.format")
			}
		}
	}
	dst := map[string]any{"model": src["model"]}
	if verbosity != "" {
		dst["verbosity"] = verbosity
	}
	// prompt_cache_key 提升（除名逻辑见函数头 fieldsAccept 处）：会话粘性键链路
	// 需要它（session.BodyKey 的 prompt_cache_key 级，pi-ai/Codex 类客户端把会话
	// ID 放这里而非 conversation 维度）。提升为 Chat 顶层字段后由 forwarding.Parse
	// 校验并落到 Request.PromptCacheKey——否则跨协议请求丢键，同会话每笔独立轮换。
	if v, ok := src["prompt_cache_key"].(string); ok && v != "" {
		dst["prompt_cache_key"] = v
	}
	if v, ok := src["stream"]; ok {
		dst["stream"] = v
	}
	if v := src["reasoning"]; v != nil {
		m, err := object(v, "reasoning")
		if err != nil {
			return nil, err
		}
		if err = fields(m, "reasoning", "effort summary context"); err != nil {
			return nil, err
		}
		// summary/context shape reasoning output, which this bridge cannot
		// return; they are dropped rather than rejected. effort still steers the
		// upstream model even though the reasoning text is not delivered.
		for _, k := range []string{"summary", "context"} {
			if v := m[k]; v != nil {
				if _, err = stringValue(v, "reasoning."+k); err != nil {
					return nil, err
				}
				*dropped = append(*dropped, "reasoning."+k)
			}
		}
		if v := m["effort"]; v != nil {
			// 合法档位集是 realm/模型相关的（同一模型名 CN 与 global 档位可以不同，
			// 见 upstream/effort_catalog.go），桥接层没有这份信息，因此**不在这里**
			// 用字面量白名单断言：只要求非空字符串，越界/未知档位交给出站钳位
			// （upstream.ClampEffort）归一，未知模型则透传由上游裁决。
			//
			// 历史教训：这里的白名单曾漏掉 max，而 /v1/models 正是靠同一份静态表
			// 宣告 glm-5.3-flash 支持 [low high max]，客户端选 max 必 400——网关
			// 自相矛盾；原生 Chat 入口（forwarding.reasoning）从未拒过任何非空档位。
			s, err := nonempty(v, "reasoning.effort")
			if err != nil {
				return nil, err
			}
			dst["reasoning_effort"] = s
		}
	}
	if v := src["max_output_tokens"]; v != nil {
		dst["max_tokens"] = v
	}
	if err := sampling(src, dst, 2); err != nil {
		return nil, err
	}
	if v := src["metadata"]; v != nil {
		if _, err := object(v, "metadata"); err != nil {
			return nil, err
		}
		// Client metadata is echoed only, not interpreted as WorkBuddy routing.
	}
	if v := src["tools"]; v != nil {
		declared, err := tools.declarations(v, dropped)
		if err != nil {
			return nil, err
		}
		dst["tools"] = declared
	}
	if v := src["parallel_tool_calls"]; v != nil {
		dst["parallel_tool_calls"] = v
	}
	if v := src["tool_choice"]; v != nil {
		if s, ok := v.(string); ok {
			if s != "auto" && s != "none" && s != "required" {
				return nil, invalid("tool_choice", "auto, none or required expected")
			}
			dst["tool_choice"] = s
		} else {
			m, err := object(v, "tool_choice")
			if err != nil {
				return nil, err
			}
			if err = fields(m, "tool_choice", "type name"); err != nil {
				return nil, err
			}
			if m["type"] != "function" {
				return nil, invalid("tool_choice.type", "function required")
			}
			name, err := tools.choice(m["name"])
			if err != nil {
				return nil, err
			}
			dst["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	messages := []any{}
	if v := src["instructions"]; v != nil {
		s, err := stringValue(v, "instructions")
		if err != nil {
			return nil, err
		}
		messages = append(messages, map[string]any{"role": "system", "content": s})
	}
	switch input := src["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input})
	case []any:
		// Group adjacent function calls and preceding assistant text into the same
		// Chat turn. Never move an item across a user or tool-result boundary.
		var assistant map[string]any
		for i, v := range input {
			p := fmt.Sprintf("input[%d]", i)
			m, err := object(v, p)
			if err != nil {
				return nil, err
			}
			switch m["type"] {
			case nil, "message":
				if err = fields(m, p, "type role content id status", "phase"); err != nil {
					return nil, err
				}
				if v := m["phase"]; v != nil {
					// ResponseItem.Message.phase classifies assistant text as
					// commentary/partial_answer/final_answer. It only orders output for
					// the producing provider, which Chat cannot express; accept any
					// string for forward compatibility and drop it.
					if _, err = stringValue(v, p+".phase"); err != nil {
						return nil, err
					}
					*dropped = append(*dropped, p+".phase")
				}
				if v := m["id"]; v != nil {
					if _, err = nonempty(v, p+".id"); err != nil {
						return nil, err
					}
				}
				if v := m["status"]; v != nil && v != "completed" {
					return nil, invalid(p+".status", "only completed history items are supported")
				}
				role, _ := m["role"].(string)
				if role != "user" && role != "assistant" && role != "system" && role != "developer" {
					return nil, invalid(p+".role", "unsupported message role")
				}
				// responsesContent 按 role 决定允许的 part 类型：assistant 只收文本
				// （input_text / output_text，replayed ResponseOutputMessage 无损回放），
				// user 额外收 input_image，system/developer 只收 input_text。
				content, err := responsesContent(m["content"], p+".content", role)
				if err != nil {
					return nil, err
				}
				msg := map[string]any{"role": role, "content": content}
				messages = append(messages, msg)
				assistant = nil
				if role == "assistant" {
					assistant = msg
				}
			case "function_call":
				if err = fields(m, p, "type id call_id name namespace arguments status"); err != nil {
					return nil, err
				}
				if v := m["id"]; v != nil {
					if _, err = nonempty(v, p+".id"); err != nil {
						return nil, err
					}
				}
				if v := m["status"]; v != nil && v != "completed" {
					return nil, invalid(p+".status", "only completed tool history is supported")
				}
				name, err := nonempty(m["name"], p+".name")
				if err != nil {
					return nil, err
				}
				name, err = tools.history(name, m["namespace"], p)
				if err != nil {
					return nil, err
				}
				call, err := toolCall(m["call_id"], name, m["arguments"], p)
				if err != nil {
					return nil, err
				}
				if assistant == nil {
					assistant = map[string]any{"role": "assistant", "content": nil}
					messages = append(messages, assistant)
				}
				calls, _ := assistant["tool_calls"].([]any)
				assistant["tool_calls"] = append(calls, call)
			case "function_call_output":
				if err = fields(m, p, "type call_id output id status"); err != nil {
					return nil, err
				}
				if v := m["status"]; v != nil && v != "completed" {
					return nil, invalid(p+".status", "only completed tool results are supported")
				}
				if v := m["id"]; v != nil {
					if _, err = nonempty(v, p+".id"); err != nil {
						return nil, err
					}
				}
				id, err := nonempty(m["call_id"], p+".call_id")
				if err != nil {
					return nil, err
				}
				output, err := responsesToolOutput(m["output"], p+".output")
				if err != nil {
					return nil, err
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": output})
				assistant = nil
			case "reasoning":
				// Client-side reasoning state (summary/content/encrypted_content)
				// has no Chat representation. Accept and drop it so consumers that
				// replay their full history (Codex sends encrypted reasoning items
				// every turn) keep working; the model re-reasons from the visible
				// messages.
				*dropped = append(*dropped, p)
			default:
				*dropped = append(*dropped, p)
			}
		}
	default:
		return nil, invalid("input", "string or array required")
	}
	dst["messages"] = messages
	return dst, nil
}
