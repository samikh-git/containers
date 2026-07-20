// Command termbridge is the sandbox-side half of the web terminal: a
// WebSocket↔PTY bridge the router proxies browser connections to
// (/api/workspaces/{id}/terminal → sandbox :4322/term).
//
// Protocol, per connection:
//   - binary frames        raw terminal bytes, both directions
//   - text frames (c→s)    JSON control: {"type":"resize","cols":N,"rows":N}
//   - close 4000 (s→c)     the shell exited
//   - close 4002 (s→c)     the sandbox is stopping (SIGTERM — hibernation)
//
// Each connection gets its own login shell in /workspace. The bridge holds
// no state worth checkpointing: shells die with the sandbox by design (§7 —
// processes don't survive scale-to-zero, /workspace does).
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const (
	listenAddr = ":4322"

	closeShellExited = 4000
	closeStopping    = 4002
)

// The router is the only peer that can reach this port (sandbox netns / veth,
// §8), so cross-origin checks would only ever see the router's forwarded
// request. Accept all origins and let the router's auth be the gate.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

type resizeMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// sessions tracks live connections so a stop signal can close them with a
// code the browser can tell apart from a network failure.
var sessions = struct {
	sync.Mutex
	conns map[*websocket.Conn]struct{}
}{conns: map[*websocket.Conn]struct{}{}}

func main() {
	// SIGTERM is best-effort: docker/containerd signal PID 1 (opencode), not
	// us, so most hibernations just drop the sockets. When we do get the
	// signal (direct kill, future supervisor), say goodbye properly.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		sessions.Lock()
		for c := range sessions.conns {
			c.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(closeStopping, "sandbox stopping"),
				time.Now().Add(2*time.Second))
			c.Close()
		}
		sessions.Unlock()
		os.Exit(0)
	}()

	http.HandleFunc("/term", serveTerm)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	log.Printf("termbridge listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}

func serveTerm(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already replied with an error
	}
	sessions.Lock()
	sessions.conns[conn] = struct{}{}
	sessions.Unlock()
	defer func() {
		sessions.Lock()
		delete(sessions.conns, conn)
		sessions.Unlock()
		conn.Close()
	}()

	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = "/workspace"
	if _, err := os.Stat(cmd.Dir); err != nil {
		cmd.Dir = "/" // dev runs outside the sandbox contract
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "pty: "+err.Error()),
			time.Now().Add(2*time.Second))
		return
	}
	defer func() {
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// PTY → socket. Serialize writes: the reader goroutine below never
	// writes data frames, so only close frames race — WriteControl is safe
	// concurrently with WriteMessage per gorilla's contract.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				// Shell exited (PTY slave closed) — tell the client why.
				conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(closeShellExited, "shell exited"),
					time.Now().Add(2*time.Second))
				return
			}
		}
	}()

	// Socket → PTY.
readLoop:
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		switch kind {
		case websocket.BinaryMessage:
			if _, err := ptmx.Write(data); err != nil {
				break readLoop
			}
		case websocket.TextMessage:
			var msg resizeMsg
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" && msg.Cols > 0 && msg.Rows > 0 {
				pty.Setsize(ptmx, &pty.Winsize{Cols: msg.Cols, Rows: msg.Rows})
			}
		}
	}
	ptmx.Close() // unblocks the reader goroutine
	<-done
}
