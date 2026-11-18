package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"cotta/internal/api"
	"cotta/internal/auth"
	"cotta/internal/session"
	"cotta/internal/store"
	"cotta/internal/testssh"
	"cotta/internal/vault"
)

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	ssh    *testssh.Server
	mgr    *session.Manager
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cotta.db"))
	if err != nil {
		t.Fatal(err)
	}
	v := vault.New(st)
	mgr := session.NewManager()
	a := &api.API{Store: st, Vault: v, Auth: auth.New(v, time.Hour), Sessions: mgr, Static: fstest.MapFS{"index.html": {Data: []byte("ok")}}}
	srv := httptest.NewServer(a.Handler())
	sshd, err := testssh.Start("127.0.0.1:0", "demo", "s3cret-pw")
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	t.Cleanup(func() { mgr.CloseAll(); srv.Close(); sshd.Close(); st.Close() })
	return &harness{t: t, srv: srv, client: &http.Client{Jar: jar}, ssh: sshd, mgr: mgr}
}

// do sends a JSON request the way the frontend does and decodes the reply.
func (h *harness) do(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, &buf)
	req.Header.Set("X-Cotta", "1")
	req.Header.Set("Content-Type", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	var raw any
	json.NewDecoder(res.Body).Decode(&raw)
	switch v := raw.(type) {
	case map[string]any:
		out = v
	case []any:
		out["list"] = v
	}
	return res.StatusCode, out
}

func (h *harness) sshHostPort() (string, int) {
	host, port, _ := net.SplitHostPort(h.ssh.Addr)
	p, _ := strconv.Atoi(port)
	return host, p
}

