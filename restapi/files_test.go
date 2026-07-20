package restapi

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// filesTestServer provisions ws1 and returns its mount path alongside the
// test server.
func filesTestServer(t *testing.T, token string) (ts *httpTestServer, mount string) {
	t.Helper()
	srv, _ := newTestServer(t, token)
	resp, body := do(t, "POST", srv.URL+"/api/workspaces", `{"id":"ws1"}`, token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("up: got %d (%v)", resp.StatusCode, body)
	}
	return &httpTestServer{URL: srv.URL}, body["mount"].(string)
}

// httpTestServer is a tiny holder so helpers read naturally.
type httpTestServer struct{ URL string }

func (s *httpTestServer) files(path string) string {
	return s.URL + "/api/workspaces/ws1/files/" + path
}

func TestFileWriteReadRoundtrip(t *testing.T) {
	ts, _ := filesTestServer(t, "")

	resp, body := do(t, "PUT", ts.files("src/main.go"), `{"content":"package main\n","baseHash":""}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write: got %d (%v)", resp.StatusCode, body)
	}
	hash := body["hash"].(string)
	if hash == "" {
		t.Fatal("write returned empty hash")
	}

	resp, body = do(t, "GET", ts.files("src/main.go"), "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read: got %d (%v)", resp.StatusCode, body)
	}
	if body["type"] != "file" || body["content"] != "package main\n" || body["hash"] != hash {
		t.Fatalf("read mismatch: %v", body)
	}
}

func TestFileListing(t *testing.T) {
	ts, mount := filesTestServer(t, "")
	os.MkdirAll(filepath.Join(mount, "b-dir"), 0o755)
	os.WriteFile(filepath.Join(mount, "a.txt"), []byte("hi"), 0o644)

	resp, body := do(t, "GET", ts.URL+"/api/workspaces/ws1/files", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list root: got %d (%v)", resp.StatusCode, body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %v", entries)
	}
	// Dirs sort first.
	first := entries[0].(map[string]any)
	if first["name"] != "b-dir" || first["type"] != "dir" {
		t.Fatalf("dirs-first ordering broken: %v", entries)
	}
}

func TestFileWriteConflict(t *testing.T) {
	ts, _ := filesTestServer(t, "")
	resp, body := do(t, "PUT", ts.files("f.txt"), `{"content":"v1","baseHash":""}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create: got %d (%v)", resp.StatusCode, body)
	}
	v1hash := body["hash"].(string)

	// Create again without baseHash → conflict.
	resp, _ = do(t, "PUT", ts.files("f.txt"), `{"content":"x","baseHash":""}`, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("create-over-existing: got %d, want 409", resp.StatusCode)
	}

	// Update with correct baseHash → ok.
	resp, body = do(t, "PUT", ts.files("f.txt"),
		fmt.Sprintf(`{"content":"v2","baseHash":"%s"}`, v1hash), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: got %d (%v)", resp.StatusCode, body)
	}

	// Update with stale hash → 409 carrying current content.
	resp, body = do(t, "PUT", ts.files("f.txt"),
		fmt.Sprintf(`{"content":"v3","baseHash":"%s"}`, v1hash), "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale write: got %d, want 409", resp.StatusCode)
	}
	if body["content"] != "v2" {
		t.Fatalf("conflict should return current content, got %v", body)
	}

	// Force overrides.
	resp, _ = do(t, "PUT", ts.files("f.txt"),
		fmt.Sprintf(`{"content":"v3","baseHash":"%s","force":true}`, v1hash), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("force write: got %d, want 200", resp.StatusCode)
	}
	resp, body = do(t, "GET", ts.files("f.txt"), "", "")
	if body["content"] != "v3" {
		t.Fatalf("force write did not land: %v", body)
	}
}

func TestFileTraversalRejected(t *testing.T) {
	ts, mount := filesTestServer(t, "")
	// A sibling secret outside the workspace data dir.
	secret := filepath.Join(filepath.Dir(mount), "secret.txt")
	os.WriteFile(secret, []byte("s3cret"), 0o644)

	for _, p := range []string{
		"../secret.txt",
		"a/../../secret.txt",
		"..%2Fsecret.txt",
		"%2e%2e/secret.txt",
	} {
		resp, body := do(t, "GET", ts.files(p), "", "")
		if resp.StatusCode == http.StatusOK && body["content"] == "s3cret" {
			t.Fatalf("traversal %q leaked the secret", p)
		}
	}
	// Absolute path.
	resp, _ := do(t, "GET", ts.URL+"/api/workspaces/ws1/files//etc/passwd", "", "")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("absolute path read succeeded")
	}
}

