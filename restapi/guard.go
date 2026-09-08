// Browser-facing request guards. The REST API is reachable at a loopback
// address by default and, in tunnel mode, behind a public hostname — both
// are addressable by any page the operator happens to be visiting. Bearer
// auth alone does not stop that: a cross-site form POST or a WebSocket
// upgrade carries ambient credentials and needs no preflight, and a DNS
// rebind turns "loopback only" into "any origin". So every request is also
// checked for where it claims to come from.
package restapi

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// tokenEqual compares a presented credential against the expected one in
// constant time. Both the router API token and the terminal token arrive
// from untrusted callers who may measure how long the comparison takes.
func tokenEqual(presented, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// bearerEqual reports whether an Authorization header carries expected.
func bearerEqual(header, expected string) bool {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	return ok && tokenEqual(tok, expected)
}

// hostAllowed vets the Host header. Loopback is always allowed — that is the
// address the router binds by default and the one an SSH forward lands on.
// Anything else must be named in AllowedHosts (serve's --allowed-host, or the
// tunnel hostname the installer configures). A DNS-rebinding page reaches
// 127.0.0.1 but still sends its own name in Host, so this rejects it.
func (s *Server) hostAllowed(hostport string) bool {
	host := hostOnly(hostport)
	if host == "" {
		return false
	}
	if isLoopbackName(host) {
		return true
	}
	for _, h := range s.AllowedHosts {
		if strings.EqualFold(host, hostOnly(h)) {
			return true
		}
	}
	return false
}

// originAllowed vets the Origin header of a cross-site-capable request. An
// absent Origin means a non-browser client (curl, the CLI, a script): those
// cannot be driven by a hostile page, so they pass. A present Origin must
// name a host we would have accepted in Host — i.e. our own UI.
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if origin == "null" {
		return false // sandboxed iframe / file:// — never our UI
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return s.hostAllowed(u.Host)
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopbackName(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// guard applies the origin/host checks every handler needs. It returns false
// once the response has been written.
func (s *Server) guard(w http.ResponseWriter, r *http.Request) bool {
	if !s.hostAllowed(r.Host) {
		slog.Warn("api rejected host", "host", r.Host, "path", r.URL.Path, "remote", r.RemoteAddr)
		httpError(w, http.StatusMisdirectedRequest,
			"unrecognized Host header (start serve with --allowed-host for this name)")
		return false
	}
	if !s.originAllowed(r.Header.Get("Origin")) {
		slog.Warn("api rejected origin",
			"origin", r.Header.Get("Origin"), "path", r.URL.Path, "remote", r.RemoteAddr)
		httpError(w, http.StatusForbidden, "cross-origin request refused")
		return false
	}
	return true
}

// requireJSON rejects bodies that are not JSON. Beyond being correct, this
// is a CSRF control: a form-encoded or text/plain POST is a "simple" request
// that crosses origins without a preflight, so refusing those shapes means a
// hostile page cannot reach a mutating handler even if its Origin is spoofed
// away by an older browser.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	return true
}