// attach opens the session WebSocket and returns it plus a reader that
// accumulates terminal output until want appears.
func (h *harness) attach(id string) (*websocket.Conn, func(want string) string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	h.t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/sessions/" + id + "/ws"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: h.client})
	if err != nil {
		h.t.Fatalf("ws dial: %v", err)
	}
	var seen strings.Builder
	return c, func(want string) string {
		h.t.Helper()
		for !strings.Contains(seen.String(), want) {
			typ, data, err := c.Read(ctx)
			if err != nil {
				h.t.Fatalf("waiting for %q, got so far %q: %v", want, seen.String(), err)
			}
			if typ == websocket.MessageBinary {
				seen.Write(data)
			}
		}
		return seen.String()
	}
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Nothing works before the vault exists and is unlocked.
	if code, _ := h.do("GET", "/api/hosts", nil); code != http.StatusUnauthorized {
		t.Fatalf("hosts before setup: got %d, want 401", code)
	}
	if code, _ := h.do("POST", "/api/setup", map[string]any{"passphrase": "short"}); code != http.StatusBadRequest {
		t.Fatalf("weak passphrase: got %d, want 400", code)
	}
	if code, body := h.do("POST", "/api/setup", map[string]any{"passphrase": "correct horse battery"}); code != http.StatusOK {
		t.Fatalf("setup: %d %v", code, body)
	}

	// Save a server with a stored password, without trusting its key yet.
	host, port := h.sshHostPort()
	code, saved := h.do("POST", "/api/hosts", map[string]any{"name": "demo", "user": "demo", "host": host, "port": port, "authType": "password", "password": "s3cret-pw"})
	if code != http.StatusCreated {
		t.Fatalf("create host: %d %v", code, saved)
	}
	if saved["hasPassword"] != true || saved["trusted"] != false {
		t.Fatalf("unexpected host: %v", saved)
	}
	hostID := saved["id"]

	// First connect is refused until the fingerprint is trusted.
	code, hk := h.do("POST", "/api/sessions", map[string]any{"hostId": hostID, "cols": 80, "rows": 24})
	if code != http.StatusConflict || hk["code"] != "hostkey_unknown" || hk["fingerprint"] == "" {
		t.Fatalf("expected hostkey_unknown, got %d %v", code, hk)
	}
	if code, _ := h.do("POST", "/api/hosts/"+jsonID(hostID)+"/trust", map[string]any{"hostKey": hk["hostKey"]}); code != http.StatusOK {
		t.Fatalf("trust: %d", code)
	}
	code, sess := h.do("POST", "/api/sessions", map[string]any{"hostId": hostID, "cols": 80, "rows": 24})
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %v", code, sess)
	}
	id := sess["id"].(string)

	// Type a command and see it echoed.
	c, wait := h.attach(id)
	wait("$ ")
	c.Write(ctx, websocket.MessageBinary, []byte("hello cotta\r"))
	wait("you said: hello cotta")
	c.Close(websocket.StatusNormalClosure, "")

	// Closing the browser leaves the session running; a new attach replays it.
	time.Sleep(100 * time.Millisecond)
	_, list := h.do("GET", "/api/sessions", nil)
	if l := list["list"].([]any); len(l) != 1 || l[0].(map[string]any)["lastLine"] != "demo@testssh:~$" {
		t.Fatalf("session list after detach: %v", list)
	}
	c2, wait2 := h.attach(id)
	if replay := wait2("you said: hello cotta"); !strings.Contains(replay, "Cotta test shell") {
		t.Fatalf("replay is missing the start of the session: %q", replay)
	}

	// Locking signs browsers out and kicks sockets, but the shell survives.
	if code, _ := h.do("POST", "/api/lock", nil); code != http.StatusOK {
		t.Fatalf("lock: %d", code)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	for {
		if _, _, err := c2.Read(rctx); err != nil {
			if websocket.CloseStatus(err) != 4001 {
				t.Fatalf("after lock, socket closed with %v, want code 4001", err)
			}
			break
		}
	}
	cancel()
	if code, _ := h.do("GET", "/api/sessions", nil); code != http.StatusUnauthorized {
		t.Fatalf("sessions while locked: got %d, want 401", code)
	}
	if code, _ := h.do("POST", "/api/unlock", map[string]any{"passphrase": "wrong wrong wrong"}); code != http.StatusForbidden {
		t.Fatalf("wrong passphrase: got %d, want 403", code)
	}
	if code, _ := h.do("POST", "/api/unlock", map[string]any{"passphrase": "correct horse battery"}); code != http.StatusOK {
		t.Fatalf("unlock: %d", code)
	}
	c3, wait3 := h.attach(id)
	wait3("you said: hello cotta")

	// Exiting the shell ends the session and tells the browser not to retry.
	c3.Write(ctx, websocket.MessageBinary, []byte("exit\r"))
	rctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c3.Read(rctx); err != nil {
			if websocket.CloseStatus(err) != 4000 {
				t.Fatalf("after exit, socket closed with %v, want code 4000", err)
			}
			break
		}
	}
	if _, list := h.do("GET", "/api/sessions", nil); len(list["list"].([]any)) != 0 {
		t.Fatalf("session still listed after exit: %v", list)
	}

	// A changed host key is refused with both fingerprints.
	h.ssh.Rekey()
	code, changed := h.do("POST", "/api/sessions", map[string]any{"hostId": hostID})
	if code != http.StatusConflict || changed["code"] != "hostkey_changed" || changed["pinnedFingerprint"] != hk["fingerprint"] || changed["fingerprint"] == hk["fingerprint"] {
		t.Fatalf("expected hostkey_changed, got %d %v", code, changed)
	}
}

func TestWrongPasswordAndAskEachTime(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/setup", map[string]any{"passphrase": "correct horse battery"})
	host, port := h.sshHostPort()
	_, probe := h.do("POST", "/api/probe", map[string]any{"host": host, "port": port})
	if probe["reachable"] != true {
		t.Fatalf("probe: %v", probe)
	}
	_, saved := h.do("POST", "/api/hosts", map[string]any{"user": "demo", "host": host, "port": port, "authType": "ask", "hostKey": probe["hostKey"]})
	if saved["trusted"] != true || saved["fingerprint"] != probe["fingerprint"] {
		t.Fatalf("host should be pinned from the probe: %v", saved)
	}
	if code, body := h.do("POST", "/api/sessions", map[string]any{"hostId": saved["id"]}); code != http.StatusUnprocessableEntity || body["code"] != "password_required" {
		t.Fatalf("expected password_required, got %d %v", code, body)
	}
	if code, body := h.do("POST", "/api/sessions", map[string]any{"hostId": saved["id"], "password": "nope"}); code != http.StatusUnprocessableEntity || body["code"] != "auth_failed" {
		t.Fatalf("expected auth_failed, got %d %v", code, body)
	}
	if code, body := h.do("POST", "/api/sessions", map[string]any{"hostId": saved["id"], "password": "s3cret-pw"}); code != http.StatusCreated {
		t.Fatalf("expected a session, got %d %v", code, body)
	}
}

