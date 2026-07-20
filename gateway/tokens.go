package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// usageTap tees an upstream response body while scanning for provider usage
// fields (DESIGN §9: parse OpenAI-compatible and Anthropic streams enough to
// log token counts). Parsing is best-effort — incomplete or unknown shapes
// simply leave Input/Output at zero.
type usageTap struct {
	r      io.Reader
	kind   string // "anthropic" | "openai" | …
	buf    []byte // incomplete trailing line
	body   []byte // non-SSE fallback (capped)
	input  int64
	output int64
}

const usageBodyCap = 1 << 20 // 1 MiB — enough for non-stream JSON; SSE is line-scanned

func (t *usageTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.scan(p[:n])
	}
	return n, err
}

func (t *usageTap) scan(chunk []byte) {
	t.buf = append(t.buf, chunk...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSpace(t.buf[:i]))
		t.buf = t.buf[i+1:]
		t.scanLine(line)
	}
	// Retain a small non-SSE sample for JSON responses that arrive without
	// newlines until the end (or as a single blob).
	if len(t.body) < usageBodyCap {
		remain := usageBodyCap - len(t.body)
		if remain > len(chunk) {
			remain = len(chunk)
		}
		t.body = append(t.body, chunk[:remain]...)
	}
}

func (t *usageTap) scanLine(line string) {
	if line == "" || line == "data: [DONE]" {
		return
	}
	payload := line
	if rest, ok := strings.CutPrefix(line, "data:"); ok {
		payload = strings.TrimSpace(rest)
		if payload == "" || payload == "[DONE]" {
			return
		}
	} else if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, ":") {
		return
	}
	in, out, ok := parseUsageJSON([]byte(payload), t.kind)
	if !ok {
		return
	}
	// Providers often emit usage incrementally (message_start then
	// message_delta). Keep the running max so partials don't clobber totals.
	if in > t.input {
		t.input = in
	}
	if out > t.output {
		t.output = out
	}
}

// finish drains any trailing buffer and, if still empty, tries a non-SSE JSON body.
func (t *usageTap) finish() (input, output int64) {
	if len(t.buf) > 0 {
		t.scanLine(string(bytes.TrimSpace(t.buf)))
		t.buf = nil
	}
	if t.input == 0 && t.output == 0 && len(t.body) > 0 {
		if in, out, ok := parseUsageJSON(t.body, t.kind); ok {
			t.input, t.output = in, out
		}
	}
	return t.input, t.output
}

func parseUsageJSON(b []byte, kind string) (input, output int64, ok bool) {
	// Strip a leading "data: " if a caller passed a raw SSE line.
	b = bytes.TrimSpace(b)
	if bytes.HasPrefix(b, []byte("data:")) {
		b = bytes.TrimSpace(b[5:])
	}
	if len(b) == 0 || b[0] != '{' {
		return 0, 0, false
	}

	switch kind {
	case "anthropic":
		var msg struct {
			Type    string     `json:"type"`
			Usage   *anthUsage `json:"usage"`
			Message *struct {
				Usage *anthUsage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &msg) != nil {
			return 0, 0, false
		}
		u := msg.Usage
		if u == nil && msg.Message != nil {
			u = msg.Message.Usage
		}
		if u == nil {
			// Non-stream Anthropic message object.
			var bare struct {
				Usage *anthUsage `json:"usage"`
			}
			if json.Unmarshal(b, &bare) != nil || bare.Usage == nil {
				return 0, 0, false
			}
			u = bare.Usage
		}
		return u.InputTokens, u.OutputTokens, true

	default: // openai-compatible (and unknown kinds that follow it)
		var envelope struct {
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				InputTokens      int64 `json:"input_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(b, &envelope) != nil || envelope.Usage == nil {
			return 0, 0, false
		}
		u := envelope.Usage
		in := u.PromptTokens
		if u.InputTokens > in {
			in = u.InputTokens
		}
		out := u.CompletionTokens
		if u.OutputTokens > out {
			out = u.OutputTokens
		}
		return in, out, true
	}
}

type anthUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// streamCopyAndTap copies upstream → client while extracting token usage for
// the given provider kind. Returns bytes written and any tokens found.
func streamCopyAndTap(w http.ResponseWriter, r io.Reader, kind string) (n, input, output int64) {
	tap := &usageTap{r: r, kind: kind}
	n = streamCopy(w, tap)
	input, output = tap.finish()
	return n, input, output
}
