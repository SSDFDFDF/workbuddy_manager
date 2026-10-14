package upstream

import (
	"strings"
	"testing"
	"time"
)

// TestQueueCode 覆盖三种真实形态与若干反例：
//   - HTTP 信封 {"code":6020,"data":{...}}
//   - 双层包裹 {"data":{"data":{...}}}（桌面端 readCapacityQueueDataFromRaw 同款）
//   - SSE 帧 payload 用 bizCode（桌面端终帧读的就是 bizCode）
//
// 反例覆盖两类最容易误判的东西：DeepSeek 系 "code":0 的业务信封、以及正文里
// 恰好出现 6020 这个数字（如 token 计数）的非队列 body。
func TestQueueCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"envelope-waiting", `{"code":6020,"msg":"queued","data":{"queue_position":12,"queue_size":200,"retry_after":5}}`, QueueCodeWaiting},
		{"envelope-full", `{"code":6021,"msg":"model queue is full"}`, QueueCodeFull},
		{"user-limit-with-limit-wording", `{"code":6022,"msg":"user limit exceeded, retry later"}`, QueueCodeUserLimit},
		{"string-code", `{"code":"6020","data":{"retry_after":3000}}`, QueueCodeWaiting},
		{"frame-bizcode", `{"type":"message","bizCode":6020,"message":"queued","data":{"data":{"queue_position":3}}}`, QueueCodeWaiting},
		{"snake-biz-code", `{"biz_code":6021}`, QueueCodeFull},
		{"nested-data", `{"code":6020,"data":{"data":{"retry_after":12,"queue_position":1,"queue_size":9}}}`, QueueCodeWaiting},
		{"zero-envelope", `{"code":0,"msg":"ok","data":{"content":"hi"}}`, ""},
		{"token-number-noise", `{"msg":"prompt has 6020 tokens"}`, ""},
		{"empty", ``, ""},
		{"html", `<html><body>502 Bad Gateway</body></html>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueueCode(tc.body); got != tc.want {
				t.Fatalf("QueueCode=%q want %q", got, tc.want)
			}
		})
	}
}

// TestParseQueueInfoRetryAfter 口径：<1000 视为秒、>=1000 视为毫秒（与官方
// normalizeRetryAfterMs 一致），越界丢弃为 0。字段「更深的赢」。
func TestParseQueueInfoRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"seconds", `{"code":6020,"data":{"retry_after":5}}`, 5 * time.Second},
		{"milliseconds", `{"code":6020,"data":{"retry_after":3000}}`, 3 * time.Second},
		{"deep-wins", `{"code":6020,"retry_after":9999,"data":{"retry_after":7}}`, 7 * time.Second},
		{"too-large-dropped", `{"code":6020,"data":{"retry_after":1000000000}}`, 0},
		{"zero-dropped", `{"code":6020,"data":{"retry_after":0}}`, 0},
		{"absent", `{"code":6020,"data":{"queue_position":2}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueueRetryAfter(tc.body); got != tc.want {
				t.Fatalf("QueueRetryAfter=%s want %s", got, tc.want)
			}
		})
	}
}

// TestParseQueueInfoFields 位置/容量解析（用于 gateway_hint 与日志）。
func TestParseQueueInfoFields(t *testing.T) {
	info, ok := ParseQueueInfo(`{"code":6020,"data":{"data":{"queue_position":42,"queue_size":300,"retry_after":8}}}`)
	if !ok {
		t.Fatal("not parsed")
	}
	if info.Code != QueueCodeWaiting || info.Position != 42 || info.Size != 300 || info.RetryAfter != 8*time.Second {
		t.Fatalf("%+v", info)
	}
	if _, ok := ParseQueueInfo(`{"code":0}`); ok {
		t.Fatal("code 0 must not parse as queue")
	}
}

// TestClassifyModelQueued：队列必须压过 429 与限流/额度词表（否则健康账号被记
// 一次软冷却），但不得抢 11102 的词（语义更具体）与 402 的语义。
func TestClassifyModelQueued(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"waiting-400", 400, `{"code":6020,"msg":"queued"}`, ErrModelQueued},
		{"user-limit-429", 429, `{"code":6022,"msg":"user limit exceeded"}`, ErrModelQueued},
		{"full-200-envelope", 200, `{"code":6021,"msg":"queue full, quota exceeded"}`, ErrModelQueued},
		{"plain-429-stays-soft", 429, `{"msg":"rate limit exceeded"}`, ErrSoftRate},
		{"model-blocked-wins", 400, `{"code":11102,"msg":"service info not found"}`, ErrModelBlocked},
		{"402-stays-hard-credit", 402, `{"code":6020}`, ErrHardCredit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d)=%s want %s", tc.status, got, tc.want)
			}
		})
	}
}

// TestErrorStatusModelQueued 排队 = 可重试语义 → 429。
func TestErrorStatusModelQueued(t *testing.T) {
	if got := ErrorStatus(ErrModelQueued); got != 429 {
		t.Fatalf("ErrorStatus=%d want 429", got)
	}
	if got := ErrModelQueued.String(); got != "model_queued" {
		t.Fatalf("String=%q", got)
	}
}

// TestGatewayHintModelQueued hint 带可行动的量化信息（位置/容量/等待时长），
// 拿不到就退中性文案（不编造）。
func TestGatewayHintModelQueued(t *testing.T) {
	full := GatewayHint(ErrModelQueued, `{"code":6020,"data":{"queue_position":12,"queue_size":200,"retry_after":5}}`, HintContext{})
	for _, want := range []string{"queue position 12", "queue size 200", "retry after 5s"} {
		if !strings.Contains(full, want) {
			t.Fatalf("hint missing %q: %s", want, full)
		}
	}
	if h := GatewayHint(ErrModelQueued, `{"code":6021}`, HintContext{}); !strings.Contains(h, "full") {
		t.Fatalf("full-queue hint: %s", h)
	}
	if h := GatewayHint(ErrModelQueued, `{"code":6022}`, HintContext{}); !strings.Contains(h, "limit") {
		t.Fatalf("user-limit hint: %s", h)
	}
	if h := GatewayHint(ErrModelQueued, `{}`, HintContext{}); h == "" || strings.Contains(h, "0") {
		t.Fatalf("neutral hint: %s", h)
	}
}

// TestBusinessCode：取最浅（信封）码，非 JSON 不猜，0 不算码。
func TestBusinessCode(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"code":11102,"msg":"service info not found"}`, "11102"},
		{`{"code":0,"msg":"ok"}`, ""},
		{`{"code":6020,"data":{"code":7}}`, "6020"},
		{`{"data":{"code":"14018"}}`, "14018"},
		{`{"bizCode":6021}`, "6021"},
		{`{"msg":"plain text 11102"}`, ""},
	}
	for _, tc := range cases {
		if got := BusinessCode(tc.body); got != tc.want {
			t.Fatalf("BusinessCode(%s)=%q want %q", tc.body, got, tc.want)
		}
	}
}
