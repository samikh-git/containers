// The web terminal proxy: GET /api/workspaces/{id}/terminal upgrades to a
// WebSocket and splices it byte-for-byte onto the sandbox's termbridge
// (sandbox-image/termbridge). The router never parses WebSocket frames — it
// forwards the client's upgrade request, hijacks the connection, and copies
// raw bytes both ways. The one thing it adds is the idle signal: any
// client→server traffic after the upgrade is a human touching the terminal
// (input or resize; browsers send nothing unprompted), recorded through
// dataplane.TerminalActivityLog as the fourth idle condition (DESIGN §7).
package restapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TerminalEndpointResolver is the optional Runtime capability the terminal
// proxy needs: a host-dialable address for a workspace's termbridge.
type TerminalEndpointResolver interface {
	TerminalEndpoint(ctx context.Context, id string) (string, error)
}

// terminalToken derives the credential the sandbox's bridge expects. An
// unconfigured key is not an error here — the bridge fails closed on its own
// side, and the empty token will simply be refused.
func (s *Server) terminalToken(id string) (string, error) {
	if s.Router.Terminal == nil {
		return "", nil
	}
	return s.Router.Terminal.Token(id)
}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// Provenance first, and unconditionally. A WebSocket upgrade is exempt
	// from the same-origin policy and never preflights, so without this any
	// page the operator visits could open a shell in every sandbox — with or
	// without a token configured, and from a public hostname once a tunnel is
	// attached. The browser always sends Origin on an upgrade, so an absent
	// one means a non-browser client.
	if !s.guard(w, r) {
		return
	}
	if r.Header.Get("Origin") == "" && s.Token == "" {
		// Nothing identified this caller: no origin to vet, no token to
		// present. Refuse rather than hand a raw shell to whoever asked.
		httpError(w, http.StatusForbidden, "terminal requires an Origin or a token")
		return
	}
	// Auth is inline, not s.auth: the browser WebSocket API cannot set an
	// Authorization header, so the token may arrive as ?token=… instead.
	// URL-borne tokens can end up in logs — acceptable for the local-mode
	// bearer token, which the operator can rotate freely.
	if s.Token != "" &&
		!bearerEqual(r.Header.Get("Authorization"), s.Token) &&
		!tokenEqual(r.URL.Query().Get("token"), s.Token) {
		httpError(w, http.StatusUnauthorized, "authorization required")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		httpError(w, http.StatusBadRequest, "expected a WebSocket upgrade")
		return
	}
	resolver, ok := s.Router.Runtime.(TerminalEndpointResolver)
	if !ok {
		httpError(w, http.StatusNotImplemented, "runtime cannot resolve terminal endpoints (running with --runtime none?)")
		return
	}
	endpoint, err := resolver.TerminalEndpoint(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	back, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		httpError(w, http.StatusBadGateway, "terminal unreachable: "+err.Error())
		return
	}

	// The bridge demands this workspace's terminal token: a peer sandbox can
	// open its port, so proving which workspace the connection is for is the
	// only thing that separates the operator from a neighbour.
	bridgeToken, err := s.terminalToken(id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Replay the upgrade request against the bridge's /term. Everything the
	// handshake needs (Sec-WebSocket-*, Upgrade, Connection) passes through;
	// the router API token must not leak into the sandbox (same rule as the
	// OpenCode proxy).
	var req strings.Builder
	fmt.Fprintf(&req, "GET /term?token=%s HTTP/1.1\r\nHost: %s\r\n",
		url.QueryEscape(bridgeToken), endpoint)
	for k, vs := range r.Header {
		if k == "Authorization" || k == "Host" {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(back, req.String()); err != nil {
		back.Close()
		httpError(w, http.StatusBadGateway, "terminal handshake failed: "+err.Error())
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		back.Close()
		httpError(w, http.StatusInternalServerError, "connection cannot be hijacked")
		return
	}
	client, clientBuf, err := hj.Hijack()
	if err != nil {
		back.Close()
		return
	}

	// Splice. The bridge's 101 response flows back verbatim through the
	// backend→client copy; if the handshake failed, so does its error. Read
	// the client through the hijacked bufio.Reader — it may hold bytes that
	// arrived with the request.
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := clientBuf.Read(buf)
			if n > 0 {
				s.TerminalActivity.Touch(id)
				if _, werr := back.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		io.Copy(client, back)
	}()
	<-done // either side dropping tears down both
	back.Close()
	client.Close()
}
