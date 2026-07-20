package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// End-to-end over a real PTY: connect, run a command, see its output, resize.
func TestBridgeRunsShellOverWebSocket(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(serveTerm))
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/term"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage,
		[]byte("echo bridge-$((20+22))\r")); err != nil {
		t.Fatal(err)
	}

	// The PTY echoes the input line too; scan output until the expansion
	// (which only the shell can have produced) appears.
	deadline := time.Now().Add(10 * time.Second)
	var out strings.Builder
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("connection dropped before output arrived: %v (got %q)", err, out.String())
		}
		if kind == websocket.BinaryMessage {
			out.Write(data)
		}
		if strings.Contains(out.String(), "bridge-42") {
			return
		}
	}
	t.Fatalf("shell output never arrived: %q", out.String())
}
