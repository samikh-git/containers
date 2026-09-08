package dataplane

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTerminalTokenIsPerWorkspaceAndStable(t *testing.T) {
	k := &TerminalKey{Path: filepath.Join(t.TempDir(), "terminal.key")}

	a1, err := k.Token("ws1")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := k.Token("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 {
		t.Fatal("token must be stable for a workspace across calls")
	}
	b, err := k.Token("ws2")
	if err != nil {
		t.Fatal(err)
	}
	if a1 == b {
		t.Fatal("two workspaces must not share a terminal token")
	}

	// A fresh key object over the same file recomputes the same tokens —
	// this is what lets a restarted router still reach a running sandbox.
	again := &TerminalKey{Path: k.Path}
	a3, err := again.Token("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if a3 != a1 {
		t.Fatal("token must survive a router restart")
	}
}

// A branch is a clone: it arrives holding its parent's token file, and must
// not keep it — otherwise the parent's credential opens the branch's shell.
func TestTerminalTokenInstallOverwritesInheritedFile(t *testing.T) {
	k := &TerminalKey{Path: filepath.Join(t.TempDir(), "terminal.key")}
	mount := t.TempDir()

	if err := k.Install("parent", mount); err != nil {
		t.Fatal(err)
	}
	parent, _ := os.ReadFile(filepath.Join(mount, filepath.FromSlash(TerminalTokenFile)))

	if err := k.Install("branch", mount); err != nil {
		t.Fatal(err)
	}
	branch, err := os.ReadFile(filepath.Join(mount, filepath.FromSlash(TerminalTokenFile)))
	if err != nil {
		t.Fatal(err)
	}
	if string(parent) == string(branch) {
		t.Fatal("clone kept its parent's terminal token")
	}
	want, _ := k.Token("branch")
	if string(branch) != want {
		t.Fatalf("installed token %q, want %q", branch, want)
	}
}
