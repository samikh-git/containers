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
	"io/fs"
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
	mux.HandleFunc("PUT /api/workspaces/{id}/files/{path...}", s.authJSON(s.fileWrite))
	mux.HandleFunc("POST /api/workspaces/{id}/files/{path...}", s.authJSON(s.fileOp))
	mux.HandleFunc("POST /api/workspaces/{id}/files", s.authJSON(s.fileOp))
	mux.HandleFunc("DELETE /api/workspaces/{id}/files/{path...}", s.auth(s.fileDelete))
}

// workspaceRoot resolves a workspace id to its mount path. EnsureWorkspace is
// idempotent by contract; no s.mu here — file IO must not serialize behind
// provisioning.
func (s *Server) workspaceRoot(r *http.Request, id string) (string, error) {
	return s.Router.Storage.EnsureWorkspace(r.Context(), id, s.Defaults.QuotaGB)
}

// cleanRel turns the {path...} segment into a root-relative name for os.Root.
// Lexical cleaning only — containment is the kernel's job below, not this
// function's. "" and "/" both mean the workspace root itself (".").
func cleanRel(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", errors.New("invalid path")
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", errors.New("path must be relative")
	}
	clean := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(rel)), "/")
	if clean == "" {
		return ".", nil
	}
	return clean, nil
}

// openWorkspace returns an os.Root confined to the workspace mount. Every
// file operation goes through it, so path resolution — including each symlink
// hop — is enforced by the kernel at the moment of use. That matters because
// the workspace is agent-writable and the agent is the adversary: a check
// that resolves a name and then acts on it can be beaten by swapping a
// directory for a symlink in between, and this design has no such window.
// The caller must Close the returned root.
func (s *Server) openWorkspace(r *http.Request, id string) (*os.Root, error) {
	mount, err := s.workspaceRoot(r, id)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(mount)
}

// resolveFile pulls {path...} off the request and opens the workspace root.
// ok=false means the response has been written; otherwise the caller owns
// root and must Close it.
func (s *Server) resolveFile(w http.ResponseWriter, r *http.Request) (root *os.Root, rel string, ok bool) {
	id, idOK := pathID(w, r)
	if !idOK {
		return nil, "", false
	}
	rel, err := cleanRel(r.PathValue("path"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return nil, "", false
	}
	if isLeasePath(rel) {
		// The lease file is fencing metadata (dataplane.FenceAndWrite), not
		// workspace content; the editor must never read or clobber it.
		httpError(w, http.StatusForbidden, "lease file is not editable")
		return nil, "", false
	}
	root, err = s.openWorkspace(r, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return nil, "", false
	}
	return root, rel, true
}

func isLeasePath(rel string) bool {
	clean, err := cleanRel(rel)
	return err == nil && clean == ".lease"
}

// fileErr maps an os.Root error onto a status. Anything the kernel refused to
// resolve inside the workspace — a "..", an escaping symlink, a path swapped
// under us mid-operation — is a client error, not a server fault.
func fileErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		httpError(w, http.StatusNotFound, "not found")
	case errors.Is(err, os.ErrInvalid), isEscape(err):
		httpError(w, http.StatusBadRequest, "path escapes workspace")
	default:
		httpError(w, http.StatusInternalServerError, err.Error())
	}
}

// isEscape recognizes os.Root's containment refusal ("path escapes from
// parent"), which the standard library exposes as a plain error with no
// sentinel to match on.
func isEscape(err error) bool {
	return err != nil && strings.Contains(err.Error(), "escapes")
}

// ensureParent creates rel's parent directory if it is missing. Stat first,
// so an existing-but-unusable parent (a symlink out of the workspace) reports
// the escape rather than MkdirAll's bare "file exists".
func ensureParent(root *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	if _, err := root.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return root.MkdirAll(dir, 0o755)
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
	root, rel, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil {
		fileErr(w, err)
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Root.Stat follows the link and refuses it if it leaves the
		// workspace, so a planted `ln -s / pwn` reads as an escape, not as /.
		if info, err = root.Stat(rel); err != nil {
			fileErr(w, err)
			return
		}
	}
	if info.IsDir() {
		dir, err := root.Open(rel)
		if err != nil {
			fileErr(w, err)
			return
		}
		entries, err := dir.ReadDir(-1)
		dir.Close()
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		atRoot := rel == "."
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
	b, err := root.ReadFile(rel)
	if err != nil {
		fileErr(w, err)
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
	root, rel, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	defer root.Close()
	if rel == "." {
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
	if info, err := root.Lstat(rel); err == nil {
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
			cur, err := root.ReadFile(rel)
			if err != nil {
				fileErr(w, err)
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

	if err := ensureParent(root, rel); err != nil {
		fileErr(w, err)
		return
	}
	// Temp file + rename keeps a concurrently reading agent from ever seeing
	// a half-written file. O_EXCL so we never land on a name the agent
	// planted; both steps go through root, so neither can be redirected out
	// of the workspace by a symlink swapped in mid-write.
	tmpName := path.Join(path.Dir(rel), fmt.Sprintf(".editor-write-%d", time.Now().UnixNano()))
	tmp, err := root.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		fileErr(w, err)
		return
	}
	if _, err := tmp.WriteString(req.Content); err != nil {
		tmp.Close()
		root.Remove(tmpName)
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tmp.Close(); err != nil {
		root.Remove(tmpName)
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := root.Rename(tmpName, rel); err != nil {
		root.Remove(tmpName)
		fileErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"hash": contentHash([]byte(req.Content))})
}

func (s *Server) fileOp(w http.ResponseWriter, r *http.Request) {
	root, rel, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	defer root.Close()
	if rel == "." {
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
		if _, err := root.Lstat(rel); err == nil {
			httpError(w, http.StatusConflict, "path exists")
			return
		}
		if err := root.MkdirAll(rel, 0o755); err != nil {
			fileErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{})
	case "rename":
		if isLeasePath(req.To) {
			httpError(w, http.StatusForbidden, "lease file is not editable")
			return
		}
		dst, err := cleanRel(req.To)
		if err != nil {
			httpError(w, http.StatusBadRequest, "to: "+err.Error())
			return
		}
		if req.To == "" || dst == "." {
			httpError(w, http.StatusBadRequest, "to required")
			return
		}
		if _, err := root.Lstat(rel); err != nil {
			fileErr(w, err)
			return
		}
		if _, err := root.Lstat(dst); err == nil {
			httpError(w, http.StatusConflict, "destination exists")
			return
		}
		if err := ensureParent(root, dst); err != nil {
			fileErr(w, err)
			return
		}
		if err := root.Rename(rel, dst); err != nil {
			fileErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{})
	default:
		httpError(w, http.StatusBadRequest, `op must be "mkdir" or "rename"`)
	}
}

func (s *Server) fileDelete(w http.ResponseWriter, r *http.Request) {
	root, rel, ok := s.resolveFile(w, r)
	if !ok {
		return
	}
	defer root.Close()
	if rel == "." {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}
	info, err := root.Lstat(rel)
	if err != nil {
		fileErr(w, err)
		return
	}
	if info.IsDir() {
		if r.URL.Query().Get("recursive") == "1" {
			if err := root.RemoveAll(rel); err != nil {
				fileErr(w, err)
				return
			}
		} else if err := root.Remove(rel); err != nil {
			httpError(w, http.StatusConflict, "directory not empty (use ?recursive=1)")
			return
		}
	} else if err := root.Remove(rel); err != nil { // files and symlinks: removes the link, never the target
		fileErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
