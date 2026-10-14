package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy_manager/internal/upstream"
)

// Stream relays text immediately but buffers tools until the native completion
// predicate and destination schema validate. Incomplete tools appear only in
// response.incomplete, never in tool lifecycle events that eager clients execute.
func Stream(w http.ResponseWriter, r io.Reader, req *Request, hint func(string) string, opts ...upstream.StreamOption) error {
	b := newBuilder(req)
	started, textStarted, streamedText := false, false, false
	textIndex := -1
	sequence := 0
	emit := func(name string, data map[string]any) error {
		data["type"] = name
		if req.Kind == Responses {
			data["sequence_number"] = sequence
			sequence++
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
			return &upstream.DownstreamWriteError{Err: err}
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	start := func() error {
		if started {
			return nil
		}
		started = true
		if req.Kind == Responses {
			if err := emit("response.created", map[string]any{"response": b.responseEnvelope("in_progress", []any{}, nil)}); err != nil {
				return err
			}
			return emit("response.in_progress", map[string]any{"response": b.responseEnvelope("in_progress", []any{}, nil)})
		}
		// Provisional counters, replaced by observed final input/output usage.
		return emit("message_start", map[string]any{"message": map[string]any{"id": b.id, "type": "message", "role": "assistant", "model": req.Chat.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	}
	startText := func(index int) error {
		if textStarted {
			if textIndex != index {
				return fmt.Errorf("multiple streaming text blocks are not enabled")
			}
			return nil
		}
		textStarted, textIndex = true, index
		if err := start(); err != nil {
			return err
		}
		if req.Kind == Anthropic {
			return emit("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "text", "text": ""}})
		}
		if err := emit("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": b.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}); err != nil {
			return err
		}
		return emit("response.content_part.added", map[string]any{"item_id": b.messageID, "output_index": index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
	}
	resp, err := collect(r, req, func(index int, text string) error {
		if err := startText(index); err != nil {
			return err
		}
		streamedText = true
		if req.Kind == Anthropic {
			return emit("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "text_delta", "text": text}})
		}
		return emit("response.output_text.delta", map[string]any{"item_id": b.messageID, "output_index": index, "content_index": 0, "delta": text, "logprobs": []any{}})
	}, opts...)
	var result map[string]any
	if err == nil {
		result, err = b.formatCompletion(resp)
	}
	if err != nil {
		var we *upstream.DownstreamWriteError
		if errors.As(err, &we) {
			return err
		}
		status, body := streamError(err, hint)
		if !started {
			var fe *upstream.FrameError
			if errors.As(err, &fe) && fe.Kind == upstream.ErrModelQueued {
				upstream.SetQueueRetryAfter(w.Header(), fe.RetryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if e := json.NewEncoder(w).Encode(ErrorBody(req.Kind, status, body)); e != nil {
				return &upstream.DownstreamWriteError{Err: e}
			}
			return err
		}
		if req.Kind == Anthropic {
			if e := emit("error", ErrorBody(req.Kind, status, body)); e != nil {
				return e
			}
		} else {
			failed := b.responseEnvelope("failed", []any{}, nil)
			failed["error"] = body["error"]
			if e := emit("response.failed", map[string]any{"response": failed}); e != nil {
				return e
			}
		}
		return err
	}
	if err := start(); err != nil {
		return err
	}
	key := "output"
	if req.Kind == Anthropic {
		key = "content"
	}
	for i, v := range result[key].([]any) {
		item := v.(map[string]any)
		if item["type"] == "text" || item["type"] == "message" {
			if err := startText(i); err != nil {
				return err
			}
			// Text that never arrived as deltas (a refusal fallback, or a snapshot
			// that superseded them) still has to reach the client in the block that
			// is about to close.
			text := ""
			if req.Kind == Anthropic {
				text, _ = item["text"].(string)
			} else {
				part := item["content"].([]any)[0].(map[string]any)
				text, _ = part["text"].(string)
			}
			if !streamedText && text != "" {
				if req.Kind == Anthropic {
					if err := emit("content_block_delta", map[string]any{"index": i, "delta": map[string]any{"type": "text_delta", "text": text}}); err != nil {
						return err
					}
				} else {
					if err := emit("response.output_text.delta", map[string]any{"item_id": b.messageID, "output_index": i, "content_index": 0, "delta": text, "logprobs": []any{}}); err != nil {
						return err
					}
				}
			}
			if req.Kind == Anthropic {
				if err := emit("content_block_stop", map[string]any{"index": i}); err != nil {
					return err
				}
			} else {
				part := item["content"].([]any)[0].(map[string]any)
				if err := emit("response.output_text.done", map[string]any{"item_id": b.messageID, "output_index": i, "content_index": 0, "text": part["text"], "logprobs": []any{}}); err != nil {
					return err
				}
				if err := emit("response.content_part.done", map[string]any{"item_id": b.messageID, "output_index": i, "content_index": 0, "part": part}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", map[string]any{"output_index": i, "item": item}); err != nil {
					return err
				}
			}
			continue
		}
		// Never signal that partial arguments/calls are ready for execution.
		if req.Kind == Responses && item["status"] == "incomplete" {
			continue
		}
		if req.Kind == Anthropic {
			if err := emit("content_block_start", map[string]any{"index": i, "content_block": map[string]any{"type": "tool_use", "id": item["id"], "name": item["name"], "input": map[string]any{}}}); err != nil {
				return err
			}
			raw, err := json.Marshal(item["input"])
			if err != nil {
				return err
			}
			if err := emit("content_block_delta", map[string]any{"index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(raw)}}); err != nil {
				return err
			}
			if err := emit("content_block_stop", map[string]any{"index": i}); err != nil {
				return err
			}
		} else {
			initial := map[string]any{"id": item["id"], "type": "function_call", "call_id": item["call_id"], "name": item["name"], "arguments": "", "status": "in_progress"}
			if namespace, ok := item["namespace"]; ok {
				initial["namespace"] = namespace
			}
			if err := emit("response.output_item.added", map[string]any{"output_index": i, "item": initial}); err != nil {
				return err
			}
			if err := emit("response.function_call_arguments.delta", map[string]any{"item_id": item["id"], "output_index": i, "delta": item["arguments"]}); err != nil {
				return err
			}
			if err := emit("response.function_call_arguments.done", map[string]any{"item_id": item["id"], "output_index": i, "name": item["name"], "arguments": item["arguments"]}); err != nil {
				return err
			}
			if err := emit("response.output_item.done", map[string]any{"output_index": i, "item": item}); err != nil {
				return err
			}
		}
	}
	if req.Kind == Anthropic {
		if err := emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": result["stop_reason"], "stop_sequence": nil}, "usage": result["usage"]}); err != nil {
			return err
		}
		return emit("message_stop", map[string]any{})
	}
	terminal := "response.completed"
	if result["status"] == "incomplete" {
		terminal = "response.incomplete"
	}
	return emit(terminal, map[string]any{"response": result})
}
