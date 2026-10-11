package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/reqlog"
)

func TestToolSelectionEnforcementRouting(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"none", "required", "forced-wrong", "parallel", "allowed"} {
				t.Run(fmt.Sprint(path, stream, scenario), func(t *testing.T) {
					attempts := 0
					up := newFakeUpstream(t, func(string) (int, string, bool) {
						attempts++
						calls := `{"index":0,"id":"a","type":"function","function":{"name":"lookup","arguments":"{}"}}`
						if scenario == "parallel" {
							calls += `,{"index":1,"id":"b","type":"function","function":{"name":"lookup","arguments":"{}"}}`
						}
						end := `data: {"choices":[{"index":0,"delta":{"tool_calls":[` + calls + `]},"finish_reason":"tool_calls"}]}`
						if scenario == "required" {
							end = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
						}
						return 200, `data: {"choices":[{"index":0,"delta":{"content":"checking"}}]}` + "\n\n" + end + "\n\n" +
							`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}` + "\n\ndata: [DONE]\n\n", true
					})
					body := map[string]any{"model": "cn:glm-5.2", "stream": stream}
					var tools []any
					for _, name := range []string{"lookup", "other"} {
						if path == "/v1/responses" {
							tools = append(tools, map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}, "strict": false})
						} else {
							tools = append(tools, map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}})
						}
					}
					body["tools"] = tools
					choice := "auto"
					if scenario == "none" || scenario == "required" {
						choice = scenario
					}
					if path == "/v1/responses" {
						body["store"], body["input"], body["tool_choice"] = false, "hi", choice
						if scenario == "forced-wrong" {
							body["tool_choice"] = map[string]any{"type": "function", "name": "other"}
						}
						if scenario == "parallel" {
							body["parallel_tool_calls"] = false
						}
					} else {
						body["max_tokens"], body["messages"] = 100, []any{map[string]any{"role": "user", "content": "hi"}}
						if choice == "required" {
							choice = "any"
						}
						tc := map[string]any{"type": choice}
						if scenario == "forced-wrong" {
							tc["type"], tc["name"] = "tool", "other"
						}
						if scenario == "parallel" {
							tc["disable_parallel_tool_use"] = true
						}
						body["tool_choice"] = tc
					}
					encoded, _ := json.Marshal(body)
					req := httptest.NewRequest("POST", path, strings.NewReader(string(encoded)))
					req.Header.Set("Anthropic-Version", "2023-06-01")
					logs := reqlog.New(reqlog.Config{})
					h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999}, &auth.Auth{UID: "b", AccessToken: "b", ExpiresAt: 9999999999}), Upstream: up, RequestLog: logs})
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					success := scenario == "allowed"
					if attempts != 1 || !stream && !success && rec.Code != 502 || success && rec.Code != 200 {
						t.Fatal(attempts, rec.Code, rec.Body)
					}
					if !success {
						for _, marker := range []string{"response.completed", "message_stop", "response.function_call_arguments", `"type":"tool_use"`, `"type":"function_call"`} {
							if strings.Contains(rec.Body.String(), marker) {
								t.Fatal("forbidden tool delivered", rec.Body)
							}
						}
					}
					entries := logs.Snapshot().Recent
					if len(entries) != 1 || entries[0].OK != success || entries[0].Attempts != 1 || entries[0].PromptTokens != 5 || entries[0].CompletionTokens != 3 {
						t.Fatal("incorrect accounting", entries)
					}
				})
			}
		}
	}
}

func TestUnsupportedNamespacesFailBeforeUpstream(t *testing.T) {
	attempts := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) { attempts++; return 200, "", false })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a", AccessToken: "a", ExpiresAt: 9999999999}), Upstream: up})
	for _, group := range []string{
		`{"type":"namespace","name":"crm","description":"","tools":[]}`,
		`{"type":"namespace","name":"crm","description":"","tools":[{"type":"custom","name":"f"}]}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"x","store":false,"input":"hi","tools":[`+group+`]}`)))
		if rec.Code != 400 || attempts != 0 {
			t.Fatal("unsupported declaration reached upstream", rec.Code, attempts)
		}
	}
}
