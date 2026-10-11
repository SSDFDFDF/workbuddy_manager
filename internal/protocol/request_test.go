package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustDecode(t *testing.T, kind Kind, body string) *Request {
	t.Helper()
	r, err := Decode(kind, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRequestMapping(t *testing.T) {
	res := mustDecode(t, Responses, `{"model":"cn:test","store":false,"stream":true,"temperature":0,"parallel_tool_calls":false,"max_output_tokens":9007199254740993,"instructions":"rules","metadata":{"conversation_id":"not-routing"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer","default":9007199254740993}}},"strict":false}],"tool_choice":{"type":"function","name":"lookup"},"input":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"output_text","text":"checking","annotations":[],"logprobs":[]}]},{"type":"function_call","id":"fc_other","call_id":"a","name":"lookup","arguments":"{\"id\":9007199254740993}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"b","output":"second"},{"type":"function_call_output","call_id":"a","output":"first"}]}`)
	if res.Chat.ConversationID != "" {
		t.Fatal("client metadata became routing state")
	}
	msgs := res.Chat.Object["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages=%#v", msgs)
	}
	calls := msgs[2].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 || calls[0].(map[string]any)["id"] != "a" {
		t.Fatal(calls)
	}
	raw, _ := json.Marshal(res.Chat.Object)
	for _, want := range []string{`9007199254740993`, `"temperature":0`, `"parallel_tool_calls":false`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("lost %s: %s", want, raw)
		}
	}
	for _, foreign := range []string{"input", "instructions", "store", "metadata"} {
		if _, ok := res.Chat.Object[foreign]; ok {
			t.Fatalf("foreign field %s forwarded", foreign)
		}
	}
}

func TestAnthropicToolHistory(t *testing.T) {
	r := mustDecode(t, Anthropic, `{"model":"test","max_tokens":100,"system":[{"type":"text","text":"rules"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"any","disable_parallel_tool_use":true},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"check"},{"type":"tool_use","id":"a","name":"f","input":{"n":9007199254740993}},{"type":"tool_use","id":"b","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"B"},{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"A"}],"is_error":false},{"type":"text","text":"continue"}]}]}`)
	if r.Chat.Object["tool_choice"] != "required" || r.Chat.Object["parallel_tool_calls"] != false {
		t.Fatal(r.Chat.Object)
	}
	msgs := r.Chat.Object["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatal(msgs)
	}
	if msgs[3].(map[string]any)["tool_call_id"] != "b" || msgs[5].(map[string]any)["role"] != "user" {
		t.Fatal(msgs)
	}
}

func TestUnsupportedAndInvalidRequests(t *testing.T) {
	cases := []struct {
		kind        Kind
		body, param string
	}{
		{Responses, `{"model":"x","store":false,"input":"hi","previous_response_id":"resp_x"}`, "previous_response_id"},
		{Responses, `{"model":"x","store":false,"input":"hi","reasoning":{"effort":""}}`, "reasoning.effort"},
		{Responses, `{"model":"x","store":false,"input":"hi","reasoning":{"effort":7}}`, "reasoning.effort"},
		{Responses, `{"model":"x","store":false,"input":"hi","truncation":"always"}`, "truncation"},
		{Responses, `{"model":"x","store":false,"input":"hi","include":["reasoning.encrypted_content",3]}`, "include[1]"},
		{Responses, `{"model":"x","store":false,"input":"hi","text":{"verbosity":true}}`, "text.verbosity"},
		{Responses, `{"model":"x","store":false,"input":"hi","text":{"format":{"type":"json_object"}}}`, "text.format"},
		{Responses, `{"model":"x","store":false,"input":[{"role":"user","content":[{"type":"input_image","image_url":"http://x"}]}]}`, "input"},
		{Responses, `{"model":"x","store":false,"input":[{"type":"function_call_output","call_id":"missing","output":"ok"}]}`, "tool_call_id"},
		{Responses, `{"model":"x","store":false,"input":"hi","temperature":3}`, "temperature"},
		{Responses, `{"model":"x","model":"y","store":false,"input":"hi"}`, "duplicate"},
		{Anthropic, `{"model":"x","messages":[{"role":"user","content":"hi"}]}`, "max_tokens"},
		{Anthropic, `{"model":"x","max_tokens":1,"messages":[{"role":"system","content":"hi"}]}`, "role"},
		{Anthropic, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"always"}}`, "thinking.type"},
		{Anthropic, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":0}}`, "thinking.budget_tokens"},
		{Anthropic, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"stop_sequences":"END"}`, "stop_sequences"},
		{Anthropic, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"stop_sequences":["END",""]}`, "stop_sequences[1]"},
		{Anthropic, `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"bad","is_error":"yes"}]}]}`, "is_error"},
		{Anthropic, `{"model":"x","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}},{"type":"text","text":"after"}]}]}`, "order"},
	}
	for _, tt := range cases {
		t.Run(string(tt.kind)+"/"+tt.param, func(t *testing.T) {
			_, err := Decode(tt.kind, []byte(tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.param) {
				t.Fatalf("err=%v want %s", err, tt.param)
			}
		})
	}
}

