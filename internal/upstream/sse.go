// SSE decoding and completion state are shared by streaming and aggregation.
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy_manager/internal/jsondoc"
)

const maxSSEEvent = 8 << 20
const maxAggregate = 64 << 20

var errEmptyStream = errors.New("upstream stream contained no completion choices")
var errTruncatedStream = errors.New("upstream stream ended without all choice finish reasons")

func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

type FrameError struct {
	Payload string
	Kind    ErrKind
	Status  int
}

func (e *FrameError) Error() string { return e.Payload }

// DownstreamWriteError distinguishes delivery failure from upstream read failure.
type DownstreamWriteError struct{ Err error }

func (e *DownstreamWriteError) Error() string { return e.Err.Error() }
func (e *DownstreamWriteError) Unwrap() error { return e.Err }

type sseEvent struct{ Data, Event, ID, Retry string }
type eventReader struct{ br *bufio.Reader }

func (r *eventReader) next() (sseEvent, error) {
	var e sseEvent
	var data []string
	size := 0
	for {
		// ReadSlice bounds allocation even for a hostile stream without newline.
		var line []byte
		for {
			p, err := r.br.ReadSlice('\n')
			size += len(p)
			if size > maxSSEEvent {
				return e, fmt.Errorf("SSE event exceeds %d bytes", maxSSEEvent)
			}
			line = append(line, p...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if err != io.EOF {
					return e, fmt.Errorf("read upstream: %w", err)
				}
				if len(line) > 0 || len(data) > 0 {
					return e, errTruncatedStream
				}
				return e, io.EOF
			}
			break
		}
		s := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		if s == "" {
			if len(data) == 0 {
				e = sseEvent{}
				size = 0
				continue
			}
			e.Data = strings.Join(data, "\n")
			return e, nil
		}
		if strings.HasPrefix(s, ":") {
			continue
		}
		k, v, _ := strings.Cut(s, ":")
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "data":
			data = append(data, v)
		case "event":
			e.Event = v
		case "id":
			if !strings.ContainsRune(v, 0) {
				e.ID = v
			}
		case "retry":
			e.Retry = v
		}
	}
}

type completionState struct {
	top                 map[string]any
	choices             map[int]*choiceState
	expected            int
	aggregate           bool
	bytes               int
	emptyToolIdentity   bool
	declaredToolNames   map[string]bool
	dialectToolNames    bool
	legacyFunctionCalls bool
}
type choiceState struct {
	fields              map[string]any
	message             map[string]any
	tools               map[int]map[string]any
	finish              string
	emptyToolIdentity   bool
	declaredToolNames   map[string]bool
	dialectToolNames    bool
	legacyFunctionCalls bool
	toolNames           map[int]*toolNameState
	messageStrings      map[string]*strings.Builder
	toolStrings         map[int]map[string]*strings.Builder
}

// legacyToolIndex separates the pre-tool_calls single call from modern call
// indices (validated to stay below 128) so both can never merge into one entry.
const legacyToolIndex = 1000

func newCompletion(n int, aggregate bool) *completionState {
	if n <= 0 {
		n = 1
	}
	return &completionState{top: map[string]any{}, choices: map[int]*choiceState{}, expected: n, aggregate: aggregate}
}

func mergeStable(dst, src map[string]any, ignore map[string]bool, strict bool) error {
	for k, v := range src {
		if ignore[k] {
			continue
		}
		old, has := dst[k]
		if !has || old == nil {
			dst[k] = v
			continue
		}
		if v == nil || reflect.DeepEqual(old, v) {
			continue
		}
		a, aok := old.(map[string]any)
		b, bok := v.(map[string]any)
		if aok && bok {
			if err := mergeStable(a, b, nil, strict); err != nil {
				return err
			}
			continue
		}
		if strict {
			return fmt.Errorf("response_not_aggregatable: conflicting field %s", k)
		}
	}
	return nil
}

// appendString keeps a growing buffer instead of copying the entire prefix on
// every frame. Builders stay behind pointers (a non-zero Builder must not be
// copied). String() exposes an immutable prefix without a second allocation;
// subsequent appends never overwrite bytes already visible to maps/observers.
func appendString(dst map[string]any, buffers map[string]*strings.Builder, k string, v any) error {
	if v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("invalid string delta %s", k)
	}
	b := buffers[k]
	if b == nil {
		b = &strings.Builder{}
		old, _ := dst[k].(string)
		b.WriteString(old)
		buffers[k] = b
	}
	b.WriteString(s)
	dst[k] = b.String()
	return nil
}

