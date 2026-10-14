package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQueueAuthorityAndEncodings(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{"code":14018,"data":{"code":6020}}`, "14018"},
		{`{"code":0,"data":{"biz_code":6020}}`, "6020"},
		{`{"error":{"code":6021},"data":{"code":6020}}`, "6021"},
		{`{"code":6.020e3}`, "6020"},
		{`{"code":6.0200000000000001e3}`, ""},
		{`{"code":"\u0036\u0030\u0032\u0030"}`, "6020"},
		{`{"diagnostics":{"code":6020}}`, ""},
		{`{"code":6020,"code":14018}`, ""},
	} {
		if got := BusinessCode(tc.body); got != tc.code {
			t.Fatalf("%s code=%q want=%q", tc.body, got, tc.code)
		}
		expected := ""
		if IsModelQueuedCode(tc.code) {
			expected = tc.code
		}
		if got := QueueCode(tc.body); got != expected {
			t.Fatalf("%s queue=%q want=%q", tc.body, got, expected)
		}
	}
	if got := Classify(429, `{"code":14018,"msg":"credits exhausted","data":{"code":6020}}`); got != ErrHardCredit {
		t.Fatal(got)
	}
}

func TestQueueNumbersStrictAndBounded(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{`"5garbage"`, 0}, {`999.9`, 0}, {`5.0000000000000001`, 0}, {`"18446744073711552"`, 0}, {`9223372036854775807`, 0},
		{`999`, 999 * time.Second}, {`1000`, time.Second}, {`1500`, 1500 * time.Millisecond},
		{`7200000`, 2 * time.Hour}, {`7200001`, 0}, {`-1`, 0}, {`null`, 0}, {`"5"`, 5 * time.Second},
	} {
		body := `{"code":6020,"data":{"retry_after":` + tc.value + `}}`
		if got := QueueRetryAfter(body); got != tc.want {
			t.Fatalf("%s=%s want=%s", tc.value, got, tc.want)
		}
	}
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{time.Second, "1"}, {1500 * time.Millisecond, "2"}, {3 * time.Second, "3"}, {0, ""}} {
		h := http.Header{}
		SetQueueRetryAfter(h, tc.d)
		if h.Get("Retry-After") != tc.want {
			t.Fatal(h, tc)
		}
	}
	// Siblings must not override envelope-owned queue data.
	body := `{"code":6020,"data":{"retry_after":3},"diagnostics":{"data":{"retry_after":60}}}`
	if QueueRetryAfter(body) != 3*time.Second {
		t.Fatal("diagnostics contaminated delay")
	}
}

func TestQueueNumberExactJSON(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
		ok    bool
	}{
		{"6.020e3", 6020, true}, {"6.0200000000000001e3", 0, false},
		{"5.0000000000000001", 0, false}, {"5.0", 5, true},
		{"50e-1", 5, true}, {"1e-1000000", 0, false}, {"1e1000000", 0, false},
		{"9223372036854775807.0", 9223372036854775807, true},
		{"9223372036854775808.0", 0, false},
		{"-1", 0, false}, {"0.000", 0, true},
	} {
		n, ok := queueNumber(json.Number(tc.value))
		if n != tc.want || ok != tc.ok {
			t.Fatalf("%s = (%d,%v), want (%d,%v)", tc.value, n, ok, tc.want, tc.ok)
		}
	}
}

func TestQueueErrorUsesSelectedEnvelope(t *testing.T) {
	for _, tc := range []struct{ payload, key, code, message string }{
		{`{"code":0,"data":{"biz_code":6020,"message":"inner queue","retry_after":3,"token":"secret"}}`, "biz_code", "6020", "inner queue"},
		{`{"code":6021,"message":"outer queue","error":{"bizCode":14018,"message":"inner non-authoritative"}}`, "code", "6021", "outer queue"},
		{`{"error":{"bizCode":6022,"msg":"nested queue"},"data":{"code":6020}}`, "bizCode", "6022", "nested queue"},
	} {
		e := QueueErrorResponse(tc.payload, 0)["error"].(map[string]any)
		safe := e["upstream"].(map[string]any)
		if fmt.Sprint(safe[tc.key]) != tc.code || e["message"] != tc.message || safe["data"] != nil || safe["token"] != nil {
			t.Fatalf("payload=%s error=%v", tc.payload, e)
		}
	}
}

func TestQueueErrorPreservesSafeBusinessFields(t *testing.T) {
	b := QueueErrorResponse(`{"biz_code":6020,"message":"queued","requestId":"r","token":"secret","data":{"queue_position":1,"retry_after":3}}`, 60*time.Second)
	e := b["error"].(map[string]any)
	safe := e["upstream"].(map[string]any)
	if safe["biz_code"] == nil || safe["requestId"] != "r" || safe["token"] != nil || safe["data"] != nil {
		t.Fatal(safe)
	}
	if !strings.Contains(e["gateway_hint"].(string), "retry after 60s") {
		t.Fatal(e)
	}
}
