package restapi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"containerization/dataplane"
)

// termRuntime is NoneRuntime plus a terminal endpoint pointing at a fake
// termbridge.
type termRuntime struct {
	dataplane.NoneRuntime
	endpoint string
}

func (r termRuntime) TerminalEndpoint(context.Context, string) (string, error) {
	return r.endpoint, nil
}

// fakeBridge accepts one TCP connection, replies to the upgrade request with
// a 101, then echoes every byte back. The proxy never parses frames, so the
// test doesn't need real WebSocket framing either.
func fakeBridge(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.URL.Path != "/term" || req.Header.Get("Authorization") != "" {
			conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			return
		}
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		buf := make([]byte, 1024)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				conn.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return ln.Addr().String()
}

func TestTerminalProxySplicesAndRecordsActivity(t *testing.T) {
	root := t.TempDir()
	activity := filepath.Join(root, "terminal-activity.jsonl")
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: termRuntime{endpoint: fakeBridge(t)},
		},
		Token:            "sekrit",
		TerminalActivity: &dataplane.TerminalActivityLog{Path: activity},
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// Raw TCP client: send the upgrade the browser would (token in the query
	// — WebSocket clients cannot set headers), expect the bridge's 101 back
	// through the proxy, then bytes echoed post-upgrade.
	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Host and Origin are the server's own loopback address: the upgrade must
	// look like it came from the router's own UI (guard.go).
	fmt.Fprintf(conn, "GET /api/workspaces/ws1/terminal?token=sekrit HTTP/1.1\r\n"+
		"Host: %s\r\nOrigin: http://%s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr, addr)

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("expected 101 through the proxy, got %q (err %v)", status, err)
	}
	for { // drain response headers
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	if _, err := conn.Write([]byte("keystrokes")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len("keystrokes"))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatal(err)
	}
	if string(echo) != "keystrokes" {
		t.Fatalf("echo mismatch: %q", echo)
	}

	// The client→server bytes must have landed in the activity log.
	b, err := os.ReadFile(activity)
	if err != nil {
		t.Fatalf("no activity recorded: %v", err)
	}
	if !strings.Contains(string(b), `"workspace":"ws1"`) {
		t.Fatalf("activity log missing ws1: %s", b)
	}
}

func TestTerminalAuthRequired(t *testing.T) {
	root := t.TempDir()
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: termRuntime{endpoint: "127.0.0.1:1"},
		},
		Token: "sekrit",
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/workspaces/ws1/terminal?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestTerminalRequiresUpgrade(t *testing.T) {
	root := t.TempDir()
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: termRuntime{endpoint: "127.0.0.1:1"},
		},
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/workspaces/ws1/terminal", nil)
	req.Header.Set("Origin", ts.URL) // same-origin: the guard is not what we're testing
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a plain GET, got %d", resp.StatusCode)
	}
}

// A hostile page must not be able to open a shell in a sandbox. The upgrade
// carries no token here — as a cross-site request from a browser never
// would — and the Origin is somebody else's.
func TestTerminalCrossOriginRefused(t *testing.T) {
	root := t.TempDir()
	s := &Server{
		Router: &dataplane.Router{
			Storage: &dataplane.DirStorage{Root: root},
			Leases:  &dataplane.FileLeaseAuthority{Path: filepath.Join(root, "leases.json")},
			Runtime: termRuntime{endpoint: "127.0.0.1:1"},
		},
		// No token: the local-mode default, and the case where the origin
		// check is the only thing standing between evil.example and a shell.
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/workspaces/ws1/terminal", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin terminal upgrade must be refused, got %d", resp.StatusCode)
	}
}
