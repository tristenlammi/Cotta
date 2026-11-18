package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"cotta/internal/session"
)

// File access runs over a session's existing SSH connection (SFTP), so the
// Files view never logs in a second time.

const (
	// Uploads arrive in chunks so no single request is large; proxies such
	// as Cloudflare cap request bodies (100 MB on the lower plans).
	maxUploadChunk = 32 << 20
	uploadSuffix   = ".cotta-upload"
)

type fileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"` // e.g. rwxr-xr-x
	IsDir   bool      `json:"isDir"`
	IsLink  bool      `json:"isLink"`
	ModTime time.Time `json:"modTime"`
}

func (a *API) registerFiles(authed func(string, http.HandlerFunc)) {
	authed("GET /api/sessions/{id}/files", a.listFiles)
	authed("GET /api/sessions/{id}/files/download", a.downloadFile)
	authed("POST /api/sessions/{id}/files/upload", a.uploadChunk)
	authed("POST /api/sessions/{id}/files/mkdir", a.makeDir)
	authed("POST /api/sessions/{id}/files/rename", a.renameFile)
	authed("POST /api/sessions/{id}/files/delete", a.deleteFiles)
}

// sftpFor finds the session and its SFTP client, writing an error response
// and returning nil when either is unavailable.
func (a *API) sftpFor(w http.ResponseWriter, r *http.Request) *sftp.Client {
	s := a.Sessions.Get(r.PathValue("id"))
	if s == nil {
		fail(w, http.StatusNotFound, "session_ended", "That session has ended.")
		return nil
	}
	c, err := s.SFTP()
	if errors.Is(err, session.ErrEnded) {
		fail(w, http.StatusNotFound, "session_ended", "That session has ended.")
		return nil
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "sftp_unavailable", "This server doesn't offer file access (SFTP). The terminal still works.")
		return nil
	}
	return c
}

// fileError turns an SFTP error into a response the UI can show.
func fileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		fail(w, http.StatusNotFound, "not_found", "That file or folder doesn't exist on the server.")
	case errors.Is(err, os.ErrPermission):
		fail(w, http.StatusForbidden, "denied", "The server refused: permission denied.")
	default:
		fail(w, http.StatusBadGateway, "file_error", "The server reported an error: "+err.Error())
	}
}

// validName accepts a single path component.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func (a *API) listFiles(w http.ResponseWriter, r *http.Request) {
	c := a.sftpFor(w, r)
	if c == nil {
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		p = "." // the login directory
	}
	real, err := c.RealPath(p)
	if err != nil {
		fileError(w, err)
		return
	}
	infos, err := c.ReadDir(real)
	if err != nil {
		fileError(w, err)
		return
	}
	entries := make([]fileEntry, 0, len(infos))
	for _, fi := range infos {
		e := fileEntry{
			Name: fi.Name(), Size: fi.Size(), ModTime: fi.ModTime(),
			Mode:  fi.Mode().Perm().String()[1:], // drop the leading type character
			IsDir: fi.IsDir(), IsLink: fi.Mode()&os.ModeSymlink != 0,
		}
		if e.IsLink {
			// Follow the link so a link to a folder can be opened.
			if target, err := c.Stat(path.Join(real, fi.Name())); err == nil {
				e.IsDir, e.Size = target.IsDir(), target.Size()
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": real, "entries": entries})
}

func (a *API) downloadFile(w http.ResponseWriter, r *http.Request) {
	c := a.sftpFor(w, r)
	if c == nil {
		return
	}
	p := r.URL.Query().Get("path")
	fi, err := c.Stat(p)
	if err != nil {
		fileError(w, err)
		return
	}
	if fi.IsDir() {
		fail(w, http.StatusBadRequest, "is_folder", "Folders can't be downloaded yet. Download the files inside it.")
		return
	}
	f, err := c.Open(p)
	if err != nil {
		fileError(w, err)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)}))
	h.Set("Cache-Control", "no-store")
	io.Copy(w, f) // sftp.File streams with pipelined reads
}

