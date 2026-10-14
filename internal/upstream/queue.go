// Capacity queue metadata and retry policy.
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy_manager/internal/jsondoc"
)

const (
	QueueCodeWaiting      = "6020"
	QueueCodeFull         = "6021"
	QueueCodeUserLimit    = "6022"
	queueRetryAfterSanity = retryAfterSanity
)

// QueueInfo contains only recognized queue diagnostics; absent values are zero.
type QueueInfo struct {
	Code       string
	RetryAfter time.Duration
	Position   int64
	Size       int64
}

func IsModelQueuedCode(code string) bool {
	switch strings.TrimSpace(code) {
	case QueueCodeWaiting, QueueCodeFull, QueueCodeUserLimit:
		return true
	}
	return false
}

// businessEnvelope selects the shallowest nonzero code in known envelope wrappers.
// Root code wins; ties prefer error before data and code before its aliases. Do not
// search arbitrary nested objects: a diagnostic's code cannot override its envelope.
func businessEnvelope(root map[string]any) (map[string]any, string) {
	level := []map[string]any{root}
	for len(level) > 0 {
		var next []map[string]any
		for _, obj := range level {
			for _, key := range []string{"code", "bizCode", "biz_code"} {
				if code := businessCodeValue(obj[key]); code != "" && code != "0" {
					return obj, code
				}
			}
			for _, key := range []string{"error", "data"} {
				if child, ok := obj[key].(map[string]any); ok {
					next = append(next, child)
				}
			}
		}
		level = next
	}
	return root, ""
}

func businessCodeValue(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	if n, ok := queueNumber(v); ok {
		return strconv.FormatInt(n, 10)
	}
	return ""
}

// BusinessCode and QueueCode share the same authority rule. JSON escaping and
// numeric exponent notation are decoded before comparison (no literal prefilter).
func BusinessCode(body string) string {
	root, err := jsondoc.Object([]byte(body))
	if err != nil {
		return ""
	}
	_, code := businessEnvelope(root)
	if len(code) > 32 {
		return ""
	} // never archive arbitrary long upstream values
	return code
}

func QueueCode(body string) string {
	code := BusinessCode(body)
	if IsModelQueuedCode(code) {
		return code
	}
	return ""
}

// queueInfo extracts known fields independently of classification. Callers may
// already know this is a queue response from HTTP 202 even when no code exists.
func queueInfo(body string) QueueInfo {
	root, err := jsondoc.Object([]byte(body))
	if err != nil {
		return QueueInfo{}
	}
	obj, code := businessEnvelope(root)
	info := QueueInfo{Code: code}
	// Only follow data wrappers belonging to the selected envelope. Deeper fields
	// override outer fields; sibling diagnostics cannot supply queue information.
	for obj != nil {
		if v, ok := obj["queue_position"]; ok {
			info.Position, _ = queueNumber(v)
		}
		if v, ok := obj["queue_size"]; ok {
			info.Size, _ = queueNumber(v)
		}
		if v, ok := obj["retry_after"]; ok {
			n, _ := queueNumber(v)
			info.RetryAfter = queueRetryAfterOf(n)
		}
		obj, _ = obj["data"].(map[string]any)
	}
	return info
}

func ParseQueueInfo(body string) (QueueInfo, bool) {
	info := queueInfo(body)
	if !IsModelQueuedCode(info.Code) {
		return QueueInfo{}, false
	}
	return info, true
}

func QueueRetryAfter(body string) time.Duration {
	info, ok := ParseQueueInfo(body)
	if !ok {
		return 0
	}
	return info.RetryAfter
}

// queueDelay is only called after queue classification. Valid HTTP headers have
// priority over body data for every transport/status, including code-less 202.
func queueDelay(body string, headerDelay time.Duration) time.Duration {
	if headerDelay > 0 && headerDelay <= queueRetryAfterSanity {
		return headerDelay
	}
	return queueInfo(body).RetryAfter
}

// QueueRetrySeconds rounds upward, without adding a second to exact integers.
func QueueRetrySeconds(d time.Duration) int64 {
	if d <= 0 || d > queueRetryAfterSanity {
		return 0
	}
	return int64((d + time.Second - 1) / time.Second)
}