func TestKeySignIn(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/setup", map[string]any{"passphrase": "correct horse battery"})
	host, port := h.sshHostPort()
	_, probe := h.do("POST", "/api/probe", map[string]any{"host": host, "port": port})

	code, key := h.do("POST", "/api/keys", map[string]any{"name": "test key", "generate": true})
	if code != http.StatusCreated || !strings.HasPrefix(key["publicKey"].(string), "ssh-ed25519 ") {
		t.Fatalf("generate key: %d %v", code, key)
	}
	code, saved := h.do("POST", "/api/hosts", map[string]any{"user": "demo", "host": host, "port": port, "authType": "key", "keyId": key["id"], "hostKey": probe["hostKey"]})
	if code != http.StatusCreated {
		t.Fatalf("create host: %d %v", code, saved)
	}

	// The server doesn't know the key yet.
	if code, body := h.do("POST", "/api/sessions", map[string]any{"hostId": saved["id"]}); body["code"] != "auth_failed" {
		t.Fatalf("expected auth_failed before authorizing, got %d %v", code, body)
	}
	h.ssh.Authorize(key["publicKey"].(string))
	code, sess := h.do("POST", "/api/sessions", map[string]any{"hostId": saved["id"]})
	if code != http.StatusCreated {
		t.Fatalf("key sign-in: %d %v", code, sess)
	}
	_, wait := h.attach(sess["id"].(string))
	wait("$ ")

	// Text that isn't a private key is rejected on import.
	if code, body := h.do("POST", "/api/keys", map[string]any{"name": "junk", "privateKey": "not a key"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("junk key: %d %v", code, body)
	}
}

func TestSavedCommands(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/setup", map[string]any{"passphrase": "correct horse battery"})
	mkHost := func(name string) any {
		_, saved := h.do("POST", "/api/hosts", map[string]any{"name": name, "user": "demo", "host": "192.168.50.10", "authType": "ask"})
		return saved["id"]
	}
	a, b := mkHost("a"), mkHost("b")
	list := func(hostID any) []any {
		_, out := h.do("GET", "/api/commands?hostId="+jsonID(hostID), nil)
		l, _ := out["list"].([]any)
		return l
	}

	code, mine := h.do("POST", "/api/commands", map[string]any{"hostId": a, "label": "Restart", "command": "sudo systemctl restart nginx\r\n"})
	if code != http.StatusCreated || mine["command"] != "sudo systemctl restart nginx" {
		t.Fatalf("create: %d %v", code, mine)
	}
	h.do("POST", "/api/commands", map[string]any{"hostId": nil, "command": "df -h"})
	if code, _ := h.do("POST", "/api/commands", map[string]any{"hostId": a, "command": "   "}); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty command: got %d, want 422", code)
	}

	// Server a sees its own command and the shared one; b only the shared one.
	if l := list(a); len(l) != 2 {
		t.Fatalf("commands for a: %v", l)
	}
	if l := list(b); len(l) != 1 || l[0].(map[string]any)["command"] != "df -h" {
		t.Fatalf("commands for b: %v", l)
	}

	code, edited := h.do("PUT", "/api/commands/"+jsonID(mine["id"]), map[string]any{"hostId": a, "label": "Reload", "command": "sudo systemctl reload nginx"})
	if code != http.StatusOK || edited["label"] != "Reload" {
		t.Fatalf("update: %d %v", code, edited)
	}

	// Deleting a server removes its commands but not the shared ones.
	h.do("DELETE", "/api/hosts/"+jsonID(a), nil)
	if l := list(a); len(l) != 1 {
		t.Fatalf("after deleting the server: %v", l)
	}
}

func TestCrossSiteRequestsRefused(t *testing.T) {
	h := newHarness(t)
	post := func(setHeader func(*http.Request)) int {
		req, _ := http.NewRequest("POST", h.srv.URL+"/api/setup", strings.NewReader(`{"passphrase":"correct horse battery"}`))
		setHeader(req)
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post(func(*http.Request) {}); code != http.StatusForbidden {
		t.Errorf("no X-Cotta header: got %d, want 403", code)
	}
	if code := post(func(r *http.Request) { r.Header.Set("X-Cotta", "1"); r.Header.Set("Origin", "https://evil.example") }); code != http.StatusForbidden {
		t.Errorf("foreign Origin: got %d, want 403", code)
	}
}

func decodeInto(r io.Reader, out *map[string]any) { json.NewDecoder(r).Decode(out) }

func jsonID(v any) string { return strconv.FormatInt(int64(v.(float64)), 10) }
