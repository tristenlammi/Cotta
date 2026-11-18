// Package api is the HTTP and WebSocket surface the browser talks to.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"cotta/internal/auth"
	"cotta/internal/parse"
	"cotta/internal/session"
	"cotta/internal/sshx"
	"cotta/internal/store"
	"cotta/internal/vault"
)

type API struct {
	Store      *store.Store
	Vault      *vault.Vault
	Auth       *auth.Auth
	Sessions   *session.Manager
	Static     fs.FS
	Cloudflare bool
}

const (
	purposePassword = "host-password"
	purposeKey      = "key-private"
)

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/setup", a.setup)
	mux.HandleFunc("POST /api/unlock", a.unlock)

	authed := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, a.Auth.Require(h)) }
	authed("POST /api/lock", a.lock)
	authed("POST /api/parse", a.parse)
	authed("POST /api/probe", a.probe)
	authed("GET /api/hosts", a.listHosts)
	authed("POST /api/hosts", a.createHost)
	authed("PUT /api/hosts/{id}", a.updateHost)
	authed("DELETE /api/hosts/{id}", a.deleteHost)
	authed("POST /api/hosts/{id}/trust", a.trustHost)
	authed("GET /api/keys", a.listKeys)
	authed("POST /api/keys", a.createKey)
	authed("DELETE /api/keys/{id}", a.deleteKey)
	authed("GET /api/sessions", a.listSessions)
	authed("POST /api/sessions", a.createSession)
	authed("DELETE /api/sessions/{id}", a.endSession)
	authed("GET /api/sessions/{id}/ws", a.attach)

	mux.Handle("/", http.FileServerFS(a.Static))
	return securityHeaders(auth.SameOrigin(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "Couldn't read that request.")
		return false
	}
	return true
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func serverError(w http.ResponseWriter, err error) {
	fail(w, http.StatusInternalServerError, "internal", "Something went wrong on the server: "+err.Error())
}

func device(r *http.Request) string {
	ua := r.UserAgent()
	for _, d := range []struct{ needle, label string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"},
		{"Macintosh", "Mac"}, {"Windows", "Windows"}, {"CrOS", "Chromebook"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, d.needle) {
			return d.label
		}
	}
	return "another device"
}

// ---- vault & sign-in ----

func (a *API) state(w http.ResponseWriter, r *http.Request) {
	init, err := a.Vault.Initialized()
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": init,
		"unlocked":    a.Auth.Authenticated(r),
		"cloudflare":  a.Cloudflare,
	})
}

type passBody struct {
	Passphrase string `json:"passphrase"`
}

func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	var b passBody
	if !readJSON(w, r, &b) {
		return
	}
	switch err := a.Vault.Setup(b.Passphrase); {
	case errors.Is(err, vault.ErrWeakPassphrase):
		fail(w, http.StatusBadRequest, "weak_passphrase", "Use at least 8 characters.")
	case errors.Is(err, vault.ErrAlreadySetUp):
		fail(w, http.StatusConflict, "already_set_up", "Cotta is already set up. Unlock it instead.")
	case err != nil:
		serverError(w, err)
	default:
		if err := a.Auth.Login(w, r); err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (a *API) unlock(w http.ResponseWriter, r *http.Request) {
	var b passBody
	if !readJSON(w, r, &b) {
		return
	}
	if ok, wait := a.Auth.AllowAttempt(); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		fail(w, http.StatusTooManyRequests, "throttled", fmt.Sprintf("Too many wrong attempts. Try again in %d min.", int(wait.Minutes())+1))
		return
	}
	switch err := a.Vault.Unlock(b.Passphrase); {
	case errors.Is(err, vault.ErrWrongPassphrase):
		a.Auth.RecordFailure()
		fail(w, http.StatusForbidden, "wrong_passphrase", "That passphrase isn't right.")
	case errors.Is(err, vault.ErrNotSetUp):
		fail(w, http.StatusConflict, "not_set_up", "Cotta hasn't been set up yet.")
	case err != nil:
		serverError(w, err)
	default:
		a.Auth.ClearFailures()
		if err := a.Auth.Login(w, r); err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (a *API) lock(w http.ResponseWriter, _ *http.Request) {
	a.Auth.LockAll(w)
	a.Sessions.DetachAll() // shells keep running; browsers must sign in again
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- paste parsing & probing ----

func (a *API) parse(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	res, err := parse.Parse(b.Text)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "unparseable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) probe(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if b.Host == "" || b.Port < 1 || b.Port > 65535 {
		fail(w, http.StatusBadRequest, "bad_request", "Host and port are needed to check a server.")
		return
	}
	key, latency, err := sshx.Probe(r.Context(), net.JoinHostPort(b.Host, strconv.Itoa(b.Port)))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reachable": false, "error": friendlyNetError(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reachable":   true,
		"latencyMs":   max(1, latency.Milliseconds()),
		"keyType":     key.Type(),
		"fingerprint": ssh.FingerprintSHA256(key),
		"hostKey":     strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
	})
}

