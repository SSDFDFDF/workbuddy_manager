package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy_manager/internal/auth"
	"workbuddy_manager/internal/forwarding"
	"workbuddy_manager/internal/prompt"
	"workbuddy_manager/internal/scrub"
	"workbuddy_manager/internal/upstream"
)

// 内置指纹夹具（与 scrub 包同一实测口径：Claude Code 身份串）。
const auditFingerprint = "You are Claude Code, Anthropic's official CLI for Claude."

// outboundCapture 捕获 handler 真正发给上游的请求体，供断言出站内容。
type outboundCapture struct {
	body string
	seen bool
}

func captureHandler(t *testing.T, cfg Config, cap *outboundCapture) *Handler {
	t.Helper()
	base := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, gatewayBenchSSE, true
	})
	// 包一层 Transport：先记录请求体，再交给 fake upstream 的 transport。
	inner := base.HTTP.Transport
	base.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		cap.body, cap.seen = string(raw), true
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		return inner.RoundTrip(r)
	})
	// 把调用方准备好的指纹层搬到这个 fake client 上。注意不能直接赋值 Holder
	// （内含 atomic.Pointer，不可拷贝），且**必须**搬——否则用例测的是一条没有
	// 指纹层的路径，断言语义会被静默架空。
	// global realm 开关同理（globalOn 读的是 client 侧快照，不是 handler 字段）：
	// 调用方在 cfg.Upstream 上声明的域开关必须跟着一起搬，否则 global 用例会被
	// 「global realm disabled」提前 400，测到的是一条死路。
	if cfg.Upstream != nil {
		if l := cfg.Upstream.Fingerprints.Load(); l != nil {
			base.Fingerprints.Store(l)
		}
		base.GlobalEnabled = cfg.Upstream.GlobalEnabled
	}
	cfg.Upstream = base
	return NewHandler(cfg)
}

// TestOutboundFingerprintRewriteStillApplies 守护「对象路径没有绕过指纹改写层」：
//
// 出站构造改为从已解析文档出发（buildOutbound）后，指纹层不再依赖「先序列化再解析」
// 那一步。这个用例把带指纹的 user 消息发进网关，断言**真正发给上游的字节**里指纹
// 已被破坏——若实现漏掉指纹步骤（例如对象路径只做线格式转换），这里会直接暴露。
func TestOutboundFingerprintRewriteStillApplies(t *testing.T) {
	up := &upstream.Client{}
	up.Fingerprints.Store(scrub.NewLayer(true, nil))

	cap := &outboundCapture{}
	h := captureHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	}, cap)

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + auditFingerprint + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !cap.seen {
		t.Fatal("未捕获到出站请求")
	}
	if strings.Contains(cap.body, auditFingerprint) {
		t.Fatalf("出站字节仍含指纹（改写层被绕过）: %s", cap.body)
	}
	if !strings.Contains(cap.body, "official CLI tool for Claude") {
		t.Fatalf("出站字节缺少改写后的语义保留串: %s", cap.body)
	}
}

