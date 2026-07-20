// Workspace file API: the web editor's read/write surface over the workspace
// mount. Unlike the OpenCode proxy this works whether or not the sandbox is
// awake — the mount outlives the container (DESIGN: down keeps the volume).
//
//	GET    /api/workspaces/{id}/files            list workspace root
//	GET    /api/workspaces/{id}/files/{path...}  read file or list dir
//	PUT    /api/workspaces/{id}/files/{path...}  write file ({"content","baseHash","force"})
//	POST   /api/workspaces/{id}/files/{path...}  {"op":"mkdir"} | {"op":"rename","to":…}
//	DELETE /api/workspaces/{id}/files/{path...}  delete (?recursive=1 for non-empty dirs)
package restapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxFileBytes bounds both reads and writes: the editor holds whole files
	// in memory as strings; anything bigger is not a code file.
	maxFileBytes = 10 << 20
)

type fileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" | "dir" | "symlink" | "other"
	Size int64  `json:"size,omitempty"`
}

type readResponse struct {
	Type     string      `json:"type"` // "file" | "dir" | "binary"
	Content  string      `json:"content,omitempty"`
	Hash     string      `json:"hash,omitempty"`
	Entries  []fileEntry `json:"entries,omitempty"`
	Size     int64       `json:"size"`
	Modified time.Time   `json:"modified"`
}

type writeRequest struct {
	Content  string `json:"content"`
	BaseHash string `json:"baseHash"`
	Force    bool   `json:"force"`
}

type fileOpRequest struct {
	Op string `json:"op"`
	To string `json:"to"`
}

func (s *Server) registerFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/workspaces/{id}/files", s.auth(s.fileRead))
	mux.HandleFunc("GET /api/workspaces/{id}/files/{path...}", s.auth(s.fileRead))
	mux.HandleFunc("PUT /api/workspaces/{id}/files/{path...}", s.auth(s.fileWrite))
	mux.HandleFunc("POST /api/workspaces/{id}/files/{path...}", s.auth(s.fileOp))
	mux.HandleFunc("POST /api/workspaces/{id}/files", s.auth(s.fileOp))
	mux.HandleFunc("DELETE /api/workspaces/{id}/files/{path...}", s.auth(s.fileDelete))
}

// workspaceRoot resolves a workspace id to its mount path. EnsureWorkspace is
// idempotent by contract; no s.mu here — file IO must not serialize behind
// provisioning.
func (s *Server) workspaceRoot(r *http.Request, id string) (string, error) {
	return s.Router.Storage.EnsureWorkspace(r.Context(), id, s.Defaults.QuotaGB)
}

// securePath confines rel under root. Lexical cleaning kills "..", and the
// symlink check matters because the workspace is agent-writable: a hostile
// agent could plant `ln -s / pwn` and wait for the editor to follow it. The
// returned path's final component may itself be a symlink — callers that
// write/delete/rename must Lstat and refuse to operate through it.
func securePath(root, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("invalid path")
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", errors.New("path must be relative")
	}
	clean := filepath.Clean("/" + filepath.FromSlash(rel)) // "/"-anchored: cannot escape
	target := filepath.Join(root, clean)

	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("workspace root: %w", err)
	}
	// Resolve the deepest existing ancestor and check it stays inside root.
	anc := target
	for {
		if real, err := filepath.EvalSymlinks(anc); err == nil {
			if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
				return "", errors.New("path escapes workspace")
			}
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			break
		}
		anc = parent
	}
	return target, nil
}

// resolveFilePath pulls {path...} off the request and confines it. ok=false
// means the response has been written.
func (s *Server) resolveFilePath(w http.ResponseWriter, r *http.Request) (abs, rel string, ok bool) {
	id, idOK := pathID(w, r)
	if !idOK {
		return "", "", false
	}
	root, err := s.workspaceRoot(r, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return "", "", false
	}
	rel = r.PathValue("path")
	if isLeasePath(rel) {
		// The lease file is fencing metadata (dataplane.FenceAndWrite), not
		// workspace content; the editor must never read or clobber it.
		httpError(w, http.StatusForbidden, "lease file is not editable")
		return "", "", false
	}
	abs, err = securePath(root, rel)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return "", "", false
	}
	return abs, rel, true
}