func friendlyNetError(err error) string {
	msg := err.Error()
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return "That host name doesn't resolve from your Cotta server."
	case strings.Contains(msg, "refused"):
		return "The server refused the connection — is SSH running on that port?"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") || errors.Is(err, context.DeadlineExceeded):
		return "No answer. The server may be off, or not reachable from your Cotta server's network."
	case strings.Contains(msg, "no route") || strings.Contains(msg, "unreachable"):
		return "Your Cotta server has no network route to that address."
	}
	return "Couldn't connect: " + msg
}

// ---- hosts ----

type hostJSON struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	User        string     `json:"user"`
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	AuthType    string     `json:"authType"`
	KeyID       *int64     `json:"keyId"`
	HasPassword bool       `json:"hasPassword"`
	Trusted     bool       `json:"trusted"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
}

func toHostJSON(h *store.Host) hostJSON {
	fp, _ := sshx.Fingerprint(h.HostKey)
	return hostJSON{
		ID: h.ID, Name: h.Name, User: h.User, Host: h.Host, Port: h.Port,
		AuthType: h.AuthType, KeyID: h.KeyID, HasPassword: len(h.PasswordEnc) > 0,
		Trusted: h.HostKey != "", Fingerprint: fp, LastUsedAt: h.LastUsedAt,
	}
}

type hostInput struct {
	Name     string `json:"name"`
	User     string `json:"user"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	AuthType string `json:"authType"`
	KeyID    *int64 `json:"keyId"`
	Password string `json:"password"` // empty on update = keep the stored one
	HostKey  string `json:"hostKey"`  // key the user saw and chose to trust
}

// apply validates in and copies it onto h. It returns a user-facing message
// when something is wrong.
func (a *API) apply(h *store.Host, in *hostInput) (string, error) {
	in.Name, in.User, in.Host = strings.TrimSpace(in.Name), strings.TrimSpace(in.User), strings.TrimSpace(in.Host)
	if in.Port == 0 {
		in.Port = 22
	}
	switch {
	case in.Host == "":
		return "A host is needed.", nil
	case in.User == "":
		return "A user name is needed.", nil
	case in.Port < 1 || in.Port > 65535:
		return "Port must be between 1 and 65535.", nil
	}
	// Re-validate through the parser so stored values are always sane.
	if _, err := parse.Parse(in.User + "@" + bracket(in.Host)); err != nil {
		return "That user or host doesn't look right.", nil
	}
	if in.Name == "" {
		in.Name = in.Host
	}
	hostChanged := h.Host != in.Host || h.Port != in.Port
	h.Name, h.User, h.Host, h.Port = in.Name, in.User, in.Host, in.Port

	switch in.AuthType {
	case "key":
		if in.KeyID == nil {
			return "Choose a key, or add one under Keys.", nil
		}
		if _, err := a.Store.GetKey(*in.KeyID); errors.Is(err, store.ErrNotFound) {
			return "That key no longer exists.", nil
		} else if err != nil {
			return "", err
		}
		h.KeyID, h.PasswordEnc = in.KeyID, nil
	case "password":
		if in.Password != "" {
			enc, err := a.Vault.Encrypt(purposePassword, []byte(in.Password))
			if err != nil {
				return "", err
			}
			h.PasswordEnc = enc
		} else if len(h.PasswordEnc) == 0 || h.AuthType != "password" {
			return "Enter the password to store.", nil
		}
		h.KeyID = nil
	case "ask":
		h.KeyID, h.PasswordEnc = nil, nil
	default:
		return "Choose how to sign in.", nil
	}
	h.AuthType = in.AuthType

	if in.HostKey != "" {
		if fp, _ := sshx.Fingerprint(in.HostKey); fp == "" {
			return "That host key is unreadable.", nil
		}
		h.HostKey = in.HostKey
	} else if hostChanged {
		h.HostKey = "" // a different machine must be trusted afresh
	}
	return "", nil
}

func bracket(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func (a *API) listHosts(w http.ResponseWriter, _ *http.Request) {
	hosts, err := a.Store.ListHosts()
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]hostJSON, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, toHostJSON(h))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createHost(w http.ResponseWriter, r *http.Request) {
	var in hostInput
	if !readJSON(w, r, &in) {
		return
	}
	h := &store.Host{}
	if msg, err := a.apply(h, &in); err != nil {
		serverError(w, err)
		return
	} else if msg != "" {
		fail(w, http.StatusUnprocessableEntity, "invalid", msg)
		return
	}
	if err := a.Store.CreateHost(h); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toHostJSON(h))
}