// TestOutboundFingerprintAppliesAfterPromptCompose 组合场景：
// 启用系统提示词规则时，对象是先深拷贝再组合的——此时入站字节与该对象**不同源**，
// 指纹层必须退化为全量遍历（而不是依赖哨兵预检而漏掉组合进来的内容）。
// 这里让网关提示词本身就含指纹串，断言它同样被改写。
func TestOutboundFingerprintAppliesAfterPromptCompose(t *testing.T) {
	up := &upstream.Client{}
	up.Fingerprints.Store(scrub.NewLayer(true, nil))

	cap := &outboundCapture{}
	h := captureHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		// replace 模式：客户端 system 被删、网关提示词成为唯一 system。
		// 提示词里故意放指纹串（模拟用户配置的预设正文里含这类文本）。
		PromptRules: map[string]prompt.Rule{
			"": {Mode: prompt.ModeReplace, Text: auditFingerprint},
		},
	}, cap)

	body := `{"model":"glm-5.2","messages":[{"role":"system","content":"原 system"},{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(cap.body, auditFingerprint) {
		t.Fatalf("组合进来的系统提示词未被指纹层改写: %s", cap.body)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
		t.Fatalf("出站不是合法 JSON: %v", err)
	}
	msgs := sent["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("replace 模式应把网关提示词放在首位: %v", first)
	}
	if !strings.Contains(first["content"].(string), "official CLI tool for Claude") {
		t.Fatalf("网关提示词未按预期改写: %v", first["content"])
	}
}

// TestOutboundPromptComposeMatchesBytePath 守护两条组合路径产出同一结果：
// handler 现在走对象级 prompt.ComposeObject，字节版 prompt.Compose 仍被其它调用方
// 使用——两者对同一输入必须给出逐字节一致的出站文档（否则会出现"面板预览/其它
// 入口与真实请求不一致"的漂移）。
func TestOutboundPromptComposeMatchesBytePath(t *testing.T) {
	cases := map[string]string{
		"已有开头 system 块": `{"model":"glm-5.2","messages":[{"role":"system","content":"客户端 A"},{"role":"developer","content":"客户端 B"},{"role":"user","content":"hi"}]}`,
		"无 system":      `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		"中途 system":     `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"system","content":"中途"},{"role":"user","content":"again"}]}`,
		"tool 轮":        `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"r"}]}`,
	}
	modes := []string{prompt.ModeReplace, prompt.ModeAppend, prompt.ModeAfter}
	for name, body := range cases {
		for _, mode := range modes {
			t.Run(name+"/"+mode, func(t *testing.T) {
				cap := &outboundCapture{}
				h := captureHandler(t, Config{
					Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
					Upstream: &upstream.Client{},
					PromptRules: map[string]prompt.Rule{
						"": {Mode: mode, Text: "网关系统提示词"},
					},
				}, cap)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
				}

				// 参照：字节版组合 → 字节版 Parse → 字节版 Encode（旧链路语义）。
				composedBytes := prompt.Compose([]byte(body), "网关系统提示词", mode)
				composed, err := forwarding.Parse(composedBytes)
				if err != nil {
					t.Fatalf("composed parse: %v", err)
				}
				wantBytes, err := composed.Encode("glm-5.2")
				if err != nil {
					t.Fatal(err)
				}

				if cap.body != string(wantBytes) {
					t.Fatalf("对象路径与字节路径出站不一致:\n got=%s\nwant=%s", cap.body, wantBytes)
				}
			})
		}
	}
}

// TestOutboundEffortClampReachesUpstream 守护档位归一真正落到**出站字节**，并把
// 「原生 Chat 也会被归一」这一决定写成可执行契约（不是只写在注释里）：
//
//   - cn:glm-5.2 的档位能力（静态兜底表）= [high xhigh]，客户端要 medium/max 时必须
//     在发送前归一到 ≤请求档位的最高支持档，否则上游 400 code=11133（换号无用）；
//   - 模型档位未知（未收录）时**绝不改写**：编造一个集合去改用户请求比透传更糟。
func TestOutboundEffortClampReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, wantEffort string
	}{
		{"原生 Chat 降级", "/v1/chat/completions",
			`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`, "xhigh"},
		{"原生 Chat 升到最低支持档", "/v1/chat/completions",
			`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"low"}}`, "high"},
		{"原生 Chat 大小写归一", "/v1/chat/completions",
			`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"XHIGH"}`, "xhigh"},
		{"Responses 降级", "/v1/responses",
			`{"model":"cn:glm-5.2","store":false,"input":"hi","reasoning":{"effort":"max"}}`, "xhigh"},
		{"关闭思考的 off 永不被归一", "/v1/chat/completions",
			`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"off"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &outboundCapture{}
			h := captureHandler(t, Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: &upstream.Client{},
			}, cap)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
				t.Fatalf("出站不是合法 JSON: %v", err)
			}
			if got, _ := sent["reasoning_effort"].(string); got != tc.wantEffort {
				t.Fatalf("reasoning_effort=%q want %q (out=%s)", got, tc.wantEffort, cap.body)
			}
			// off 走 EncodeObject 的关闭分支：effort 消失 + thinking.disabled。
			if tc.wantEffort == "" {
				if th, _ := sent["thinking"].(map[string]any); th["type"] != "disabled" {
					t.Fatalf("off 未转成 thinking.disabled: %s", cap.body)
				}
			}
		})
	}
	// 未收录模型：没有能力依据 → 透传（不改写、不编造）。
	cap := &outboundCapture{}
	h := captureHandler(t, Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: &upstream.Client{},
	}, cap)
	body := `{"model":"cn:no-such-model","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	var sent map[string]any
	if err := json.Unmarshal([]byte(cap.body), &sent); err != nil {
		t.Fatalf("出站不是合法 JSON: %v", err)
	}
	if got, _ := sent["reasoning_effort"].(string); got != "max" {
		t.Fatalf("未知模型被改写了: reasoning_effort=%q (out=%s)", got, cap.body)
	}
}