func TestFileSymlinkEscapeRejected(t *testing.T) {
	ts, mount := filesTestServer(t, "")
	outside := filepath.Join(filepath.Dir(mount), "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s3cret"), 0o644)
	if err := os.Symlink(outside, filepath.Join(mount, "pwn")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	resp, body := do(t, "GET", ts.files("pwn/secret.txt"), "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("symlink escape read: got %d (%v), want 400", resp.StatusCode, body)
	}
	resp, _ = do(t, "PUT", ts.files("pwn/new.txt"), `{"content":"x","baseHash":""}`, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("symlink escape write: got %d, want 400", resp.StatusCode)
	}

	// A file symlink pointing outside must not be written through.
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(mount, "link.txt"))
	resp, _ = do(t, "PUT", ts.files("link.txt"), `{"content":"x","force":true}`, "")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("write through file symlink succeeded")
	}
}

func TestFileRename(t *testing.T) {
	ts, mount := filesTestServer(t, "")
	do(t, "PUT", ts.files("old.txt"), `{"content":"v","baseHash":""}`, "")

	resp, body := do(t, "POST", ts.files("old.txt"), `{"op":"rename","to":"dir/new.txt"}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: got %d (%v)", resp.StatusCode, body)
	}
	resp, _ = do(t, "GET", ts.files("old.txt"), "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old path still readable: %d", resp.StatusCode)
	}
	resp, body = do(t, "GET", ts.files("dir/new.txt"), "", "")
	if resp.StatusCode != http.StatusOK || body["content"] != "v" {
		t.Fatalf("new path unreadable: %d (%v)", resp.StatusCode, body)
	}

	// Rename onto an existing file → 409.
	do(t, "PUT", ts.files("other.txt"), `{"content":"o","baseHash":""}`, "")
	resp, _ = do(t, "POST", ts.files("dir/new.txt"), `{"op":"rename","to":"other.txt"}`, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rename onto existing: got %d, want 409", resp.StatusCode)
	}

	// Rename "escaping" the root is confined by clean-anchoring: it must
	// never land outside the mount.
	do(t, "POST", ts.files("dir/new.txt"), `{"op":"rename","to":"../escape.txt"}`, "")
	if _, err := os.Stat(filepath.Join(filepath.Dir(mount), "escape.txt")); err == nil {
		t.Fatalf("rename escaped the workspace mount")
	}
}

func TestFileMkdirAndDelete(t *testing.T) {
	ts, _ := filesTestServer(t, "")
	resp, _ := do(t, "POST", ts.files("a/b"), `{"op":"mkdir"}`, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkdir: got %d, want 201", resp.StatusCode)
	}
	do(t, "PUT", ts.files("a/b/f.txt"), `{"content":"x","baseHash":""}`, "")

	// Non-empty dir without recursive → 409.
	resp, _ = do(t, "DELETE", ts.files("a"), "", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete non-empty: got %d, want 409", resp.StatusCode)
	}
	resp, _ = do(t, "DELETE", ts.files("a")+"?recursive=1", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("recursive delete: got %d, want 204", resp.StatusCode)
	}
	resp, _ = do(t, "GET", ts.files("a"), "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dir survived delete: %d", resp.StatusCode)
	}
}

func TestFileBinaryDetection(t *testing.T) {
	ts, mount := filesTestServer(t, "")
	os.WriteFile(filepath.Join(mount, "bin.dat"), []byte{0x89, 0x50, 0x00, 0x47}, 0o644)
	resp, body := do(t, "GET", ts.files("bin.dat"), "", "")
	if resp.StatusCode != http.StatusOK || body["type"] != "binary" {
		t.Fatalf("binary detection: got %d %v", resp.StatusCode, body)
	}
	if _, has := body["content"]; has {
		t.Fatalf("binary response leaked content: %v", body)
	}
}

func TestFileAuthRequired(t *testing.T) {
	ts, _ := filesTestServer(t, "sekrit")
	resp, _ := do(t, "GET", ts.URL+"/api/workspaces/ws1/files", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: got %d, want 401", resp.StatusCode)
	}
	resp, _ = do(t, "PUT", ts.files("f.txt"), `{"content":"x"}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated write: got %d, want 401", resp.StatusCode)
	}
	resp, _ = do(t, "GET", ts.URL+"/api/workspaces/ws1/files", "", "sekrit")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated list: got %d, want 200", resp.StatusCode)
	}
}