func isLeasePath(rel string) bool {
	return filepath.Clean("/"+filepath.FromSlash(rel)) == "/.lease"
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func looksBinary(b []byte) bool {
	head := b
	if len(head) > 8192 {
		head = head[:8192]
	}
	for _, c := range head {
		if c == 0 {
			return true
		}
	}
	return !utf8.Valid(b)
}

func (s *Server) fileRead(w http.ResponseWriter, r *http.Request) {
	abs, _, ok := s.resolveFilePath(w, r)
	if !ok {
		return
	}
	info, err := os.Lstat(abs)
	if err != nil {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Follow only after securePath vetted the resolution target.
		if info, err = os.Stat(abs); err != nil {
			httpError(w, http.StatusNotFound, "not found")
			return
		}
	}
	if info.IsDir() {
		entries, err := os.ReadDir(abs)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		atRoot := filepath.Clean("/"+filepath.FromSlash(r.PathValue("path"))) == "/"
		out := make([]fileEntry, 0, len(entries))
		for _, e := range entries {
			if atRoot && e.Name() == ".lease" {
				continue
			}
			fe := fileEntry{Name: e.Name()}
			switch {
			case e.IsDir():
				fe.Type = "dir"
			case e.Type()&os.ModeSymlink != 0:
				fe.Type = "symlink"
			case e.Type().IsRegular():
				fe.Type = "file"
				if fi, err := e.Info(); err == nil {
					fe.Size = fi.Size()
				}
			default:
				fe.Type = "other"
			}
			out = append(out, fe)
		}
		sort.Slice(out, func(i, j int) bool {
			di, dj := out[i].Type == "dir", out[j].Type == "dir"
			if di != dj {
				return di
			}
			return out[i].Name < out[j].Name
		})
		writeJSON(w, http.StatusOK, readResponse{Type: "dir", Entries: out, Modified: info.ModTime()})
		return
	}
	if info.Size() > maxFileBytes {
		httpError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file exceeds %d bytes", maxFileBytes))
		return
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if looksBinary(b) {
		writeJSON(w, http.StatusOK, readResponse{Type: "binary", Size: info.Size(), Modified: info.ModTime()})
		return
	}
	writeJSON(w, http.StatusOK, readResponse{
		Type: "file", Content: string(b), Hash: contentHash(b),
		Size: info.Size(), Modified: info.ModTime(),
	})
}

func (s *Server) fileWrite(w http.ResponseWriter, r *http.Request) {
	abs, rel, ok := s.resolveFilePath(w, r)
	if !ok {
		return
	}
	if rel == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileBytes+4096)
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Never write through a symlink: the link target was vetted at resolve
	// time, but replacing content through a link is still surprising.
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			httpError(w, http.StatusConflict, "refusing to write through a symlink")
			return
		}
		if info.IsDir() {
			httpError(w, http.StatusConflict, "path is a directory")
			return
		}
		if req.BaseHash == "" && !req.Force {
			httpError(w, http.StatusConflict, "file exists")
			return
		}
		if req.BaseHash != "" && !req.Force {
			cur, err := os.ReadFile(abs)
			if err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if contentHash(cur) != req.BaseHash {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error":   "file changed on disk",
					"hash":    contentHash(cur),
					"content": string(cur),
				})
				return
			}
		}
	} else if req.BaseHash != "" && !req.Force {
		// Client thinks it's updating an existing file that is gone.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "file deleted on disk",
		})
		return
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Temp file + rename keeps a concurrently reading agent from ever seeing
	// a half-written file.
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".editor-write-*")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(req.Content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	os.Chmod(tmpName, 0o644)
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"hash": contentHash([]byte(req.Content))})
}

func (s *Server) fileOp(w http.ResponseWriter, r *http.Request) {
	abs, rel, ok := s.resolveFilePath(w, r)
	if !ok {
		return
	}
	if rel == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}
	var req fileOpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch req.Op {
	case "mkdir":
		if _, err := os.Lstat(abs); err == nil {
			httpError(w, http.StatusConflict, "path exists")
			return
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{})
	case "rename":
		id, _ := pathID(w, r) // already validated by resolveFilePath
		root, err := s.workspaceRoot(r, id)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if isLeasePath(req.To) {
			httpError(w, http.StatusForbidden, "lease file is not editable")
			return
		}
		dst, err := securePath(root, req.To)
		if err != nil {
			httpError(w, http.StatusBadRequest, "to: "+err.Error())
			return
		}
		if req.To == "" || dst == root {
			httpError(w, http.StatusBadRequest, "to required")
			return
		}
		if _, err := os.Lstat(abs); err != nil {
			httpError(w, http.StatusNotFound, "not found")
			return
		}
		if _, err := os.Lstat(dst); err == nil {
			httpError(w, http.StatusConflict, "destination exists")
			return
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.Rename(abs, dst); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{})
	default:
		httpError(w, http.StatusBadRequest, `op must be "mkdir" or "rename"`)
	}
}

func (s *Server) fileDelete(w http.ResponseWriter, r *http.Request) {
	abs, rel, ok := s.resolveFilePath(w, r)
	if !ok {
		return
	}
	if rel == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}
	info, err := os.Lstat(abs)
	if err != nil {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if info.IsDir() {
		if r.URL.Query().Get("recursive") == "1" {
			if err := os.RemoveAll(abs); err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
		} else if err := os.Remove(abs); err != nil {
			httpError(w, http.StatusConflict, "directory not empty (use ?recursive=1)")
			return
		}
	} else if err := os.Remove(abs); err != nil { // files and symlinks: removes the link, never the target
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