// SetQueueRetryAfter must be called before committing response headers.
func SetQueueRetryAfter(h http.Header, d time.Duration) {
	if seconds := QueueRetrySeconds(d); seconds > 0 {
		h.Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
}

// queueResponseBody preserves header metadata until HTTP-200 JSON/SSE decoding.
// The embedded closer retains cancellation/idle-monitor ownership.
type queueResponseBody struct {
	io.ReadCloser
	retryAfter time.Duration
}

func (r *queueResponseBody) queueHeaderDelay() time.Duration { return r.retryAfter }

func queueRetryAfterOf(v int64) time.Duration {
	if v <= 0 {
		return 0
	}
	unit := time.Second
	if v >= 1000 {
		unit = time.Millisecond
	}
	// Check before multiplication: an int64 overflow can otherwise look like 2s.
	if v > int64(queueRetryAfterSanity/unit) {
		return 0
	}
	d := time.Duration(v) * unit
	if d < time.Second {
		return 0
	}
	return d
}

// Queue fields accept complete nonnegative integers, not numeric prefixes or
// fractional values. Exponent-form JSON integers are accepted when exact/safe.
func queueNumber(v any) (int64, bool) {
	if s, ok := v.(string); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	if n, ok := jsondoc.Int(v); ok {
		if n < 0 {
			return 0, false
		}
		return n, true
	}
	if n, ok := v.(json.Number); ok {
		return queueJSONInteger(string(n))
	}
	return 0, false
}

// queueJSONInteger checks a JSON-validated decimal/exponent exactly. Work on
// decimal digits rather than float64 (which can round a fraction into an integer)
// or large powers of ten (an untrusted exponent must not drive allocations).
func queueJSONInteger(s string) (int64, bool) {
	mantissa, exponent := s, int64(0)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]
		var err error
		exponent, err = strconv.ParseInt(s[i+1:], 10, 64)
		if err != nil {
			return 0, false
		}
	}
	fractionDigits := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fractionDigits = len(mantissa) - i - 1
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	if mantissa == "" {
		return 0, false
	}
	for _, c := range mantissa {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return 0, true
	}
	// A nonzero value cannot be an int64 integer outside these bounds. Check
	// before computing scale so even extreme exponents cannot overflow it.
	if exponent < -int64(len(mantissa)) || exponent > int64(len(mantissa))+19 {
		return 0, false
	}
	significant := strings.TrimRight(digits, "0")
	scale := exponent - int64(fractionDigits) + int64(len(digits)-len(significant))
	if scale < 0 || int64(len(significant))+scale > 19 {
		return 0, false
	}
	n, err := strconv.ParseInt(significant+strings.Repeat("0", int(scale)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// QueueErrorResponse keeps the safe upstream error envelope and adds actionable
// queue information. No arbitrary raw JSON/HTML is newly reflected to clients.
func QueueErrorResponse(payload string, delay time.Duration) map[string]any {
	root, _ := jsondoc.Object([]byte(payload))
	envelope, _ := businessEnvelope(root)
	body := errorResponseObject(ErrModelQueued, envelope)
	body["error"].(map[string]any)["gateway_hint"] = queueHint(payload, delay)
	return body
}

func queueHint(payload string, delay time.Duration) string {
	info := queueInfo(payload)
	parts := make([]string, 0, 3)
	if info.Position > 0 {
		parts = append(parts, fmt.Sprintf("queue position %d", info.Position))
	}
	if info.Size > 0 {
		parts = append(parts, fmt.Sprintf("queue size %d", info.Size))
	}
	if seconds := QueueRetrySeconds(queueDelay(payload, delay)); seconds > 0 {
		parts = append(parts, fmt.Sprintf("retry after %ds", seconds))
	}
	detail := ""
	if len(parts) > 0 {
		detail = " (" + strings.Join(parts, ", ") + ")"
	}
	switch info.Code {
	case QueueCodeFull:
		return "upstream model capacity queue is full; retry later or pick another model" + detail
	case QueueCodeUserLimit:
		return "upstream model queue limit reached for this account; retry later or pick another model" + detail
	default:
		return "queued by upstream model capacity queue; retry later or pick another model" + detail
	}
}
