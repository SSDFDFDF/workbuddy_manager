// Package protocol implements the explicitly supported, stateless subset of
// Responses and Messages on top of the native Chat forwarding contract.
// It never repairs history, invents reasoning, or forwards foreign extensions.
package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"workbuddy_manager/internal/forwarding"
	"workbuddy_manager/internal/jsondoc"
	"workbuddy_manager/internal/media"
)

type Kind string

const (
	Chat      Kind = "chat"
	Responses Kind = "responses"
	Anthropic Kind = "anthropic"
)

// Request keeps the wire request separate from protocol-only response metadata.
// Source is owned by this request; it is never sent to WorkBuddy.
// Mutated 标记 Decode 是否改写过 Chat.Object（工具结果图片抬升）：原生 Chat 的
// 严格透传快路径（original body 原字节出站）只在未改写时可用。
type Request struct {
	Kind    Kind
	Chat    *forwarding.Request
	Source  map[string]any
	Mutated bool
	// Dropped lists client fields that were accepted and ignored because the
	// stateless Chat bridge cannot represent them (client telemetry, caching
	// hints, newer protocol fields, client-side reasoning state). Keeping the
	// list makes the lossy translation observable instead of silent.
	Dropped []string
	tools   *toolIndex
}

func invalid(path, message string) error {
	return &forwarding.InvalidRequest{Param: path, Message: message}
}

// fields rejects any key outside allowed unless it is explicitly listed in
// accept. Accepted keys are knowingly ignored: use this only for client-side
// hints or newer fields whose loss does not change generation semantics.
func fields(obj map[string]any, path, allowed string, accept ...string) error {
	_, err := fieldsAccept(obj, path, allowed, accept...)
	return err
}

// fieldsAccept is fields plus the accepted keys it ignored, in stable order.
func fieldsAccept(obj map[string]any, path, allowed string, accept ...string) ([]string, error) {
	set := map[string]bool{}
	for _, k := range strings.Fields(allowed) {
		set[k] = true
	}
	accepted := map[string]bool{}
	for _, k := range accept {
		for _, name := range strings.Fields(k) {
			accepted[name] = true
		}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ignored []string
	for _, k := range keys {
		if set[k] {
			continue
		}
		if accepted["*"] || accepted[k] {
			ignored = append(ignored, k)
			continue
		}
		if path != "" {
			k = path + "." + k
		}
		return nil, invalid(k, "unsupported by the stateless Chat bridge")
	}
	return ignored, nil
}

func object(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, invalid(path, "object required")
	}
	return m, nil
}

func nonempty(v any, path string) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", invalid(path, "non-empty string required")
	}
	return s, nil
}

func stringValue(v any, path string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", invalid(path, "string required")
	}
	return s, nil
}

func optionalFalse(obj map[string]any, key string) error {
	if v, ok := obj[key]; ok && v != nil && v != false {
		return invalid(key, "only false is supported")
	}
	return nil
}

func sampling(src, dst map[string]any, maxTemperature float64) error {
	for _, k := range []string{"temperature", "top_p"} {
		v, exists := src[k]
		if !exists || v == nil {
			continue
		}
		n, ok := v.(json.Number)
		if !ok {
			return invalid(k, "number required")
		}
		f, err := n.Float64()
		max := 1.0
		if k == "temperature" {
			max = maxTemperature
		}
		if err != nil || f < 0 || f > max {
			return invalid(k, fmt.Sprintf("number in [0,%g] required", max))
		}
		dst[k] = v
	}
	return nil
}

// toolResultText allows an explicitly empty result array without weakening the
// message-content contract. Keep [] distinct from a missing/null result and "".
func toolResultText(v any, path, blockType string) (any, error) {
	if a, ok := v.([]any); ok && len(a) == 0 {
		return []any{}, nil
	}
	return textParts(v, path, blockType)
}

