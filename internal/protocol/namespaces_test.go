package protocol

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy_manager/internal/jsondoc"
)

const nsFunction = `{"type":"function","name":"lookup","description":"exact child guidance","parameters":{"type":"object","properties":{"n":{"type":"integer","default":9007199254740993}}},"strict":false}`
const nsTools = `[{"type":"namespace","name":"crm","description":"","tools":[` + nsFunction + `]}]`

func nsRequest(t *testing.T, input string) *Request {
	t.Helper()
	return mustDecode(t, Responses, `{"model":"x","store":false,"input":`+input+`,"tools":`+nsTools+`}`)
}

func TestNamespaceDeclarationsChoiceAndHistory(t *testing.T) {
	r := nsRequest(t, `"hi"`)
	fn := r.Chat.Object["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "ns_3_crm_lookup" || fn["description"] != "exact child guidance" {
		t.Fatal(fn)
	}
	original, _ := jsondoc.Object([]byte(nsFunction))
	if !reflect.DeepEqual(fn["parameters"], original["parameters"]) {
		t.Fatal("schema changed", fn)
	}
	if r.Source["tools"].([]any)[0].(map[string]any)["type"] != "namespace" {
		t.Fatal("source rewritten", r.Source)
	}
	encoded, err := r.Chat.Encode("x")
	if err != nil || strings.Contains(string(encoded), `"namespace"`) || strings.Contains(string(encoded), `"input"`) {
		t.Fatal(string(encoded), err)
	}
	forced := mustDecode(t, Responses, `{"model":"x","store":false,"input":"hi","tools":`+nsTools+`,"tool_choice":{"type":"function","name":"lookup"}}`)
	if forced.Chat.Object["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "ns_3_crm_lookup" {
		t.Fatal(forced.Chat.Object)
	}
	history := `[{"role":"user","content":"hi"},{"type":"function_call","call_id":"a","name":"lookup","namespace":"crm","arguments":" {\"n\":9007199254740993} "},{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":" A\n"},{"type":"input_text","text":""},{"type":"input_text","text":"B "}]}]`
	r = nsRequest(t, history)
	msgs := r.Chat.Object["messages"].([]any)
	call := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "a" || call["function"].(map[string]any)["name"] != "ns_3_crm_lookup" || call["function"].(map[string]any)["arguments"] != ` {"n":9007199254740993} ` {
		t.Fatal(call)
	}
	want := []any{map[string]any{"type": "text", "text": " A\n"}, map[string]any{"type": "text", "text": ""}, map[string]any{"type": "text", "text": "B "}}
	if !reflect.DeepEqual(msgs[2].(map[string]any)["content"], want) {
		t.Fatal("tool result blocks changed", msgs)
	}
}

func TestNamespaceRejectedSemantics(t *testing.T) {
	for _, tools := range []string{
		strings.Replace(nsTools, `"name":"crm"`, `"name":"crm.x"`, 1),
		strings.Replace(nsTools, `"name":"crm"`, `"name":"中文"`, 1),
		strings.Replace(nsTools, `"name":"lookup"`, `"name":"`+strings.Repeat("x", 65)+`"`, 1),
		strings.Replace(nsTools, `"type":"function"`, `"type":"custom"`, 1),
		strings.Replace(nsTools, `"type":"function"`, `"type":"namespace"`, 1),
		strings.Replace(nsTools, nsFunction, "", 1),
		strings.Replace(nsTools, nsFunction, nsFunction+","+nsFunction, 1),
		"[" + nsTools[1:len(nsTools)-1] + "," + nsTools[1:len(nsTools)-1] + "]",
		"[" + nsTools[1:len(nsTools)-1] + "," + strings.Replace(nsFunction, `"lookup"`, `"ns_3_crm_lookup"`, 1) + "]",
	} {
		if _, err := Decode(Responses, []byte(`{"model":"x","store":false,"input":"hi","tools":`+tools+`}`)); err == nil {
			t.Fatal("unsupported declaration accepted", tools)
		}
	}
	for _, call := range []string{
		`"name":"lookup","namespace":"crm.x"`,
		`"name":"lookup","namespace":""`,
		`"name":"lookup","namespace":17`,
		`"name":"ns_3_crm_lookup"`,
	} {
		body := `{"model":"x","store":false,"tools":` + nsTools + `,"input":[{"type":"function_call","call_id":"a",` + call + `,"arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"ok"}]}`
		if _, err := Decode(Responses, []byte(body)); err == nil {
			t.Fatal("guessed historical namespace", body)
		}
	}
	for _, output := range []string{`null`, `[{"type":"input_image","image_url":"https://example.test/x"}]`, `[{"type":"output_text","text":"x"}]`, `[{"type":"input_text","text":"x","prompt_cache_breakpoint":{}}]`, `[{"type":"input_text","text":1}]`} {
		body := `{"model":"x","store":false,"input":[{"type":"function_call","call_id":"a","name":"old","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":` + output + `}]}`
		if _, err := Decode(Responses, []byte(body)); err == nil {
			t.Fatal("lost tool result data", body)
		}
	}
}