func TestRelaxedDefaultsAccepted(t *testing.T) {
	// store:true, background:true, strict:true, unknown fields are accepted and dropped.
	r := mustDecode(t, Responses, `{"model":"x","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`)
	if tools := r.Chat.Object["tools"].([]any); len(tools) != 1 {
		t.Fatal(tools)
	}
	for _, body := range []string{
		`{"model":"x","store":true,"input":"hi"}`,
		`{"model":"x","store":false,"background":true,"input":"hi"}`,
		`{"model":"x","store":false,"input":"hi","unexpected":false,"n":2}`,
		`{"model":"x","store":false,"input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"strict":true}]}`,
	} {
		req, err := Decode(Responses, []byte(body))
		if err != nil {
			t.Fatalf("rejected %s: %v", body, err)
		}
		if len(req.Dropped) == 0 {
			t.Fatalf("expected dropped fields for %s", body)
		}
	}
}

func TestAnthropicAdvisoryFieldsDropped(t *testing.T) {
	// metadata and cache_control are accepted and dropped: they are client-side
	// telemetry / caching hints with no Chat upstream meaning.
	r := mustDecode(t, Anthropic, `{"model":"x","max_tokens":1,"metadata":{"user_id":"u"},`+
		`"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],`+
		`"tools":[{"name":"f","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	raw, _ := json.Marshal(r.Chat.Object)
	if strings.Contains(string(raw), "cache_control") || strings.Contains(string(raw), "metadata") {
		t.Fatalf("advisory fields forwarded: %s", raw)
	}
	if r.Chat.Object["messages"].([]any)[0].(map[string]any)["role"] != "system" {
		t.Fatal(r.Chat.Object)
	}
}

func TestClientHintsAndReasoningDropped(t *testing.T) {
	// Codex sends these on every Responses request; most have no Chat
	// representation, so they are accepted, ignored and reported in Dropped.
	// prompt_cache_key is the exception: it is hoisted into the Chat document
	// (session sticky key + upstream prefix cache, matching native Chat
	// passthrough where the client value always wins) and is therefore NOT in
	// Dropped.
	r := mustDecode(t, Responses, `{"model":"x","store":false,"input":"hi","service_tier":"priority",`+
		`"client_metadata":{"x":"1"},"prompt_cache_key":"k","stream_options":{"include_usage":true},`+
		`"safety_identifier":"s","top_logprobs":5,"include":["reasoning.encrypted_content"],`+
		`"truncation":"auto","text":{"verbosity":"low"},`+
		`"reasoning":{"effort":"high","summary":"auto","context":"current_turn"}}`)
	if r.Chat.Object["reasoning_effort"] != "high" {
		t.Fatal("effort not translated", r.Chat.Object)
	}
	if r.Chat.PromptCacheKey != "k" {
		t.Fatal("prompt_cache_key not hoisted to Chat request", r.Chat.PromptCacheKey)
	}
	raw, _ := json.Marshal(r.Chat.Object)
	for _, k := range []string{"service_tier", "client_metadata", "stream_options", "safety_identifier", "top_logprobs", "include", "truncation", "verbosity"} {
		if strings.Contains(string(raw), `"`+k) {
			t.Fatalf("%s forwarded: %s", k, raw)
		}
	}
	if strings.Contains(string(raw), `"reasoning":`) {
		t.Fatalf("reasoning object forwarded: %s", raw)
	}
	for _, k := range []string{"service_tier", "client_metadata", "stream_options", "safety_identifier", "top_logprobs", "include", "truncation", "text.verbosity", "reasoning.summary", "reasoning.context"} {
		if !containsString(r.Dropped, k) {
			t.Fatalf("dropped list missing %s: %v", k, r.Dropped)
		}
	}
	if containsString(r.Dropped, "prompt_cache_key") {
		t.Fatalf("prompt_cache_key should be adopted, not dropped: %v", r.Dropped)
	}
}

