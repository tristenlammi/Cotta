package api_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

// startSession sets up a vault, a trusted password host and a running
// session, returning the session's API prefix.
func startSession(t *testing.T, h *harness) string {
	t.Helper()
	h.do("POST", "/api/setup", map[string]any{"passphrase": "correct horse battery"})
	host, port := h.sshHostPort()
	_, probe := h.do("POST", "/api/probe", map[string]any{"host": host, "port": port})
	_, saved := h.do("POST", "/api/hosts", map[string]any{"user": "demo", "host": host, "port": port, "authType": "password", "password": "s3cret-pw", "hostKey": probe["hostKey"]})
	code, sess := h.do("POST", "/api/sessions", map[string]any{"hostId": saved["id"]})
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %v", code, sess)
	}
	return "/api/sessions/" + sess["id"].(string)
}

// upload sends one chunk the way the frontend does.
func (h *harness) upload(prefix, dir, name string, offset int64, final, overwrite bool, data []byte) (int, map[string]any) {
	h.t.Helper()
	q := url.Values{"dir": {dir}, "name": {name}, "offset": {strconv.FormatInt(offset, 10)}}
	if final {
		q.Set("final", "1")
	}
	if overwrite {
		q.Set("overwrite", "1")
	}
	req, _ := http.NewRequest("POST", h.srv.URL+prefix+"/files/upload?"+q.Encode(), bytes.NewReader(data))
	req.Header.Set("X-Cotta", "1")
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	decodeInto(res.Body, &out)
	return res.StatusCode, out
}

func (h *harness) names(prefix, dir string) []string {
	h.t.Helper()
	code, out := h.do("GET", prefix+"/files?path="+url.QueryEscape(dir), nil)
	if code != http.StatusOK {
		h.t.Fatalf("list %s: %d %v", dir, code, out)
	}
	var names []string
	for _, e := range out["entries"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	return names
}

func TestFiles(t *testing.T) {
	h := newHarness(t)
	prefix := startSession(t, h)

	// The login directory resolves to an absolute path.
	code, home := h.do("GET", prefix+"/files", nil)
	if code != http.StatusOK || home["path"] != "/" {
		t.Fatalf("home listing: %d %v", code, home)
	}

	if code, out := h.do("POST", prefix+"/files/mkdir", map[string]any{"dir": "/", "name": "site"}); code != http.StatusOK {
		t.Fatalf("mkdir: %d %v", code, out)
	}
	if code, out := h.do("POST", prefix+"/files/mkdir", map[string]any{"dir": "/", "name": "site"}); code != http.StatusConflict {
		t.Fatalf("mkdir twice: got %d %v, want 409", code, out)
	}
	if code, _ := h.do("POST", prefix+"/files/mkdir", map[string]any{"dir": "/", "name": "a/b"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("mkdir with slash: got %d, want 422", code)
	}

	// A file sent in three chunks arrives intact, and is invisible under
	// its real name until the last chunk lands.
	payload := make([]byte, 300_000)
	rand.Read(payload)
	if code, out := h.upload(prefix, "/site", "build.zip", 0, false, false, payload[:100_000]); code != http.StatusOK {
		t.Fatalf("chunk 1: %d %v", code, out)
	}
	if got := h.names(prefix, "/site"); len(got) != 1 || got[0] != ".build.zip.cotta-upload" {
		t.Fatalf("mid-upload listing: %v", got)
	}
	if code, out := h.upload(prefix, "/site", "build.zip", 50_000, false, false, payload[100_000:200_000]); code != http.StatusConflict || out["code"] != "out_of_sync" {
		t.Fatalf("chunk at the wrong offset: %d %v", code, out)
	}
	h.upload(prefix, "/site", "build.zip", 100_000, false, false, payload[100_000:200_000])
	if code, out := h.upload(prefix, "/site", "build.zip", 200_000, true, false, payload[200_000:]); code != http.StatusOK || out["received"] != float64(300_000) {
		t.Fatalf("final chunk: %d %v", code, out)
	}
	if got := h.names(prefix, "/site"); len(got) != 1 || got[0] != "build.zip" {
		t.Fatalf("listing after upload: %v", got)
	}

	res, err := h.client.Get(h.srv.URL + prefix + "/files/download?path=" + url.QueryEscape("/site/build.zip"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("download: status %d, %d bytes, equal=%v", res.StatusCode, len(got), bytes.Equal(got, payload))
	}
	if cd := res.Header.Get("Content-Disposition"); cd != `attachment; filename=build.zip` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// An existing file is only replaced when asked.
	if code, out := h.upload(prefix, "/site", "build.zip", 0, true, false, []byte("new")); code != http.StatusConflict || out["code"] != "exists" {
		t.Fatalf("upload over existing: %d %v", code, out)
	}
	if code, out := h.upload(prefix, "/site", "build.zip", 0, true, true, []byte("new")); code != http.StatusOK {
		t.Fatalf("upload with overwrite: %d %v", code, out)
	}
	res, _ = h.client.Get(h.srv.URL + prefix + "/files/download?path=" + url.QueryEscape("/site/build.zip"))
	got, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(got) != "new" {
		t.Fatalf("after overwrite the file holds %q", got)
	}

	// Rename, then delete a folder with its contents.
	h.upload(prefix, "/site", "index.html", 0, true, false, []byte("<h1>hi</h1>"))
	if code, out := h.do("POST", prefix+"/files/rename", map[string]any{"dir": "/site", "from": "index.html", "to": "build.zip"}); code != http.StatusConflict {
		t.Fatalf("rename onto existing: got %d %v, want 409", code, out)
	}
	if code, out := h.do("POST", prefix+"/files/rename", map[string]any{"dir": "/site", "from": "index.html", "to": "home.html"}); code != http.StatusOK {
		t.Fatalf("rename: %d %v", code, out)
	}
	if got := h.names(prefix, "/site"); len(got) != 2 || got[0] != "build.zip" || got[1] != "home.html" {
		t.Fatalf("listing after rename: %v", got)
	}
	if code, out := h.do("POST", prefix+"/files/delete", map[string]any{"dir": "/", "names": []string{"site"}}); code != http.StatusOK {
		t.Fatalf("delete folder: %d %v", code, out)
	}
	if got := h.names(prefix, "/"); len(got) != 0 {
		t.Fatalf("listing after delete: %v", got)
	}

	if code, out := h.do("GET", prefix+"/files?path=/nope", nil); code != http.StatusNotFound || out["code"] != "not_found" {
		t.Fatalf("missing folder: %d %v", code, out)
	}
	if code, out := h.do("GET", "/api/sessions/doesnotexist/files", nil); code != http.StatusNotFound || out["code"] != "session_ended" {
		t.Fatalf("missing session: %d %v", code, out)
	}
}

func TestFilesUnavailable(t *testing.T) {
	h := newHarness(t)
	h.ssh.NoSFTP = true
	prefix := startSession(t, h)
	if code, out := h.do("GET", prefix+"/files", nil); code != http.StatusBadGateway || out["code"] != "sftp_unavailable" {
		t.Fatalf("expected sftp_unavailable, got %d %v", code, out)
	}
}