func TestNamespaceOptionalDescriptionAndStrict(t *testing.T) {
	// Omitting description/strict follows the official defaults; non-empty
	// descriptions, strict:true, defer_loading and async are accepted and dropped.
	for _, tools := range []string{
		strings.Replace(nsTools, `,"description":""`, ``, 1),
		strings.Replace(nsTools, `"description":""`, `"description":null`, 1),
		strings.Replace(nsTools, `"description":""`, `"description":"group rules"`, 1),
		strings.Replace(nsTools, `,"strict":false`, ``, 1),
		strings.Replace(nsTools, `"strict":false`, `"strict":true`, 1),
		strings.Replace(nsTools, `"strict":false`, `"defer_loading":true,"strict":false`, 1),
		strings.Replace(nsTools, `"strict":false`, `"async":false,"strict":false`, 1),
	} {
		r := mustDecode(t, Responses, `{"model":"x","store":false,"input":"hi","tools":`+tools+`}`)
		if r.tools.byChat["ns_3_crm_lookup"] != (toolIdentity{name: "lookup", namespace: "crm"}) {
			t.Fatal("alias missing", tools)
		}
	}
}

func TestNamespaceAmbiguityAndStableAliases(t *testing.T) {
	crm := nsTools[1 : len(nsTools)-1]
	fs := strings.Replace(crm, `"crm"`, `"fs"`, 1)
	for _, tools := range []string{"[" + crm + "," + fs + "]", "[" + fs + "," + crm + "]", "[" + nsFunction + "," + crm + "]"} {
		body := `{"model":"x","store":false,"input":"hi","tools":` + tools + `}`
		r := mustDecode(t, Responses, body)
		if r.tools.byIdentity[toolIdentity{name: "lookup", namespace: "crm"}] != "ns_3_crm_lookup" {
			t.Fatal("unstable alias", r.tools)
		}
		for _, name := range []string{"lookup", "ns_3_crm_lookup", "crm.lookup"} {
			forced := strings.TrimSuffix(body, "}") + `,"tool_choice":{"type":"function","name":"` + name + `"}}`
			if _, err := Decode(Responses, []byte(forced)); err == nil {
				t.Fatal("ambiguous or internal alias choice accepted", forced)
			}
		}
	}
	// Collisions must fail in both declaration orders, never first-wins.
	flatAlias := strings.Replace(nsFunction, `"lookup"`, `"ns_3_crm_lookup"`, 1)
	for _, tools := range []string{"[" + flatAlias + "," + crm + "]", "[" + crm + "," + flatAlias + "]"} {
		if _, err := Decode(Responses, []byte(`{"model":"x","store":false,"input":"hi","tools":`+tools+`}`)); err == nil {
			t.Fatal("order-dependent collision accepted", tools)
		}
	}
	alias, err := namespaceChatName("a", strings.Repeat("b", 57), "test")
	if err != nil || len(alias) != 64 {
		t.Fatal("64-byte namespace alias rejected", alias, err)
	}
	if long, err := namespaceChatName("a", strings.Repeat("b", 58), "test"); err != nil || len(long) > 64 || !strings.HasPrefix(long, "nsh_") {
		t.Fatal("overlong namespace alias not compacted", long, err)
	}
	// Delimiter-containing identities that naive namespace__name flattening
	// conflates must have different aliases.
	a, _ := namespaceChatName("a_b", "c", "test")
	b, _ := namespaceChatName("a", "b_c", "test")
	if a == b {
		t.Fatal("alias collision")
	}
}