func TestResponsesReasoningHistoryDropped(t *testing.T) {
	r := mustDecode(t, Responses, `{"model":"x","store":false,"input":[`+
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hmm"}],"encrypted_content":"zzz"},`+
		`{"role":"user","content":"hi"}]}`)
	msgs := r.Chat.Object["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatal(msgs)
	}
	if !containsString(r.Dropped, "input[0]") {
		t.Fatalf("dropped list missing reasoning item: %v", r.Dropped)
	}
}

func TestUnknownInputAndBlockTypesDropped(t *testing.T) {
	resp := mustDecode(t, Responses, `{"model":"x","store":true,"input":[{"type":"item_reference","id":"ref_1"},{"role":"user","content":"hi"}]}`)
	if !containsString(resp.Dropped, "input[0]") || !containsString(resp.Dropped, "store") {
		t.Fatalf("expected dropped unknown input/store: %v", resp.Dropped)
	}

	ant := mustDecode(t, Anthropic, `{"model":"x","max_tokens":1,"custom_field":"val",`+
		`"tools":[{"name":"f","input_schema":{"type":"object"},"strict":true,"defer_loading":true}],`+
		`"messages":[{"role":"user","content":[{"type":"custom_block","data":"x"},{"type":"text","text":"hi"}]}]}`)
	if !containsString(ant.Dropped, "custom_field") || !containsString(ant.Dropped, "tools[0].strict") ||
		!containsString(ant.Dropped, "tools[0].defer_loading") || !containsString(ant.Dropped, "messages[0].content[0]") {
		t.Fatalf("expected dropped anthropic fields: %v", ant.Dropped)
	}
}

func TestAnthropicThinkingAcceptedAndDropped(t *testing.T) {
	r := mustDecode(t, Anthropic, `{"model":"x","max_tokens":10,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[`+
		`{"role":"user","content":"hi"},`+
		`{"role":"assistant","content":[`+
		`{"type":"thinking","thinking":"hmm","signature":"sig"},`+
		`{"type":"redacted_thinking","data":"opaque"},`+
		`{"type":"tool_use","id":"a","name":"f","input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"boom","is_error":true}]}]}`)
	if _, exists := r.Chat.Object["thinking"]; exists {
		t.Fatal("thinking forwarded", r.Chat.Object)
	}
	msgs := r.Chat.Object["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatal(msgs)
	}
	if calls, _ := msgs[1].(map[string]any)["tool_calls"].([]any); len(calls) != 1 {
		t.Fatal(msgs[1])
	}
	if got := msgs[2].(map[string]any)["content"]; got != "[tool error] boom" {
		t.Fatal(got)
	}
	for _, k := range []string{"thinking", "messages[1].content[0]", "messages[1].content[1]"} {
		if !containsString(r.Dropped, k) {
			t.Fatalf("dropped list missing %s: %v", k, r.Dropped)
		}
	}
}

