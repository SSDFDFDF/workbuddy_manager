package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const thinkingHistory = `{"model":"glm-5.3","max_tokens":32,"thinking":{"type":"disabled"},"messages":[
{"role":"user","content":"hi"},
{"role":"assistant","content":[
{"type":"thinking","thinking":"  思考\n","signature":"private-signature"},
{"type":"text","text":"visible"},
{"type":"thinking","thinking":"\tsecond  "},
{"type":"redacted_thinking","data":"private-opaque"},
{"type":"tool_use","id":"call1","name":"f","input":{"n":9007199254740993}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"result"}]}]}`

func TestThinkingReplayPlaintextHistory(t *testing.T) {
	const wantText = "  思考\n\tsecond  "
	for _, enabled := range []bool{false, true} {
		r, err := DecodeWithOptions(Anthropic, []byte(thinkingHistory), DecodeOptions{ReplayThinking: enabled})
		if err != nil {
			t.Fatal(err)
		}
		wantStats := ThinkingReplayStats{Enabled: enabled, PlainBlocks: 2, RedactedBlocks: 1}
		if enabled {
			wantStats.ReplayedBlocks, wantStats.ReplayedBytes = 2, len(wantText)
		}
		if r.Thinking != wantStats {
			t.Fatalf("enabled=%v stats=%+v want=%+v", enabled, r.Thinking, wantStats)
		}
		msgs := r.Chat.Object["messages"].([]any)
		if len(msgs) != 3 {
			t.Fatal(msgs)
		}
		assistant := msgs[1].(map[string]any)
		if enabled {
			for _, field := range []string{"reasoning", "reasoning_content"} {
				if assistant[field] != wantText {
					t.Fatalf("%s text changed: %q", field, assistant[field])
				}
			}
		} else if _, exists := assistant["reasoning"]; exists {
			t.Fatal("replay must default off")
		} else if _, exists := assistant["reasoning_content"]; exists {
			t.Fatal("replay must default off")
		}
		for _, i := range []string{"0", "2"} {
			if containsString(r.Dropped, "messages[1].content["+i+"]:thinking") == enabled {
				t.Fatal("replayed blocks must not be reported dropped", r.Dropped)
			}
		}
		if !containsString(r.Dropped, "messages[1].content[3]:redacted_thinking") {
			t.Fatal(r.Dropped)
		}
		calls := assistant["tool_calls"].([]any)
		args := calls[0].(map[string]any)["function"].(map[string]any)["arguments"]
		if args != `{"n":9007199254740993}` || msgs[2].(map[string]any)["role"] != "tool" {
			t.Fatal("tool history changed", msgs)
		}
		if r.Chat.Object["thinking"].(map[string]any)["type"] != "disabled" {
			t.Fatal("history replay changed generation controls")
		}
		encoded, err := json.Marshal(r.Chat.Object)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-signature", "private-opaque"} {
			if strings.Contains(string(encoded), secret) || strings.Contains(strings.Join(r.Dropped, " "), secret) {
				t.Fatal("signature/opaque data escaped translation")
			}
		}
	}
	legacy, err := Decode(Anthropic, []byte(thinkingHistory))
	if err != nil {
		t.Fatal(err)
	}
	off, err := DecodeWithOptions(Anthropic, []byte(thinkingHistory), DecodeOptions{})
	if err != nil || !reflect.DeepEqual(legacy, off) {
		t.Fatal("default Decode changed", err)
	}
}

