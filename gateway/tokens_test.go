package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseUsageAnthropicStream(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"output_tokens":0}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	tap := &usageTap{r: strings.NewReader(body), kind: "anthropic"}
	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatal(err)
	}
	in, out := tap.finish()
	if in != 120 || out != 42 {
		t.Fatalf("got input=%d output=%d, want 120/42", in, out)
	}
}

func TestParseUsageOpenAIStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"1","choices":[{"delta":{"content":"x"}}]}`,
		``,
		`data: {"id":"1","usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	tap := &usageTap{r: strings.NewReader(body), kind: "openai"}
	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatal(err)
	}
	in, out := tap.finish()
	if in != 10 || out != 5 {
		t.Fatalf("got input=%d output=%d, want 10/5", in, out)
	}
}

func TestParseUsageAnthropicJSON(t *testing.T) {
	body := `{"id":"msg","type":"message","usage":{"input_tokens":99,"output_tokens":11}}`
	tap := &usageTap{r: strings.NewReader(body), kind: "anthropic"}
	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatal(err)
	}
	in, out := tap.finish()
	if in != 99 || out != 11 {
		t.Fatalf("got input=%d output=%d, want 99/11", in, out)
	}
}

func TestForwardRecordsTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","usage":{"input_tokens":7,"output_tokens":3}}`)
	}))
	defer upstream.Close()
	s, ledgerPath := testServer(t, upstream.URL, "anthropic")

	w := post(t, s, "ws1-token", `{"model":"claude-sonnet-5"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	events := readLedger(t, ledgerPath)
	last := events[len(events)-1]
	if last.InputTokens != 7 || last.OutputTokens != 3 {
		t.Fatalf("tokens not recorded: %+v", last)
	}
}