// pi（claude-opus-5 的 forceAdaptiveThinking / supportsMidConvoEffort 路径）发的是
// thinking:{type:"adaptive",display:"summarized"[,block_binding:…]} + output_config:{effort}。
// 这三样此前都会 400：思考开关无法被假装满足是事实，但让真实客户端直接用不了 Messages
// 入口不是——状态丢掉、effort 映射到 Chat reasoning_effort，并在 Dropped 里记账。
func TestAnthropicAdaptiveThinkingAndOutputConfig(t *testing.T) {
	r := mustDecode(t, Anthropic, `{"model":"x","max_tokens":10,"thinking":{"type":"adaptive","display":"summarized",`+
		`"block_binding":{"prefix_mismatch_behavior":"drop_block"}},"output_config":{"effort":"high"},`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if _, exists := r.Chat.Object["thinking"]; exists {
		t.Fatal("adaptive thinking forwarded", r.Chat.Object)
	}
	if got := r.Chat.Object["reasoning_effort"]; got != "high" {
		t.Fatalf("output_config.effort not mapped: %v", got)
	}
	for _, k := range []string{"thinking", "thinking.display", "thinking.block_binding"} {
		if !containsString(r.Dropped, k) {
			t.Fatalf("dropped list missing %s: %v", k, r.Dropped)
		}
	}
	// enabled + display（pi 的普通 thinking 路径）同样不得因为 display 被拒。
	r = mustDecode(t, Anthropic, `{"model":"x","max_tokens":10,"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"},"messages":[{"role":"user","content":"hi"}]}`)
	if !containsString(r.Dropped, "thinking.display") {
		t.Fatalf("display not reported: %v", r.Dropped)
	}
	// output_config.format（结构化输出）仍必须明确拒绝：丢掉它等于假装支持。
	if _, err := Decode(Anthropic, []byte(`{"model":"x","max_tokens":10,"output_config":{"format":{"type":"json_schema"}},"messages":[{"role":"user","content":"hi"}]}`)); err == nil || !strings.Contains(err.Error(), "output_config") {
		t.Fatalf("err=%v want output_config", err)
	}
}

// 越界档位不再在桥接层 400：合法集随 realm/模型变化（同一模型名 CN 与 global 可不同），
// 桥接层没有这份信息，归一交由出站钳位（upstream.ClampEffort）。
func TestReasoningEffortValuesPassThrough(t *testing.T) {
	for _, eff := range []string{"none", "off", "minimal", "low", "medium", "high", "xhigh", "max", "extreme"} {
		r := mustDecode(t, Responses, `{"model":"x","store":false,"input":"hi","reasoning":{"effort":"`+eff+`"}}`)
		if got := r.Chat.Object["reasoning_effort"]; got != eff {
			t.Fatalf("effort=%s forwarded %v", eff, got)
		}
	}
}

func TestAnthropicToolErrorMarker(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          any
	}{
		{"string", `,"content":"boom"`, "[tool error] boom"},
		{"empty-string", `,"content":""`, "[tool error]"},
		{"omitted", ``, "[tool error]"},
		{"empty-array", `,"content":[]`, []any{map[string]any{"type": "text", "text": "[tool error]"}}},
		{"text-array", `,"content":[{"type":"text","text":"detail"}]`, []any{
			map[string]any{"type": "text", "text": "[tool error]"},
			map[string]any{"type": "text", "text": "detail"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"x","max_tokens":10,"messages":[` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","is_error":true` + tc.content + `}]}]}`
			r := mustDecode(t, Anthropic, body)
			got := r.Chat.Object["messages"].([]any)[1].(map[string]any)["content"]
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("content=%#v want %#v", got, tc.want)
			}
		})
	}
	ok := mustDecode(t, Anthropic, `{"model":"x","max_tokens":10,"messages":[`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","is_error":false,"content":"fine"}]}]}`)
	if got := ok.Chat.Object["messages"].([]any)[1].(map[string]any)["content"]; got != "fine" {
		t.Fatal(got)
	}
}

func TestAnthropicStopSequencesForwarded(t *testing.T) {
	r := mustDecode(t, Anthropic, `{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stop_sequences":["</block>","END"]}`)
	stop, ok := r.Chat.Object["stop"].([]any)
	if !ok || len(stop) != 2 || stop[0] != "</block>" || stop[1] != "END" {
		t.Fatal(r.Chat.Object)
	}
	empty := mustDecode(t, Anthropic, `{"model":"x","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stop_sequences":[]}`)
	if _, exists := empty.Chat.Object["stop"]; exists {
		t.Fatal(empty.Chat.Object)
	}
}

func TestResponsesMessagePhaseAcceptedAndDropped(t *testing.T) {
	r := mustDecode(t, Responses, `{"model":"x","store":false,"input":[`+
		`{"role":"assistant","phase":"final_answer","content":"done"},`+
		`{"role":"user","content":"ok"}]}`)
	msgs := r.Chat.Object["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "assistant" {
		t.Fatal(msgs)
	}
	if !containsString(r.Dropped, "input[0].phase") {
		t.Fatalf("dropped list missing phase: %v", r.Dropped)
	}
	if _, err := Decode(Responses, []byte(`{"model":"x","store":false,"input":[{"role":"assistant","phase":7,"content":"done"}]}`)); err == nil || !strings.Contains(err.Error(), "phase") {
		t.Fatalf("non-string phase accepted: %v", err)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestResponsesAssistantTextForms(t *testing.T) {
	for _, content := range []string{
		`"你好"`,
		`[{"type":"input_text","text":"你好"}]`,
		`[{"type":"output_text","text":"你好","annotations":[],"logprobs":[]}]`,
		`[{"type":"input_text","text":"你"},{"type":"output_text","text":"好"}]`,
	} {
		r := mustDecode(t, Responses, `{"model":"x","store":false,"input":[{"role":"user","content":"hi"},{"role":"assistant","content":`+content+`},{"role":"user","content":"continue"}]}`)
		msgs := r.Chat.Object["messages"].([]any)
		if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
			t.Fatal(msgs)
		}
		v := msgs[1].(map[string]any)["content"]
		var text string
		switch v := v.(type) {
		case string:
			text = v
		case []any:
			for _, part := range v {
				p := part.(map[string]any)
				if p["type"] != "text" {
					t.Fatal(p)
				}
				text += p["text"].(string)
			}
		}
		if text != "你好" {
			t.Fatal(text)
		}
	}
	for _, item := range []string{
		`{"role":"user","content":[{"type":"output_text","text":"hi"}]}`,
		`{"role":"assistant","content":[{"type":"input_text","text":"hi","annotations":[]}]}`,
		`{"role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[{"url":"x"}]}]}`,
		`{"role":"assistant","content":[{"type":"input_image","image_url":"https://example.com/x.png"}]}`,
		`{"role":"assistant","content":[{"type":"input_text","text":42}]}`,
		`{"role":"assistant","content":[{"type":"input_text","text":"hi","cache_control":{"type":"ephemeral"}}]}`,
	} {
		if _, err := Decode(Responses, []byte(`{"model":"x","store":false,"input":[`+item+`]}`)); err == nil {
			t.Fatal("lost unsupported history information", item)
		}
	}
}

func TestNativeRequestStillPreservesExtensions(t *testing.T) {
	r := mustDecode(t, Chat, `{"model":"x","messages":[{"role":"user","content":"hi"}],"custom":{"n":9007199254740993}}`)
	if r.Chat.Object["custom"] == nil {
		t.Fatal("native extension lost")
	}
}

func FuzzRequestDecode(f *testing.F) {
	f.Add(`{"model":"x","store":false,"input":"hi"}`)
	f.Add(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	f.Add(`{"model":"x","store":false,"input":[{"role":"assistant","content":[{"type":"input_text","text":"hi"},{"type":"output_text","text":"there","annotations":[]}]}]}`)
	f.Add(`{"model":"x","store":false,"input":"hi","tools":` + nsTools + `}`)
	f.Add(`{"model":"x","store":false,"input":[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"ok"}]}]}`)
	f.Add(`{"model":"x","store":false,"input":[{"type":"function_call","call_id":"a","name":"f","namespace":"retired","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":[]}]}`)
	f.Add(`{"model":"x","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[]}]}]}`)
	f.Add(`{"model":"x","store":false,"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]}`)
	f.Add(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`)
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 1<<16 {
			t.Skip()
		}
		for _, kind := range []Kind{Responses, Anthropic} {
			_, _ = Decode(kind, []byte(body))
		}
	})
}