func (a *API) hostFromPath(w http.ResponseWriter, r *http.Request) *store.Host {
	id, ok := pathID(r)
	if !ok {
		fail(w, http.StatusNotFound, "not_found", "No such server.")
		return nil
	}
	h, err := a.Store.GetHost(id)
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "That server no longer exists.")
		return nil
	}
	if err != nil {
		serverError(w, err)
		return nil
	}
	return h
}

func (a *API) updateHost(w http.ResponseWriter, r *http.Request) {
	h := a.hostFromPath(w, r)
	if h == nil {
		return
	}
	var in hostInput
	if !readJSON(w, r, &in) {
		return
	}
	if msg, err := a.apply(h, &in); err != nil {
		serverError(w, err)
		return
	} else if msg != "" {
		fail(w, http.StatusUnprocessableEntity, "invalid", msg)
		return
	}
	if err := a.Store.UpdateHost(h); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toHostJSON(h))
}

func (a *API) deleteHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, http.StatusNotFound, "not_found", "No such server.")
		return
	}
	if err := a.Store.DeleteHost(id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// trustHost pins the exact key the user was shown, on first connect or
// after they accept a changed key.
func (a *API) trustHost(w http.ResponseWriter, r *http.Request) {
	h := a.hostFromPath(w, r)
	if h == nil {
		return
	}
	var b struct {
		HostKey string `json:"hostKey"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if fp, _ := sshx.Fingerprint(b.HostKey); fp == "" {
		fail(w, http.StatusBadRequest, "bad_request", "That host key is unreadable.")
		return
	}
	if err := a.Store.SetHostKey(h.ID, b.HostKey); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- keys ----

type keyJSON struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	PublicKey   string    `json:"publicKey"`
	Fingerprint string    `json:"fingerprint"`
	KeyType     string    `json:"keyType"`
	CreatedAt   time.Time `json:"createdAt"`
}

func toKeyJSON(k *store.Key) keyJSON {
	fp, typ := sshx.Fingerprint(k.PublicKey)
	return keyJSON{ID: k.ID, Name: k.Name, PublicKey: k.PublicKey, Fingerprint: fp, KeyType: typ, CreatedAt: k.CreatedAt}
}

func (a *API) listKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := a.Store.ListKeys()
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]keyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, toKeyJSON(k))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createKey(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name       string `json:"name"`
		Generate   bool   `json:"generate"`
		PrivateKey string `json:"privateKey"`
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		fail(w, http.StatusUnprocessableEntity, "invalid", "Give the key a name.")
		return
	}
	var priv []byte
	var pub string
	var err error
	if b.Generate {
		priv, pub, err = sshx.GenerateKey("cotta-" + strings.ReplaceAll(b.Name, " ", "-"))
	} else {
		priv, pub, err = sshx.NormalizeKey(b.PrivateKey, b.Passphrase, "")
	}
	if errors.Is(err, sshx.ErrKeyNeedsPassphrase) {
		fail(w, http.StatusUnprocessableEntity, "key_passphrase", "This key is passphrase-protected. Enter its passphrase to import it.")
		return
	}
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "invalid", "Couldn't read that private key. Paste the whole file, including the BEGIN and END lines.")
		return
	}
	enc, err := a.Vault.Encrypt(purposeKey, priv)
	if err != nil {
		serverError(w, err)
		return
	}
	k := &store.Key{Name: b.Name, PublicKey: pub, PrivateEnc: enc}
	if err := a.Store.CreateKey(k); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toKeyJSON(k))
}

func (a *API) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, http.StatusNotFound, "not_found", "No such key.")
		return
	}
	if err := a.Store.DeleteKey(id); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- sessions ----

func (a *API) listSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Sessions.List())
}

func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	var b struct {
		HostID   int64  `json:"hostId"`
		Password string `json:"password"`
		Cols     int    `json:"cols"`
		Rows     int    `json:"rows"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	h, err := a.Store.GetHost(b.HostID)
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "That server no longer exists.")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}

	var methods []ssh.AuthMethod
	switch h.AuthType {
	case "key":
		if h.KeyID == nil {
			fail(w, http.StatusUnprocessableEntity, "invalid", "This server's key was deleted. Edit the server to choose another.")
			return
		}
		k, err := a.Store.GetKey(*h.KeyID)
		if err != nil {
			serverError(w, err)
			return
		}
		pemBytes, err := a.Vault.Decrypt(purposeKey, k.PrivateEnc)
		if err != nil {
			serverError(w, err)
			return
		}
		m, err := sshx.KeyAuth(pemBytes)
		if err != nil {
			serverError(w, err)
			return
		}
		methods = []ssh.AuthMethod{m}
	case "password":
		pw, err := a.Vault.Decrypt(purposePassword, h.PasswordEnc)
		if err != nil {
			serverError(w, err)
			return
		}
		methods = sshx.PasswordAuth(string(pw))
	default: // ask
		if b.Password == "" {
			fail(w, http.StatusUnprocessableEntity, "password_required", "Enter the password for "+h.User+"@"+h.Host+".")
			return
		}
		methods = sshx.PasswordAuth(b.Password)
	}

	addr := net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
	client, err := sshx.Dial(r.Context(), sshx.Params{Addr: addr, User: h.User, Auth: methods, PinnedKey: h.HostKey})
	var hk *sshx.HostKeyError
	switch {
	case errors.As(err, &hk):
		code := "hostkey_unknown"
		if hk.Changed {
			code = "hostkey_changed"
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"code": code, "error": hk.Error(),
			"fingerprint": hk.Fingerprint, "keyType": hk.KeyType,
			"pinnedFingerprint": hk.PinnedFingerprint, "hostKey": hk.Offered,
		})
		return
	case sshx.IsAuthFailure(err):
		msg := "The server didn't accept that " + map[string]string{"key": "key", "password": "password", "ask": "password"}[h.AuthType] + "."
		fail(w, http.StatusUnprocessableEntity, "auth_failed", msg)
		return
	case err != nil:
		fail(w, http.StatusBadGateway, "unreachable", friendlyNetError(err))
		return
	}

	s, err := a.Sessions.Start(client, h.ID, h.Name, h.User+"@"+addr, device(r), b.Cols, b.Rows)
	if errors.Is(err, session.ErrTooMany) {
		fail(w, http.StatusConflict, "too_many", err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "shell_failed", "Signed in, but the server wouldn't open a shell: "+err.Error())
		return
	}
	a.Store.TouchHost(h.ID)
	writeJSON(w, http.StatusCreated, s.Info())
}

