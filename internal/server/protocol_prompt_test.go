package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/forwarding"
	"workbuddy_manager/internal/jsondoc"
	"workbuddy_manager/internal/prompt"
	"workbuddy_manager/internal/protocol"
	"workbuddy_manager/internal/scrub"
	"workbuddy_manager/internal/upstream"
)

func promptProtocolRequest(t *testing.T, kind protocol.Kind) []byte {
	t.Helper()
	schema := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer", "minimum": json.Number("9007199254740993")}, "command": map[string]any{"type": "string"}}, "required": []any{"n", "command"}, "additionalProperties": false}
	args := ` {"n":9007199254740993,"command":"echo 11128\\path\n你好"} `
	base := map[string]any{"model": "cn:glm-5.2", "stream": false}
	if kind != protocol.Anthropic {
		base["parallel_tool_calls"] = false
	}
	tool := map[string]any{"name": "lookup", "description": "Keep 11128 and original tool instructions", "parameters": schema}
	call := map[string]any{"id": "call", "type": "function", "function": map[string]any{"name": "lookup", "arguments": args}}
	switch kind {
	case protocol.Chat:
		base["tools"] = []any{map[string]any{"type": "function", "function": tool}}
		base["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}
		base["messages"] = []any{
			map[string]any{"role": "system", "content": "CLIENT instructions"},
			map[string]any{"role": "developer", "content": "DEV environment and permissions"},
			map[string]any{"role": "user", "content": "USER project rules"},
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{call}},
			map[string]any{"role": "tool", "tool_call_id": "call", "content": " result 11128\n"},
			map[string]any{"role": "developer", "content": "MID turn rule"},
			map[string]any{"role": "user", "content": "continue"},
		}
	case protocol.Responses:
		tool["type"], tool["strict"] = "function", false
		base["tools"] = []any{tool}
		base["tool_choice"] = map[string]any{"type": "function", "name": "lookup"}
		base["store"], base["instructions"] = false, "CLIENT instructions"
		base["input"] = []any{
			map[string]any{"role": "developer", "content": "DEV environment and permissions"},
			map[string]any{"role": "user", "content": "USER project rules"},
			map[string]any{"role": "assistant", "content": "checking"},
			map[string]any{"type": "function_call", "call_id": "call", "name": "lookup", "arguments": args},
			map[string]any{"type": "function_call_output", "call_id": "call", "output": " result 11128\n"},
			map[string]any{"role": "developer", "content": "MID turn rule"},
			map[string]any{"role": "user", "content": "continue"},
		}
	case protocol.Anthropic:
		base["system"], base["max_tokens"] = "CLIENT instructions", 100
		base["tools"] = []any{map[string]any{"name": "lookup", "description": tool["description"], "input_schema": schema}}
		base["tool_choice"] = map[string]any{"type": "tool", "name": "lookup", "disable_parallel_tool_use": true}
		input, err := jsondoc.Object([]byte(args))
		if err != nil {
			t.Fatal(err)
		}
		base["messages"] = []any{
			map[string]any{"role": "user", "content": "USER project rules"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "checking"}, map[string]any{"type": "tool_use", "id": "call", "name": "lookup", "input": input}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call", "content": " result 11128\n"}, map[string]any{"type": "text", "text": "continue"}}},
		}
	}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProtocolPromptCompositionPreservesToolContract(t *testing.T) {
	for kind, path := range map[protocol.Kind]string{protocol.Chat: "/v1/chat/completions", protocol.Responses: "/v1/responses", protocol.Anthropic: "/v1/messages"} {
		for _, mode := range []string{prompt.ModeNone, prompt.ModeReplace, prompt.ModeAfter, prompt.ModeAppend} {
			for _, fingerprint := range []bool{false, true} {
				t.Run(string(kind)+"/"+mode+map[bool]string{false: "/exact", true: "/fingerprint"}[fingerprint], func(t *testing.T) {
					raw := promptProtocolRequest(t, kind)
					decoded, err := protocol.Decode(kind, raw)
					if err != nil {
						t.Fatal(err)
					}
					baselineRaw, err := decoded.Chat.Encode("glm-5.2")
					if err != nil {
						t.Fatal(err)
					}
					baseline, _ := jsondoc.Object(baselineRaw)
					var sent map[string]any
					attempts := 0
					up := &upstream.Client{ChatBaseCN: "https://prompt-test.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						attempts++
						body, err := io.ReadAll(r.Body)
						if err == nil {
							sent, err = jsondoc.Object(body)
						}
						if err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"next","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"))}, nil
					})}}
					up.Fingerprints.Store(scrub.NewLayer(fingerprint, nil))
					h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "prompt", AccessToken: "fake", ExpiresAt: 9999999999}), Upstream: up, PromptRules: map[string]prompt.Rule{"": {Mode: mode, Text: "GATEWAY"}}})
					req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
					req.Header.Set("Anthropic-Version", "2023-06-01")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != 200 || attempts != 1 {
						t.Fatal(rec.Code, rec.Body, attempts)
					}
					expectedTools := baseline["tools"]
					if fingerprint {
						expObj, _ := jsondoc.Object(baselineRaw)
						scrub.Object(expObj)
						expectedTools = expObj["tools"]
					}
					if !reflect.DeepEqual(sent["tools"], expectedTools) {
						t.Fatalf("tools changed: %#v / %#v", sent["tools"], expectedTools)
					}
					for _, key := range []string{"tool_choice", "parallel_tool_calls"} {
						if !reflect.DeepEqual(sent[key], baseline[key]) {
							t.Fatalf("%s changed: %#v / %#v", key, sent[key], baseline[key])
						}
					}
					var originalRules, actualRules, originalHistory, actualHistory []any
					for _, item := range baseline["messages"].([]any) {
						m := item.(map[string]any)
						if m["role"] == "system" || m["role"] == "developer" {
							originalRules = append(originalRules, m["content"])
						} else {
							originalHistory = append(originalHistory, m)
						}
					}
					for _, item := range sent["messages"].([]any) {
						m := item.(map[string]any)
						if m["role"] == "system" || m["role"] == "developer" {
							actualRules = append(actualRules, m["content"])
						} else {
							actualHistory = append(actualHistory, m)
						}
					}
					if mode == prompt.ModeReplace {
						if !reflect.DeepEqual(actualRules, []any{"GATEWAY"}) {
							t.Fatal("replace did not remove every system/developer rule", actualRules)
						}
					} else {
						var clientRules []any
						for _, rule := range actualRules {
							if rule != "GATEWAY" {
								clientRules = append(clientRules, rule)
							}
						}
						if !reflect.DeepEqual(clientRules, originalRules) {
							t.Fatal("client rules reordered or removed", actualRules)
						}
					}
					if fingerprint {
						// Explicitly separate lossy fingerprint rewriting from prompt
						// composition. It alters historical arguments AND tool results.
						if reflect.DeepEqual(originalHistory, actualHistory) {
							t.Fatal("fixture did not exercise fingerprint rewriting")
						}
						b, _ := json.Marshal(originalHistory)
						v, _ := jsondoc.Decode([]byte(strings.ReplaceAll(string(b), "11128", "11-128")))
						originalHistory = v.([]any)
					}
					if !reflect.DeepEqual(originalHistory, actualHistory) {
						t.Fatalf("tool/project history changed: %#v / %#v", originalHistory, actualHistory)
					}
					// Check insertion positions and message roles, not merely the
					// set of surviving client rule strings.
					msgs := baseline["messages"].([]any)
					prefix := 0
					for prefix < len(msgs) {
						role := msgs[prefix].(map[string]any)["role"]
						if role != "system" && role != "developer" {
							break
						}
						prefix++
					}
					gateway := map[string]any{"role": "system", "content": "GATEWAY"}
					var want []any
					switch mode {
					case prompt.ModeNone:
						want = msgs
					case prompt.ModeReplace:
						want = append([]any{gateway}, originalHistory...)
					case prompt.ModeAfter:
						want = append([]any{gateway}, msgs...)
					case prompt.ModeAppend:
						want = append(want, msgs[:prefix]...)
						want = append(want, gateway)
						want = append(want, msgs[prefix:]...)
					}
					if fingerprint {
						b, _ := json.Marshal(want)
						v, _ := jsondoc.Decode([]byte(strings.ReplaceAll(string(b), "11128", "11-128")))
						want = v.([]any)
					}
					if !reflect.DeepEqual(sent["messages"], want) {
						t.Fatalf("incorrect composition: %#v / %#v", sent["messages"], want)
					}
					// The composed request must remain valid as a complete tool cycle.
					outbound, _ := json.Marshal(sent)
					if _, err := forwarding.Parse(outbound); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