// contentText flattens the text shapes a Chat message may use (plain string or
// text-part array) for translating consumers.
func contentText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []any:
		var b strings.Builder
		for _, part := range x {
			switch p := part.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String(), true
	default:
		return "", false
	}
}
func mergeList(dst map[string]any, k string, v any) error {
	if v == nil {
		return nil
	}
	a, ok := v.([]any)
	if !ok {
		return fmt.Errorf("invalid array delta %s", k)
	}
	old, _ := dst[k].([]any)
	dst[k] = append(old, a...)
	return nil
}
func (c *choiceState) mergeMessage(m map[string]any, snapshot, strict bool) error {
	if snapshot { // Full messages are snapshots, never a second copy of deltas.
		for k, v := range m {
			if c.legacyFunctionCalls {
				// Translating consumers need the text/tool subset only: fold the
				// legacy call, keep fields this bridge can represent, ignore the
				// rest, and let a snapshot supersede earlier deltas instead of
				// failing on a benign re-send.
				switch k {
				case "function_call":
					// A snapshot supersedes earlier deltas: reset the folded call so
					// its name/arguments are not appended to what already arrived.
					delete(c.tools, legacyToolIndex)
					delete(c.toolStrings, legacyToolIndex)
					if err := c.mergeLegacyFunctionCall(v); err != nil {
						return err
					}
					continue
				case "content":
					// Translating consumers deliver text only: flatten text-part
					// arrays so a later delta seeds from the snapshot text instead of
					// losing it.
					if s, ok := contentText(v); ok {
						c.message[k] = s
					} else {
						delete(c.message, k)
					}
					delete(c.messageStrings, k)
					continue
				case "reasoning", "reasoning_content", "refusal", "annotations", "tool_calls":
					c.message[k] = v
					delete(c.messageStrings, k)
					continue
				default:
					continue
				}
			}
			if old, ok := c.message[k]; ok && old != nil && old != "" && !reflect.DeepEqual(old, v) {
				return fmt.Errorf("conflicting message snapshot field %s", k)
			}
			c.message[k] = v
			// A snapshot may replace empty/null fields. Re-seed any later
			// delta from that exact snapshot, not a stale buffer.
			delete(c.messageStrings, k)
		}
		if c.legacyFunctionCalls && c.modernCalls() {
			// A snapshot that carries modern calls supersedes any folded legacy
			// call; the same call must never be delivered twice.
			delete(c.tools, legacyToolIndex)
			delete(c.toolStrings, legacyToolIndex)
		}
		return nil
	}
	for _, k := range []string{"content", "reasoning", "reasoning_content", "refusal"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		if c.legacyFunctionCalls {
			// Translating consumers deliver text only: a text-part array is
			// flattened, and a value with no text meaning is ignored instead of
			// failing the turn.
			if k == "content" {
				if s, ok := contentText(v); ok {
					if err := appendString(c.message, c.messageStrings, k, s); err != nil {
						return err
					}
				}
				continue
			}
			if s, ok := v.(string); ok {
				if err := appendString(c.message, c.messageStrings, k, s); err != nil {
					return err
				}
			}
			continue
		}
		if err := appendString(c.message, c.messageStrings, k, v); err != nil {
			return err
		}
	}
	if v, ok := m["annotations"]; ok {
		if a, isArray := v.([]any); isArray || !c.legacyFunctionCalls {
			if err := mergeList(c.message, "annotations", a); err != nil {
				return err
			}
		}
		// Translating consumers ignore a malformed annotations value instead of
		// failing the turn; the destination protocols would discard it anyway.
	}
	if raw, ok := m["tool_calls"]; ok && raw != nil {
		calls, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("invalid tool_calls array")
		}
		for _, v := range calls {
			d, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid tool call delta")
			}
			idx := -1
			if n, ok := jsondoc.Int(d["index"]); ok && n >= 0 && n < 128 {
				idx = int(n)
			} else if _, present := d["index"]; present {
				return fmt.Errorf("invalid tool index")
			}
			id, _ := d["id"].(string)
			if idx < 0 && id != "" {
				for i, t := range c.tools {
					if t["id"] == id {
						idx = i
						break
					}
				}
				if idx < 0 {
					idx = len(c.tools)
					for c.tools[idx] != nil {
						idx++
					}
				}
			}
			if idx < 0 {
				// The legacy pseudo-index is not a modern call: count only real
				// call slots so one indexed-less delta still resolves uniquely.
				modern, n := -1, 0
				for i := range c.tools {
					if i == legacyToolIndex {
						continue
					}
					modern, n = i, n+1
				}
				if n != 1 {
					return fmt.Errorf("ambiguous tool delta without index or id")
				}
				idx = modern
			}
			t := c.tools[idx]
			if t == nil {
				t = map[string]any{}
				c.tools[idx] = t
			}
			identity := d
			if c.emptyToolIdentity && (d["id"] == "" || d["type"] == "") {
				// Some OpenAI-compatible endpoints send empty identity strings
				// on continuation chunks. An explicit index identifies the call;
				// empty strings contain no new identity and must not erase it.
				// Non-empty conflicts still fail in mergeStable below.
				identity = make(map[string]any, len(d))
				for k, v := range d {
					if (k == "id" || k == "type") && v == "" {
						continue
					}
					identity[k] = v
				}
			}
			if err := mergeStable(t, identity, map[string]bool{"index": true, "function": true}, true); err != nil {
				return err
			}
			if raw, ok := d["function"]; ok {
				fn, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid tool function")
				}
				to, _ := t["function"].(map[string]any)
				if to == nil {
					to = map[string]any{}
					t["function"] = to
				}
				if c.toolStrings[idx] == nil {
					c.toolStrings[idx] = map[string]*strings.Builder{}
				}
				// Native adapter follows incremental name semantics, including delayed names.
				for _, k := range []string{"name", "arguments"} {
					if v, ok := fn[k]; ok {
						if k == "name" && (c.declaredToolNames != nil || c.dialectToolNames) {
							if c.toolNames[idx] == nil {
								c.toolNames[idx] = &toolNameState{declared: c.declaredToolNames}
							}
							if err := c.toolNames[idx].add(v); err != nil {
								return err
							}
						} else if err := appendString(to, c.toolStrings[idx], k, v); err != nil {
							return err
						}
					}
				}
				if err := mergeStable(to, fn, map[string]bool{"name": true, "arguments": true}, strict); err != nil {
					return err
				}
			}
		}
	}
	if raw, ok := m["function_call"]; ok && raw != nil && c.legacyFunctionCalls {
		// Skip a legacy echo when the same frame (or an earlier one) already
		// delivered modern tool calls.
		if !c.modernCalls() {
			if err := c.mergeLegacyFunctionCall(raw); err != nil {
				return err
			}
		}
	}
	ignore := map[string]bool{"content": true, "reasoning": true, "reasoning_content": true, "refusal": true, "annotations": true, "tool_calls": true}
	if c.legacyFunctionCalls {
		// Foreign message fields have no representation in the destination
		// protocols; ignoring them keeps a chatty upstream from failing the
		// turn on a conflict the client would never see.
		for k := range m {
			ignore[k] = true
		}
	}
	return mergeStable(c.message, m, ignore, strict)
}