func (a *API) endSession(w http.ResponseWriter, r *http.Request) {
	if s := a.Sessions.Get(r.PathValue("id")); s != nil {
		s.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// WebSocket close codes the browser acts on.
const (
	closeEnded   = 4000 // the shell exited; don't reconnect
	closeDropped = 4001 // detached by the server; reconnect and replay
)

// attach streams a session to a browser: scrollback first, then live.
// Binary frames carry terminal bytes both ways; text frames carry JSON.
func (a *API) attach(w http.ResponseWriter, r *http.Request) {
	s := a.Sessions.Get(r.PathValue("id"))
	if s == nil {
		fail(w, http.StatusNotFound, "not_found", "That session has ended.")
		return
	}
	c, err := websocket.Accept(w, r, nil) // rejects cross-origin upgrades
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sub, snap := s.Attach(device(r))
	if sub == nil {
		c.Close(closeEnded, "session ended")
		return
	}
	defer s.Detach(sub)

	write := func(typ websocket.MessageType, data []byte) error {
		wctx, done := context.WithTimeout(ctx, 20*time.Second)
		defer done()
		return c.Write(wctx, typ, data)
	}
	send := func(v any) error {
		raw, _ := json.Marshal(v)
		return write(websocket.MessageText, raw)
	}

	for len(snap) > 0 {
		n := min(len(snap), 64*1024)
		if write(websocket.MessageBinary, snap[:n]) != nil {
			return
		}
		snap = snap[n:]
	}
	cols, rows := s.Size()
	if send(map[string]any{"type": "ready", "cols": cols, "rows": rows, "latencyMs": s.LatencyMs()}) != nil {
		return
	}

	go func() {
		defer cancel()
		for data := range sub.C {
			if write(websocket.MessageBinary, data) != nil {
				return
			}
		}
		if s.Dropped(sub) {
			c.Close(closeDropped, "detached")
		} else {
			c.Close(closeEnded, "session ended")
		}
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			if s.Input(sub, data) != nil {
				return
			}
			continue
		}
		var msg struct {
			Type string `json:"type"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "resize":
			s.Resize(sub, msg.Cols, msg.Rows)
		case "ping":
			// Also the moment to notice the browser's sign-in has expired.
			if !a.Auth.Authenticated(r) {
				c.Close(closeDropped, "locked")
				return
			}
			if send(map[string]any{"type": "pong", "latencyMs": s.LatencyMs(), "attached": s.Attached()}) != nil {
				return
			}
		}
	}
}