func textParts(v any, path string, allowedTypes ...string) (any, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	a, ok := v.([]any)
	if !ok || len(a) == 0 {
		return nil, invalid(path, "string or non-empty text content array required")
	}
	out := make([]any, 0, len(a))
	for i, v := range a {
		p := fmt.Sprintf("%s[%d]", path, i)
		m, err := object(v, p)
		if err != nil {
			return nil, err
		}
		typ, _ := m["type"].(string)
		supported := false
		for _, allowedType := range allowedTypes {
			if typ == allowedType {
				supported = true
				break
			}
		}
		if !supported {
			return nil, invalid(p+".type", "only "+strings.Join(allowedTypes, " or ")+" is supported")
		}
		part, err := textPartFrom(m, p, typ)
		if err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}

// Decode delegates native Chat unchanged. Cross-protocol requests use an explicit
// allowlist, preserving numeric values and checking the resulting tool history
// with the exact same validator as native Chat.
//
// 解码后按入口默认（或 media.tool_images 显式配置）执行工具结果图片策略：
// passthrough 零改动，hoist 抽出图片并在工具批次后插入 user 图片消息（改写标记
// 写入 Request.Mutated，供 encodedRequest 决定是否重新序列化），reject 明确 400。
func Decode(kind Kind, raw []byte) (*Request, error) {
	if kind == Chat {
		chat, err := forwarding.Parse(raw)
		if err != nil {
			return nil, err
		}
		mutated, err := media.ApplyToolPolicy(chat.Object, media.ToolPolicyFor(false))
		if err != nil {
			return nil, mediaError(err)
		}
		return &Request{Kind: kind, Chat: chat, Mutated: mutated}, nil
	}
	src, err := jsondoc.Object(raw)
	if err != nil {
		return nil, invalid("body", err.Error())
	}
	var dst map[string]any
	var tools *toolIndex
	var dropped []string
	switch kind {
	case Responses:
		tools = newToolIndex()
		dst, err = responsesRequest(src, tools, &dropped)
	case Anthropic:
		dst, err = anthropicRequest(src, &dropped)
	default:
		return nil, invalid("protocol", "unsupported protocol")
	}
	if err != nil {
		return nil, err
	}
	if choice := dst["tool_choice"]; choice == "required" {
		if tools, _ := dst["tools"].([]any); len(tools) == 0 {
			return nil, invalid("tool_choice", "required/any requires at least one tool")
		}
	}
	encoded, err := json.Marshal(dst)
	if err != nil {
		return nil, err
	}
	chat, err := forwarding.Parse(encoded)
	if err != nil {
		return nil, err
	}
	mutated, err := media.ApplyToolPolicy(chat.Object, media.ToolPolicyFor(true))
	if err != nil {
		return nil, mediaError(err)
	}
	return &Request{Kind: kind, Chat: chat, Source: src, Mutated: mutated, tools: tools, Dropped: dropped}, nil
}

func function(name, description, schema any, path string) (map[string]any, error) {
	n, err := nonempty(name, path+".name")
	if err != nil {
		return nil, err
	}
	params, err := object(schema, path+".parameters")
	if err != nil {
		return nil, err
	}
	if params["type"] != "object" {
		return nil, invalid(path+".parameters.type", "object schema required")
	}
	fn := map[string]any{"name": n, "parameters": params, "strict": false}
	if description != nil {
		d, err := stringValue(description, path+".description")
		if err != nil {
			return nil, err
		}
		fn["description"] = d
	}
	return map[string]any{"type": "function", "function": fn}, nil
}

func toolCall(id, name, args any, path string) (map[string]any, error) {
	cid, err := nonempty(id, path+".call_id")
	if err != nil {
		return nil, err
	}
	n, err := nonempty(name, path+".name")
	if err != nil {
		return nil, err
	}
	s, err := stringValue(args, path+".arguments")
	if err != nil {
		return nil, err
	}
	if _, err = jsondoc.Object([]byte(s)); err != nil {
		return nil, invalid(path+".arguments", "JSON object string required")
	}
	return map[string]any{"id": cid, "type": "function", "function": map[string]any{"name": n, "arguments": s}}, nil
}