// modernCalls reports whether the choice already carries modern tool calls,
// either as deltas (tools map) or from a message snapshot.
func (c *choiceState) modernCalls() bool {
	if calls, ok := c.message["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	for i := range c.tools {
		if i != legacyToolIndex {
			return true
		}
	}
	return false
}

// mergeLegacyFunctionCall folds the pre-tool_calls `function_call` field into
// the tool array (at a dedicated index) so translated responses can deliver it
// as a regular function call. Name chunks follow the common single-shot /
// cumulative / repeated dialects; arguments append. An all-empty placeholder is
// ignored rather than turned into an unusable call.
func (c *choiceState) mergeLegacyFunctionCall(raw any) error {
	fn, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid function_call")
	}
	name, _ := fn["name"].(string)
	args, _ := fn["arguments"].(string)
	if name == "" && args == "" {
		return nil
	}
	if fn["name"] != nil {
		if _, ok := fn["name"].(string); !ok {
			return fmt.Errorf("invalid function_call name")
		}
	}
	if fn["arguments"] != nil {
		if _, ok := fn["arguments"].(string); !ok {
			return fmt.Errorf("invalid function_call arguments")
		}
	}
	t := c.tools[legacyToolIndex]
	if t == nil {
		t = map[string]any{"id": "call_legacy", "type": "function"}
		c.tools[legacyToolIndex] = t
	}
	to, _ := t["function"].(map[string]any)
	if to == nil {
		to = map[string]any{}
		t["function"] = to
	}
	if c.toolStrings[legacyToolIndex] == nil {
		c.toolStrings[legacyToolIndex] = map[string]*strings.Builder{}
	}
	if name != "" {
		cur, _ := to["name"].(string)
		switch {
		case cur == "" || name == cur || strings.HasPrefix(name, cur):
			to["name"] = name
		default:
			to["name"] = cur + name
		}
	}
	if fn["arguments"] != nil {
		if err := appendString(to, c.toolStrings[legacyToolIndex], "arguments", fn["arguments"]); err != nil {
			return err
		}
	}
	return nil
}
func (s *completionState) add(obj map[string]any) error {
	for _, k := range []string{"id", "model"} {
		if v, exists := obj[k]; exists && v != nil {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("invalid completion %s", k)
			}
		}
		if old, ok := s.top[k]; ok && obj[k] != nil && obj[k] != "" && old != obj[k] {
			return fmt.Errorf("completion %s changed mid-stream", k)
		}
	}
	if err := mergeStable(s.top, obj, map[string]bool{"choices": true, "usage": true, "object": true}, s.aggregate); err != nil {
		return err
	}
	if u, ok := obj["usage"]; ok && u != nil {
		s.top["usage"] = u
	}
	raw, exists := obj["choices"]
	if !exists {
		return fmt.Errorf("upstream event has no choices")
	}
	choices, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("invalid choices array")
	}
	seen := map[int]bool{}
	for _, v := range choices {
		ch, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid choice")
		}
		n, ok := jsondoc.Int(ch["index"])
		if !ok || n < 0 || n >= int64(s.expected) || seen[int(n)] {
			return fmt.Errorf("invalid or duplicate choice index")
		}
		i := int(n)
		seen[i] = true
		c := s.choices[i]
		if c == nil {
			c = &choiceState{fields: map[string]any{"index": i}, message: map[string]any{}, tools: map[int]map[string]any{}, emptyToolIdentity: s.emptyToolIdentity,
				declaredToolNames: s.declaredToolNames, dialectToolNames: s.dialectToolNames, legacyFunctionCalls: s.legacyFunctionCalls, toolNames: map[int]*toolNameState{},
				messageStrings: map[string]*strings.Builder{}, toolStrings: map[int]map[string]*strings.Builder{}}
			s.choices[i] = c
		}
		if c.finish != "" {
			return fmt.Errorf("choice emitted after finish")
		}
		if d, has := ch["delta"]; has {
			m, ok := d.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid choice delta")
			}
			if err := c.mergeMessage(m, false, s.aggregate); err != nil {
				return err
			}
		}
		if d, has := ch["message"]; has {
			m, ok := d.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid choice message")
			}
			if err := c.mergeMessage(m, true, s.aggregate); err != nil {
				return err
			}
		}
		if raw, has := ch["logprobs"]; has && raw != nil && !s.legacyFunctionCalls {
			m, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid logprobs")
			}
			to, _ := c.fields["logprobs"].(map[string]any)
			if to == nil {
				to = map[string]any{}
				c.fields["logprobs"] = to
			}
			for _, k := range []string{"content", "refusal"} {
				if v, ok := m[k]; ok {
					if err := mergeList(to, k, v); err != nil {
						return err
					}
				}
			}
			if err := mergeStable(to, m, map[string]bool{"content": true, "refusal": true}, s.aggregate); err != nil {
				return err
			}
		}
		if err := mergeStable(c.fields, ch, map[string]bool{"index": true, "delta": true, "message": true, "finish_reason": true, "logprobs": true}, s.aggregate); err != nil {
			return err
		}
		if f, ok := ch["finish_reason"]; ok && f != nil {
			str, ok := f.(string)
			if !ok {
				return fmt.Errorf("invalid finish_reason")
			}
			c.finish = str
		}
		if c.finish == "tool_calls" {
			if err := c.validateTools(); err != nil {
				return err
			}
		} else if c.legacyFunctionCalls && c.finish == "function_call" && len(c.tools) > 0 {
			// The legacy terminal label maps onto the modern tool validation.
			if err := c.validateTools(); err != nil {
				return err
			}
		} else if len(c.toolNames) > 0 && (c.finish == "length" || c.finish == "content_filter") {
			// Resolve only observed complete names for translating consumers.
			// The protocol layer decides whether partial arguments can be represented.
			if err := c.resolveToolNames(); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c *choiceState) resolveToolNames() error {
	for index, state := range c.toolNames {
		name, err := state.name()
		if err != nil {
			return err
		}
		c.tools[index]["function"].(map[string]any)["name"] = name
	}
	return nil
}
func (c *choiceState) validateTools() error {
	if c.legacyFunctionCalls && c.modernCalls() {
		// The modern call is authoritative; validate it without the legacy echo.
		delete(c.tools, legacyToolIndex)
		delete(c.toolStrings, legacyToolIndex)
	}
	if err := c.resolveToolNames(); err != nil {
		return err
	}
	calls := []any{}
	if len(c.tools) > 0 {
		for _, t := range c.tools {
			calls = append(calls, t)
		}
	} else {
		calls, _ = c.message["tool_calls"].([]any)
	}
	if len(calls) == 0 {
		return fmt.Errorf("tool_calls finish without calls")
	}
	for _, v := range calls {
		t, _ := v.(map[string]any)
		fn, _ := t["function"].(map[string]any)
		name, _ := fn["name"].(string)
		id, _ := t["id"].(string)
		args, ok := fn["arguments"].(string)
		if name == "" || id == "" || !ok {
			return fmt.Errorf("incomplete tool call")
		}
		if c.declaredToolNames != nil && !c.declaredToolNames[name] {
			return fmt.Errorf("upstream tool name does not match declared tools")
		}
		if _, err := jsondoc.Decode([]byte(args)); err != nil {
			return fmt.Errorf("invalid completed tool arguments: %w", err)
		}
	}
	return nil
}
func (s *completionState) complete() error {
	if len(s.choices) == 0 {
		return errEmptyStream
	}
	if len(s.choices) != s.expected {
		return errTruncatedStream
	}
	for _, c := range s.choices {
		if c.finish == "" {
			return errTruncatedStream
		}
	}
	return nil
}
func (s *completionState) response() map[string]any {
	out := s.top
	out["object"] = "chat.completion"
	if out["id"] == nil || out["id"] == "" {
		out["id"] = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if out["created"] == nil {
		out["created"] = time.Now().Unix()
	}
	ids := []int{}
	for i := range s.choices {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	chs := []any{}
	for _, i := range ids {
		c := s.choices[i]
		if _, ok := c.message["role"]; !ok {
			c.message["role"] = "assistant"
		}
		if _, ok := c.message["content"]; !ok {
			c.message["content"] = nil
		}
		if c.legacyFunctionCalls && c.modernCalls() {
			// Modern calls are authoritative; a legacy echo must not be delivered
			// as a second call or clobber a snapshot's call array.
			delete(c.tools, legacyToolIndex)
			delete(c.toolStrings, legacyToolIndex)
		}
		if len(c.tools) > 0 {
			idxs := []int{}
			for j := range c.tools {
				idxs = append(idxs, j)
			}
			sort.Ints(idxs)
			a := []any{}
			for _, j := range idxs {
				a = append(a, c.tools[j])
			}
			c.message["tool_calls"] = a
		}
		c.fields["message"] = c.message
		finish := c.finish
		if c.legacyFunctionCalls && finish == "stop" && c.tools[legacyToolIndex] != nil {
			// A folded legacy call is a tool turn even when the upstream labelled
			// the terminal frame stop.
			finish = "tool_calls"
		}
		c.fields["finish_reason"] = finish
		chs = append(chs, c.fields)
	}
	out["choices"] = chs
	if u, ok := out["usage"].(map[string]any); ok {
		out["usage"] = ensureUsageTotal(u)
	}
	return out
}
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	p, ok := jsondoc.Int(u["prompt_tokens"])
	c, yes := jsondoc.Int(u["completion_tokens"])
	if ok && yes && p >= 0 && c >= 0 && p <= int64(^uint64(0)>>1)-c {
		out := map[string]any{}
		for k, v := range u {
			out[k] = v
		}
		out["total_tokens"] = p + c
		return out
	}
	return u
}

// errorFrame uses the same classification for HTTP errors and stream errors.
func errorFrame(obj map[string]any, payload, event string) *FrameError {
	_, has := obj["error"]
	code, codePresent := obj["code"]
	business := codePresent && code != nil && fmt.Sprint(code) != "0"
	// bizCode：官方桌面端在终帧用 bizCode 报业务码（capacity-queue 读的就是它，
	// 见 5.6.2 客户端 readCapacityQueueErrorFromTerminal）。不认它 → 队列帧被当成
	// 普通帧透传，客户端拿到一段无法解析的 fragment。仍以 code 为主：两者都出现时
	// 取 code，避免重复判定。
	if !business {
		if biz, ok := obj["bizCode"]; ok && biz != nil && fmt.Sprint(biz) != "0" {
			code, business = biz, true
		}
	}
	if !has && !business && event != "error" {
		return nil
	}
	kind := Classify(400, payload)
	if hasBusinessCode(payload, "6004") {
		kind = ErrSoftRate
	}
	return &FrameError{payload, kind, ErrorStatus(kind)}
}
func ErrorStatus(k ErrKind) int {
	switch k {
	case ErrSoftRate, ErrModelQueued:
		// 排队是**可重试**语义：429 + Retry-After（上游明示则用之）。
		return 429
	case ErrBadParams, ErrClient, ErrImageInvalid, ErrPromptTooLong, ErrContentBlocked, ErrModelBlocked:
		return 400
	case ErrHardCredit, ErrAccountFault, ErrSessionDead:
		return 503
	default:
		return 502
	}
}

// StreamOption 是响应管线的可选行为开关。WithErrorFrameObserver / WithFrameObserver
// 只做旁路观测、不改透传字节；WithMarkupRepair 会改写出站帧——把正文里的原生工具
// 调用标记还原成 tool_calls（§15 式显式可见的近似变换，见 dsml.go）。
type StreamOption func(*streamOptions)
type streamOptions struct {
	onErrorFrame        func(string)
	onFrame             func(map[string]any)
	markup              *markupStream
	expected            int
	firstEvent          time.Duration
	firstGeneration     time.Duration
	tail                time.Duration
	emptyToolIdentity   bool
	declaredToolNames   map[string]bool
	dialectToolNames    bool
	legacyFunctionCalls bool
}

func WithErrorFrameObserver(fn func(string)) StreamOption {
	return func(o *streamOptions) { o.onErrorFrame = fn }
}
func WithFrameObserver(fn func(map[string]any)) StreamOption {
	return func(o *streamOptions) { o.onFrame = fn }
}
func WithExpectedChoices(n int) StreamOption { return func(o *streamOptions) { o.expected = n } }

// WithEmptyToolIdentityDeltas opts a translating consumer into treating empty
// identity fields as absent. No identity is invented and conflicting non-empty
// values or ambiguous deltas remain errors. Native Chat stays unchanged.
func WithEmptyToolIdentityDeltas() StreamOption {
	return func(o *streamOptions) { o.emptyToolIdentity = true }
}

// WithLegacyFunctionCalls opts a translating consumer into folding the legacy
// single-call `function_call` message field into the modern tool array. Native
// Chat never sets this option, so its raw field pass-through stays unchanged.
func WithLegacyFunctionCalls() StreamOption {
	return func(o *streamOptions) { o.legacyFunctionCalls = true }
}

// watchResponse closes a blocked upstream reader when semantic deadlines expire.
// Heartbeats do not extend these deadlines. All timer ownership ends with consume.
func watchResponse(r io.Reader, o streamOptions) (func(*completionState, map[string]any), func(), func() error) {
	closer, ok := r.(io.Closer)
	if !ok {
		return func(*completionState, map[string]any) {}, func() {}, func() error { return nil }
	}
	first, generation, tail := o.firstEvent, o.firstGeneration, o.tail
	if first <= 0 {
		first = 120 * time.Second
	}
	if generation <= 0 {
		generation = 300 * time.Second
	}
	if tail <= 0 {
		tail = 10 * time.Second
	}
	var mu sync.Mutex
	var failure error
	stopped := false
	expire := func(stage string) {
		mu.Lock()
		if stopped {
			mu.Unlock()
			return
		}
		failure = fmt.Errorf("upstream %s timeout", stage)
		stopped = true
		mu.Unlock()
		_ = closer.Close()
	}
	t1 := time.AfterFunc(first, func() { expire("first model event") })
	t2 := time.AfterFunc(generation, func() { expire("first generation") })
	var t3 *time.Timer
	note := func(s *completionState, obj map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		if len(s.choices) > 0 {
			t1.Stop()
		}
		for _, c := range s.choices {
			if c.finish != "" || len(c.tools) > 0 {
				t2.Stop()
			}
			for _, k := range []string{"content", "reasoning", "reasoning_content", "refusal"} {
				if v, ok := c.message[k].(string); ok && v != "" {
					t2.Stop()
				}
			}
		}
		if s.complete() == nil && t3 == nil {
			t3 = time.AfterFunc(tail, func() { expire("tail") })
		}
	}
	stop := func() {
		mu.Lock()
		stopped = true
		t1.Stop()
		t2.Stop()
		if t3 != nil {
			t3.Stop()
		}
		mu.Unlock()
	}
	cause := func() error { mu.Lock(); defer mu.Unlock(); return failure }
	return note, stop, cause
}

func WithResponseTimeouts(first, generation, tail time.Duration) StreamOption {
	return func(o *streamOptions) { o.firstEvent = first; o.firstGeneration = generation; o.tail = tail }
}

func consume(r io.Reader, s *completionState, emit func(sseEvent, map[string]any) error, o streamOptions) (result error) {
	note, stop, cause := watchResponse(r, o)
	defer func() {
		stop()
		if err := cause(); err != nil {
			result = err
		}
	}()
	br := bufio.NewReaderSize(r, 64*1024)
	// Native endpoints occasionally return a complete JSON response rather than SSE.
	for {
		p, err := br.Peek(1)
		if err != nil {
			if err == io.EOF {
				return errEmptyStream
			}
			return err
		}
		if p[0] == ' ' || p[0] == '\n' || p[0] == '\r' || p[0] == '\t' {
			_, _ = br.ReadByte()
			continue
		}
		break
	}
	first, _ := br.Peek(1)
	if first[0] == '{' || first[0] == '[' {
		raw, err := io.ReadAll(io.LimitReader(br, maxAggregate+1))
		if err != nil {
			return err
		}
		if len(raw) > maxAggregate {
			return fmt.Errorf("upstream response too large")
		}
		obj, err := jsondoc.Object(raw)
		if err != nil {
			return err
		}
		if e := errorFrame(obj, string(raw), ""); e != nil {
			if o.onErrorFrame != nil {
				o.onErrorFrame(e.Payload)
			}
			return e
		}
		if obj["object"] != "chat.completion" {
			return fmt.Errorf("unsupported upstream JSON response")
		}
		// 标记还原展开（见 dsml.go）：整包快照同样可能把工具调用吐成标记文本。
		// 展开出的调用帧 / 收尾帧是 delta 形态，与原快照帧同走 s.add 校验。
		frames := o.markup.expand(obj)
		for _, f := range frames {
			if derr := s.add(f); derr != nil {
				return derr
			}
			note(s, f)
			if o.onFrame != nil {
				o.onFrame(f)
			}
		}
		for _, f := range o.markup.tailFrames() {
			if derr := s.add(f); derr != nil {
				return derr
			}
			note(s, f)
			if o.onFrame != nil {
				o.onFrame(f)
			}
			frames = append(frames, f)
		}
		if err = s.complete(); err != nil {
			return err
		}
		if emit != nil {
			for _, f := range frames {
				for _, v := range f["choices"].([]any) {
					ch, ok := v.(map[string]any)
					if !ok {
						continue
					}
					if ch["message"] != nil {
						ch["delta"] = ch["message"]
						delete(ch, "message")
					}
				}
				f["object"] = "chat.completion.chunk"
				if derr := emit(sseEvent{}, f); derr != nil {
					return derr
				}
			}
			return nil
		}
		return nil
	}
	parser := eventReader{br: br}
	type buffered struct {
		event sseEvent
		obj   map[string]any
	}
	var prelude []buffered
	// deliver 把一帧送进聚合状态并按序透出（含 prelude 续传与已透出正文的回收）。
	// 标记还原（WithMarkupRepair）展开出的正文帧 / 调用帧 / 收尾帧与流尾兜底帧
	// 都走这里：合成帧不绕过任何既有校验（index 唯一、finish 顺序、tool_calls
	// 完整性），与非合成帧同一条路径。
	deliver := func(e sseEvent, f map[string]any) error {
		if derr := s.add(f); derr != nil {
			return derr
		}
		note(s, f)
		if o.onFrame != nil {
			o.onFrame(f)
		}
		if emit != nil {
			if len(s.choices) == 0 {
				prelude = append(prelude, buffered{e, f})
				return nil
			}
			for _, b := range prelude {
				if derr := emit(b.event, b.obj); derr != nil {
					return derr
				}
			}
			prelude = nil
			if derr := emit(e, f); derr != nil {
				return derr
			}
			// Native Chat can discard delivered text; translating consumers retain
			// bounded state for their terminal response object.
			if !s.aggregate {
				for _, c := range s.choices {
					for _, k := range []string{"content", "reasoning", "reasoning_content", "refusal", "annotations"} {
						delete(c.message, k)
						delete(c.messageStrings, k)
					}
					delete(c.fields, "logprobs")
				}
			}
		}
		return nil
	}
	// flushTail 在流收尾前把标记还原层仍持有的未判定字节原文交还（绝不吞字节）。
	flushTail := func() error {
		for _, f := range o.markup.tailFrames() {
			if derr := deliver(sseEvent{}, f); derr != nil {
				return derr
			}
		}
		return nil
	}
	for {
		e, err := parser.next()
		if err == io.EOF {
			if derr := flushTail(); derr != nil {
				return derr
			}
			return s.complete()
		}
		if err != nil {
			return err
		}
		if e.Data == "[DONE]" {
			if derr := flushTail(); derr != nil {
				return derr
			}
			return s.complete()
		}
		obj, err := jsondoc.Object([]byte(e.Data))
		if err != nil {
			return fmt.Errorf("invalid upstream event: %w", err)
		}
		if fe := errorFrame(obj, e.Data, e.Event); fe != nil {
			if o.onErrorFrame != nil {
				o.onErrorFrame(e.Data)
			}
			return fe
		}
		s.bytes += len(e.Data)
		if s.bytes > maxAggregate {
			return fmt.Errorf("completion exceeds bounded state limit")
		}
		for fi, f := range o.markup.expand(obj) {
			ev := e
			if fi > 0 {
				ev = sseEvent{} // 合成帧不携带原事件的 event/id 行
			}
			if derr := deliver(ev, f); derr != nil {
				return derr
			}
		}
	}
}

// ConsumeCompletion shares the native decoder, limits, observers, deadlines and
// success predicate with Aggregate/StreamHint. emit sees validated Chat chunks
// (including JSON snapshots converted to a chunk). It must not mutate them or
// acknowledge completion before this function returns successfully.
func ConsumeCompletion(r io.Reader, emit func(map[string]any) error, opts ...StreamOption) (map[string]any, error) {
	o := streamOptions{expected: 1}
	for _, f := range opts {
		f(&o)
	}
	s := newCompletion(o.expected, true)
	s.emptyToolIdentity = o.emptyToolIdentity
	s.declaredToolNames = o.declaredToolNames
	s.legacyFunctionCalls = o.legacyFunctionCalls
	s.dialectToolNames = o.dialectToolNames
	err := consume(r, s, func(_ sseEvent, obj map[string]any) error {
		if emit == nil {
			return nil
		}
		return emit(obj)
	}, o)
	if err != nil {
		return nil, err
	}
	return s.response(), nil
}

func Aggregate(r io.Reader, opts ...StreamOption) (map[string]any, error) {
	o := streamOptions{expected: 1}
	for _, f := range opts {
		f(&o)
	}
	s := newCompletion(o.expected, true)
	s.legacyFunctionCalls = o.legacyFunctionCalls
	s.dialectToolNames = o.dialectToolNames
	if err := consume(r, s, nil, o); err != nil {
		return nil, err
	}
	return s.response(), nil
}
func Stream(w http.ResponseWriter, r io.Reader) error { return StreamHint(w, r, nil) }
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string, opts ...StreamOption) error {
	o := streamOptions{expected: 1}
	for _, f := range opts {
		f(&o)
	}
	committed := false
	write := func(e sseEvent, obj map[string]any) error {
		if !committed {
			h := w.Header()
			h.Set("Content-Type", "text/event-stream")
			h.Set("Cache-Control", "no-cache")
			h.Set("X-Accel-Buffering", "no")
			committed = true
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		var b strings.Builder
		if e.Event != "" {
			fmt.Fprintf(&b, "event: %s\n", e.Event)
		}
		if e.ID != "" {
			fmt.Fprintf(&b, "id: %s\n", e.ID)
		}
		fmt.Fprintf(&b, "data: %s\n\n", raw)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err = io.WriteString(w, b.String()); err != nil {
			return &DownstreamWriteError{err}
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	s := newCompletion(o.expected, false)
	s.emptyToolIdentity = o.emptyToolIdentity
	s.declaredToolNames = o.declaredToolNames
	s.legacyFunctionCalls = o.legacyFunctionCalls
	s.dialectToolNames = o.dialectToolNames
	err := consume(r, s, write, o)
	var we *DownstreamWriteError
	if errors.As(err, &we) {
		return err
	}
	if err != nil {
		status := 502
		obj := map[string]any{"error": map[string]any{"type": "upstream_error", "code": "upstream_protocol_error", "message": err.Error()}}
		var fe *FrameError
		if errors.As(err, &fe) {
			status = fe.Status
			obj = ErrorResponse(fe.Kind, fe.Payload)
			if hint := frameGatewayHint(hintFn, fe.Payload); hint != "" {
				obj["error"].(map[string]any)["gateway_hint"] = hint
			}
		}
		if !committed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if e := json.NewEncoder(w).Encode(obj); e != nil {
				return &DownstreamWriteError{e}
			}
			return err
		}
		if e := write(sseEvent{}, obj); e != nil {
			return e
		}
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, e := io.WriteString(w, "data: [DONE]\n\n"); e != nil {
		return &DownstreamWriteError{e}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return err
}
func ErrorResponse(kind ErrKind, payload string) map[string]any {
	e := map[string]any{"type": "upstream_error", "code": kind.String(), "message": "upstream rejected the request"}
	if obj, err := jsondoc.Object([]byte(payload)); err == nil {
		if inner, ok := obj["error"].(map[string]any); ok {
			obj = inner
		}
		safe := map[string]any{}
		for _, k := range []string{"code", "message", "msg", "type", "param", "requestId", "request_id"} {
			if v, ok := obj[k]; ok {
				safe[k] = v
			}
		}
		e["upstream"] = safe
		if v, ok := obj["message"].(string); ok {
			e["message"] = v
		} else if v, ok := obj["msg"].(string); ok {
			e["message"] = v
		}
	} // Non-JSON error bodies are not reflected (may contain credentials/HTML).
	return map[string]any{"error": e}
}
func frameGatewayHint(fn func(string) string, p string) (out string) {
	if fn == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	return strings.TrimSpace(fn(p))
}
func attachHintToErrorFrame(payload, hint string) string {
	m, err := jsondoc.Object([]byte(payload))
	if err != nil {
		return payload
	}
	e, ok := m["error"].(map[string]any)
	if !ok {
		return payload
	}
	e["gateway_hint"] = hint
	b, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return string(b)
}