func TestThinkingReplayEdgeBlocks(t *testing.T) {
	for _, tc := range []struct {
		name, blocks, want string
		plain, redacted    int
	}{
		{"only-thinking", `{"type":"thinking","thinking":"x"}`, "x", 1, 0},
		{"whitespace", `{"type":"thinking","thinking":" \n\t"}`, " \n\t", 1, 0},
		{"empty", `{"type":"thinking","thinking":""}`, "", 0, 0},
		{"missing", `{"type":"thinking"}`, "", 0, 0},
		{"wrong-type", `{"type":"thinking","thinking":{"text":"not-plaintext"}}`, "", 0, 0},
		{"opaque", `{"type":"redacted_thinking","thinking":"not-plaintext","data":"opaque"}`, "", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[` + tc.blocks + `]}]}`
			r, err := DecodeWithOptions(Anthropic, []byte(body), DecodeOptions{ReplayThinking: true})
			if err != nil {
				t.Fatal(err)
			}
			want := ThinkingReplayStats{Enabled: true, PlainBlocks: tc.plain, ReplayedBlocks: tc.plain, ReplayedBytes: len(tc.want), RedactedBlocks: tc.redacted}
			if r.Thinking != want {
				t.Fatalf("stats=%+v want=%+v", r.Thinking, want)
			}
			msgs := r.Chat.Object["messages"].([]any)
			if tc.want == "" {
				if len(msgs) != 1 || len(r.Dropped) != 1 {
					t.Fatal(msgs, r.Dropped)
				}
			} else if len(msgs) != 2 {
				t.Fatal(msgs)
			} else if got := msgs[1].(map[string]any); got["reasoning"] != tc.want || got["reasoning_content"] != tc.want || got["content"] != nil {
				t.Fatal(got)
			}
		})
	}
	for _, enabled := range []bool{false, true} {
		_, err := DecodeWithOptions(Anthropic, []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"thinking","thinking":"bad-role"}]}]}`), DecodeOptions{ReplayThinking: enabled})
		if err == nil {
			t.Fatal("thinking under user must remain invalid")
		}
	}
}

// 多个 assistant 消息各自独立回放；工具轮次保持配对合法，出站编码不丢字段。
func TestThinkingReplayMultipleAssistantMessages(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[
{"role":"user","content":"q1"},
{"role":"assistant","content":[{"type":"thinking","thinking":"t1"},{"type":"tool_use","id":"c1","name":"f","input":{}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]},
{"role":"assistant","content":[{"type":"thinking","thinking":"t2"},{"type":"text","text":"done"}]},
{"role":"user","content":"q2"}]}`
	r, err := DecodeWithOptions(Anthropic, []byte(raw), DecodeOptions{ReplayThinking: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Thinking != (ThinkingReplayStats{Enabled: true, PlainBlocks: 2, ReplayedBlocks: 2, ReplayedBytes: 4}) {
		t.Fatalf("stats=%+v", r.Thinking)
	}
	msgs := r.Chat.Object["messages"].([]any)
	first, second := msgs[1].(map[string]any), msgs[3].(map[string]any)
	for field, want := range map[string]struct {
		msg  map[string]any
		text string
	}{
		"assistant#1": {first, "t1"}, "assistant#2": {second, "t2"},
	} {
		if want.msg["reasoning"] != want.text || want.msg["reasoning_content"] != want.text {
			t.Fatalf("%s=%+v", field, want.msg)
		}
	}
	if first["content"] != nil || len(first["tool_calls"].([]any)) != 1 || second["content"].([]any)[0].(map[string]any)["text"] != "done" {
		t.Fatal(msgs[1], msgs[3])
	}
	// content:null + reasoning 必须仍在既有 Chat 契约内：出站编码后原样保留。
	out, err := r.Chat.Encode("m")
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatal(err)
	}
	got := round["messages"].([]any)[1].(map[string]any)
	if got["reasoning"] != "t1" || got["reasoning_content"] != "t1" || got["content"] != nil {
		t.Fatalf("encode round-trip lost reasoning: %s", out)
	}
	if _, exists := round["messages"].([]any)[0].(map[string]any)["reasoning"]; exists {
		t.Fatal("user message polluted")
	}
}

func TestThinkingReplayDoesNotAffectOtherProtocols(t *testing.T) {
	for kind, raw := range map[Kind]string{
		Chat:      `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok","reasoning":"native history","reasoning_content":"extension"}]}`,
		Responses: `{"model":"m","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"do not replay"}]},{"role":"user","content":"hi"}]}`,
	} {
		off, err := Decode(kind, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		on, err := DecodeWithOptions(kind, []byte(raw), DecodeOptions{ReplayThinking: true})
		if err != nil || !reflect.DeepEqual(off, on) || on.Thinking != (ThinkingReplayStats{}) {
			t.Fatalf("%s changed: %v", kind, err)
		}
	}
}