func TestNamespaceOutputAndStatelessReplay(t *testing.T) {
	for _, finish := range []string{"tool_calls", "length"} {
		for _, chunks := range [][]any{{"ns_3_", "crm_", "lookup"}, {"ns_3_crm_lookup", "ns_3_crm_lookup"}, {"ns_3_crm_", "ns_3_crm_lookup"}} {
			raw := strings.Replace(nameChunks(chunks), `"finish_reason":"tool_calls"`, `"finish_reason":"`+finish+`"`, 1)
			req := nsRequest(t, `"hi"`)
			c, err := Aggregate(strings.NewReader(raw), req)
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Format(req)
			if err != nil {
				t.Fatal(err)
			}
			call := out["output"].([]any)[0].(map[string]any)
			if call["name"] != "lookup" || call["namespace"] != "crm" || call["call_id"] != "call_1" {
				t.Fatal(call)
			}
			rec := httptest.NewRecorder()
			if err := Stream(rec, strings.NewReader(raw), req, nil); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(rec.Body.String(), "ns_3_crm_lookup") {
				t.Fatal("internal alias leaked", rec.Body)
			}
			es := events(t, rec.Body.String(), Responses)
			for _, e := range es {
				if item, ok := e["item"].(map[string]any); ok {
					if item["namespace"] != "crm" || item["name"] != "lookup" {
						t.Fatal("lifecycle identity differs", e)
					}
				}
			}
			terminal := es[len(es)-1]["response"].(map[string]any)
			compareSemantic(t, terminal["output"], out["output"])
			// Simulate a fresh stateless second request; no cache from first turn.
			history := append([]any{}, out["output"].([]any)...)
			history = append(history, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{map[string]any{"type": "input_text", "text": "ok"}}})
			next := map[string]any{"model": "x", "store": false, "tools": req.Source["tools"], "input": history}
			encoded, _ := json.Marshal(next)
			r, err := Decode(Responses, encoded)
			if finish == "length" {
				if err == nil {
					t.Fatal("incomplete namespaced call replayed")
				}
			} else if err != nil || r.tools.byIdentity[toolIdentity{name: "lookup", namespace: "crm"}] != "ns_3_crm_lookup" {
				t.Fatal(err)
			}
		}
	}
	// A raw snapshot and native Chat need not know about namespace envelopes.
	raw := `{"object":"chat.completion","choices":[{"index":0,"message":{"tool_calls":[{"id":"a","type":"function","function":{"name":"ns_3_crm_lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	snapshot, _ := jsondoc.Object([]byte(raw))
	if out, err := Format(nsRequest(t, `"hi"`), snapshot); err != nil || out["output"].([]any)[0].(map[string]any)["namespace"] != "crm" {
		t.Fatal(out, err)
	}
}

func TestNamespacedHistoryWithoutRedeclaration(t *testing.T) {
	body := `{"model":"x","store":false,"input":[{"type":"function_call","call_id":"a","name":"lookup","namespace":"crm","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"ok"}]}`
	r := mustDecode(t, Responses, body)
	call := r.Chat.Object["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["name"] != "ns_3_crm_lookup" || len(declaredToolNames(r)) != 0 || len(r.tools.byChat) != 0 {
		t.Fatal("historical identity lost or granted current permission", r.Chat.Object)
	}
	// Native Chat extensions are neither remapped nor subjected to the
	// Responses namespace allowlist.
	native := mustDecode(t, Chat, `{"model":"x","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","namespace":"crm","parameters":{"type":"object"}}}]}`)
	fn := native.Chat.Object["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "lookup" || fn["namespace"] != "crm" || native.tools != nil {
		t.Fatal("native extensions altered", fn)
	}
}

func TestNamespaceForcedSelection(t *testing.T) {
	req := mustDecode(t, Responses, `{"model":"x","store":false,"input":"hi","tools":`+nsTools+`,"tool_choice":{"type":"function","name":"lookup"},"parallel_tool_calls":false}`)
	for _, name := range []string{"ns_3_crm_lookup", "lookup"} {
		raw := nameChunks([]any{name})
		c, err := Aggregate(strings.NewReader(raw), req)
		if err == nil {
			_, err = c.Format(req)
		}
		if (err == nil) != (name == "ns_3_crm_lookup") {
			t.Fatal("namespaced selection mismatch", name, err)
		}
	}
	// Identical local names in namespaces do not change plain historical calls
	// when an explicit flat declaration also exists.
	body := `{"model":"x","store":false,"tools":[` + nsTools[1:len(nsTools)-1] + `,` + nsFunction + `],"input":[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"ok"}]}`
	r := mustDecode(t, Responses, body)
	call := r.Chat.Object["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["name"] != "lookup" {
		t.Fatal("flat history reassigned to namespace", call)
	}
}

func FuzzNamespaceIdentity(f *testing.F) {
	f.Add("crm", "lookup")
	f.Add("a_b", "c")
	f.Add(strings.Repeat("a", 64), strings.Repeat("b", 64))
	f.Fuzz(func(t *testing.T, ns, name string) {
		alias, err := namespaceChatName(ns, name, "test")
		if err != nil {
			return
		}
		if len(alias) > 64 {
			t.Fatal(alias)
		}
		if strings.HasPrefix(alias, "ns_") {
			// Short form is length-delimited, not an underscore split.
			var n int
			if _, err := fmt.Sscanf(alias, "ns_%d_", &n); err != nil || n != len(ns) || !strings.HasSuffix(alias, ns+"_"+name) {
				t.Fatal(alias)
			}
		} else if !strings.HasPrefix(alias, "nsh_") || len(alias) != 47 {
			t.Fatal(alias)
		}
		tool, _ := jsondoc.Object([]byte(nsFunction))
		tool["name"] = name
		body, _ := json.Marshal(map[string]any{"model": "x", "store": false, "input": "hi", "tools": []any{map[string]any{"type": "namespace", "name": ns, "description": "", "tools": []any{tool}}}})
		r, err := Decode(Responses, body)
		if err != nil || r.tools.byChat[alias] != (toolIdentity{name: name, namespace: ns}) {
			t.Fatal(alias, err)
		}
		historyOnly := newToolIndex()
		got, err := historyOnly.history(name, ns, "history")
		if err != nil || got != alias || len(historyOnly.byChat) != 0 {
			t.Fatal("unstable or authorized history", got, err)
		}
	})
}
