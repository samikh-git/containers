package dataplane

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// TerminalTokenFile is where the router drops a workspace's terminal
// credential, relative to the workspace mount. It sits under .agent-state/,
// which the entrypoint already git-ignores.
const TerminalTokenFile = ".agent-state/terminal.token"

// TerminalKey derives per-workspace credentials for the sandbox's web
// terminal bridge.
//
// The bridge listens on a port every sandbox on the shared network can
// reach, and it hands out a root shell — so "only the router can route to
// it" is not a control, it is an assumption about the network, and on both
// the Docker and the CNI paths that assumption is false: sandboxes can talk
// to each other. A per-workspace token turns reachability back into
// something that has to be authorized. A peer sandbox can still open the TCP
// connection; it just has nothing to present.
//
// Tokens are HMAC(key, workspace-id), so the router can recompute one at any
// time — after a restart, for a workspace it did not create — without
// keeping per-workspace state. The key is a file in the data root; losing it
// only means terminals need reconnecting after the router rewrites the
// token files.
type TerminalKey struct {
	// Path is the key file (created on first use, 0600).
	Path string

	mu  sync.Mutex
	key []byte
}

func (k *TerminalKey) load() ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.key) > 0 {
		return k.key, nil
	}
	if b, err := os.ReadFile(k.Path); err == nil && len(b) >= 32 {
		k.key = b
		return k.key, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(k.Path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(k.Path, key, 0o600); err != nil {
		return nil, err
	}
	k.key = key
	return k.key, nil
}

// Token returns the terminal credential for a workspace.
func (k *TerminalKey) Token(wsID string) (string, error) {
	key, err := k.load()
	if err != nil {
		return "", fmt.Errorf("terminal key: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(wsID))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Install writes the workspace's token into its mount, where the sandbox's
// termbridge reads it. Rewritten on every start: a branch cloned from
// another workspace inherits that workspace's file, and it must not keep it.
func (k *TerminalKey) Install(wsID, mountPath string) error {
	token, err := k.Token(wsID)
	if err != nil {
		return err
	}
	dst := filepath.Join(mountPath, filepath.FromSlash(TerminalTokenFile))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Truncate-and-write rather than rename: the sandbox may already have
	// this path open, and the file is small enough that a torn read only
	// costs a reconnect.
	return os.WriteFile(dst, []byte(token), 0o600)
}