// uploadChunk appends one chunk of an upload. The file is written under a
// temporary name and only takes its real name once the final chunk lands,
// so a half-finished upload never replaces a good file.
func (a *API) uploadChunk(w http.ResponseWriter, r *http.Request) {
	c := a.sftpFor(w, r)
	if c == nil {
		return
	}
	q := r.URL.Query()
	dir, name := q.Get("dir"), q.Get("name")
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 || dir == "" || !validName(name) {
		fail(w, http.StatusBadRequest, "bad_request", "That upload request is malformed.")
		return
	}
	target := path.Join(dir, name)
	tmp := path.Join(dir, "."+name+uploadSuffix)

	var f *sftp.File
	if offset == 0 {
		if q.Get("overwrite") != "1" {
			if _, err := c.Lstat(target); err == nil {
				fail(w, http.StatusConflict, "exists", name+" already exists in that folder.")
				return
			}
		}
		f, err = c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	} else {
		st, serr := c.Stat(tmp)
		if serr != nil || st.Size() != offset {
			fail(w, http.StatusConflict, "out_of_sync", "The upload lost its place. Start it again.")
			return
		}
		if f, err = c.OpenFile(tmp, os.O_WRONLY); err == nil {
			_, err = f.Seek(offset, io.SeekStart)
		}
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		fileError(w, err)
		return
	}

	n, err := f.ReadFromWithConcurrency(http.MaxBytesReader(w, r.Body, maxUploadChunk), 16)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		c.Remove(tmp)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, "chunk_too_large", "That upload chunk is too large.")
			return
		}
		fileError(w, err)
		return
	}

	if q.Get("final") == "1" {
		// PosixRename replaces the target atomically where the server
		// supports it; otherwise fall back to remove-then-rename.
		if err := c.PosixRename(tmp, target); err != nil {
			c.Remove(target)
			if err := c.Rename(tmp, target); err != nil {
				c.Remove(tmp)
				fileError(w, err)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"received": offset + n})
}

func (a *API) makeDir(w http.ResponseWriter, r *http.Request) {
	c := a.sftpFor(w, r)
	if c == nil {
		return
	}
	var b struct{ Dir, Name string }
	if !readJSON(w, r, &b) {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Dir == "" || !validName(b.Name) {
		fail(w, http.StatusUnprocessableEntity, "invalid", "A folder name can't be empty or contain a slash.")
		return
	}
	if err := c.Mkdir(path.Join(b.Dir, b.Name)); err != nil {
		if _, serr := c.Lstat(path.Join(b.Dir, b.Name)); serr == nil {
			fail(w, http.StatusConflict, "exists", b.Name+" already exists in that folder.")
			return
		}
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) renameFile(w http.ResponseWriter, r *http.Request) {
	c := a.sftpFor(w, r)
	if c == nil {
		return
	}
	var b struct{ Dir, From, To string }
	if !readJSON(w, r, &b) {
		return
	}
	b.To = strings.TrimSpace(b.To)
	if b.Dir == "" || !validName(b.From) || !validName(b.To) {
		fail(w, http.StatusUnprocessableEntity, "invalid", "A name can't be empty or contain a slash.")
		return
	}
	if _, err := c.Lstat(path.Join(b.Dir, b.To)); err == nil {
		fail(w, http.StatusConflict, "exists", b.To+" already exists in that folder.")
		return
	}
	if err := c.Rename(path.Join(b.Dir, b.From), path.Join(b.Dir, b.To)); err != nil {
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) deleteFiles(w http.ResponseWriter, r *http.Request) {
	c := a.sftpFor(w, r)
	if c == nil {
		return
	}
	var b struct {
		Dir   string
		Names []string
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Dir == "" || len(b.Names) == 0 {
		fail(w, http.StatusBadRequest, "bad_request", "Nothing to delete.")
		return
	}
	for _, name := range b.Names {
		if !validName(name) {
			fail(w, http.StatusBadRequest, "bad_request", "That delete request is malformed.")
			return
		}
	}
	for _, name := range b.Names {
		p := path.Join(b.Dir, name)
		fi, err := c.Lstat(p)
		if err == nil {
			if fi.IsDir() {
				err = c.RemoveAll(p) // folders are deleted with everything in them
			} else {
				err = c.Remove(p) // a link is removed, not what it points to
			}
		}
		if err != nil {
			fileError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
