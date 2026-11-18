// Package session keeps SSH shells alive on the server independently of any
// browser. A browser attaches to a session, receives the scrollback kept so
// far, then streams live. Closing the browser only detaches.
package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	scrollback    = 512 * 1024 // bytes of output kept per session for replay
	subBuffer     = 512        // queued chunks before a slow browser is dropped
	keepaliveGap  = 15 * time.Second
	keepaliveWait = 30 * time.Second
	MaxSessions   = 32
)

var ErrTooMany = errors.New("too many running sessions — end one first")

type Info struct {
	ID         string    `json:"id"`
	HostID     int64     `json:"hostId"`
	Name       string    `json:"name"`
	Addr       string    `json:"addr"`
	StartedAt  time.Time `json:"startedAt"`
	LastDevice string    `json:"lastDevice"`
	LastLine   string    `json:"lastLine"`
	Attached   int       `json:"attached"`
	LatencyMs  int64     `json:"latencyMs"`
}

// Sub is one attached browser.
type Sub struct {
	C      chan []byte // closed when the session ends or this browser is dropped
	Device string

	cols, rows int
	sized      bool
	dropped    bool
}

type Session struct {
	ID        string
	HostID    int64
	Name      string
	Addr      string
	StartedAt time.Time

	client *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	inMu   sync.Mutex
	onEnd  func(*Session)

	latency atomic.Int64

	mu         sync.Mutex
	buf        []byte
	carry      []byte // tail of previous chunk, for sequences split across reads
	alt        bool   // remote app is on the alternate screen (vim, htop…)
	cols, rows int
	subs       map[*Sub]struct{}
	lastDevice string
	closed     bool
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

func NewManager() *Manager { return &Manager{sessions: map[string]*Session{}} }

// Start opens a shell with a PTY on an authenticated client and registers
// it. The manager owns the client from here on.
func (m *Manager) Start(client *ssh.Client, hostID int64, name, addr, device string, cols, rows int) (*Session, error) {
	m.mu.Lock()
	full := len(m.sessions) >= MaxSessions
	m.mu.Unlock()
	if full {
		client.Close()
		return nil, ErrTooMany
	}
	cols, rows = clamp(cols, rows)
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, err
	}
	idRaw := make([]byte, 16)
	rand.Read(idRaw)
	s := &Session{
		ID: hex.EncodeToString(idRaw), HostID: hostID, Name: name, Addr: addr, StartedAt: time.Now(),
		client: client, sess: sess, cols: cols, rows: rows,
		subs: map[*Sub]struct{}{}, lastDevice: device,
	}
	s.onEnd = func(s *Session) {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
	}
	fail := func(err error) (*Session, error) {
		sess.Close()
		client.Close()
		return nil, err
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		return fail(err)
	}
	if s.stdin, err = sess.StdinPipe(); err != nil {
		return fail(err)
	}
	sess.Stdout = writerFunc(s.output)
	sess.Stderr = writerFunc(s.output)
	if err := sess.Shell(); err != nil {
		return fail(err)
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	go func() {
		sess.Wait()
		s.Close()
	}()
	go s.keepalive()
	return s, nil
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := make([]Info, 0, len(all))
	for _, s := range all {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// DetachAll disconnects every browser but leaves the shells running. Used
// when the vault is locked.
func (m *Manager) DetachAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		for sub := range s.subs {
			sub.dropped = true
			close(sub.C)
			delete(s.subs, sub)
		}
		s.mu.Unlock()
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close()
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func clamp(cols, rows int) (int, int) {
	return min(max(cols, 2), 1000), min(max(rows, 2), 500)
}

var (
	altOn  = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?1047h"), []byte("\x1b[?47h")}
	altOff = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?1047l"), []byte("\x1b[?47l")}
)

// lastAltToggle finds the most recent alternate-screen switch in data.
func lastAltToggle(data []byte) (found, on bool) {
	best := -1
	for _, seq := range altOn {
		if i := bytes.LastIndex(data, seq); i > best {
			best, on = i, true
		}
	}
	for _, seq := range altOff {
		if i := bytes.LastIndex(data, seq); i > best {
			best, on = i, false
		}
	}
	return best >= 0, on
}

// output receives everything the remote shell prints.
func (s *Session) output(p []byte) {
	data := append([]byte(nil), p...) // ssh reuses its buffer
	s.mu.Lock()
	defer s.mu.Unlock()

	scan := append(s.carry, data...)
	if found, on := lastAltToggle(scan); found {
		s.alt = on
	}
	s.carry = append(s.carry[:0], scan[max(0, len(scan)-8):]...)

	s.buf = append(s.buf, data...)
	if over := len(s.buf) - scrollback; over > 0 {
		// Trim from the front, then on to the next line break so replay
		// does not start in the middle of an escape sequence.
		cut := over
		if nl := bytes.IndexByte(s.buf[cut:min(len(s.buf), cut+4096)], '\n'); nl >= 0 {
			cut += nl + 1
		}
		s.buf = append(s.buf[:0], s.buf[cut:]...)
	}
	for sub := range s.subs {
		select {
		case sub.C <- data:
		default:
			// Browser can't keep up; drop it. It reconnects and replays.
			sub.dropped = true
			close(sub.C)
			delete(s.subs, sub)
		}
	}
}

// Attach subscribes a browser and returns the scrollback to replay first.
// Returns nil if the session has already ended.
func (s *Session) Attach(device string) (*Sub, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil
	}
	var snap []byte
	if found, on := lastAltToggle(s.buf); s.alt && !(found && on) {
		// The switch into the alternate screen has scrolled out of the
		// buffer; put the new terminal in the right mode before replay.
		snap = append(snap, altOn[0]...)
	}
	snap = append(snap, s.buf...)
	sub := &Sub{C: make(chan []byte, subBuffer), Device: device}
	s.subs[sub] = struct{}{}
	return sub, snap
}

func (s *Session) Detach(sub *Sub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		close(sub.C)
	}
}

// Dropped reports whether sub was disconnected for falling behind, as
// opposed to the session ending.
func (s *Session) Dropped(sub *Sub) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sub.dropped
}

// Resize records a browser's terminal size and applies it. A browser that
// has just attached to a full-screen app also forces that app to repaint.
func (s *Session) Resize(sub *Sub, cols, rows int) {
	cols, rows = clamp(cols, rows)
	s.mu.Lock()
	fresh := !sub.sized
	sub.cols, sub.rows, sub.sized = cols, rows, true
	same := cols == s.cols && rows == s.rows
	s.cols, s.rows = cols, rows
	alt := s.alt
	closed := s.closed
	s.mu.Unlock()
	if closed || (same && !(fresh && alt)) {
		return
	}
	if same {
		s.sess.WindowChange(rows, cols+1) // nudge so the app redraws
	}
	s.sess.WindowChange(rows, cols)
}

// Input sends keystrokes. The browser that is typing owns the terminal
// size, so two devices of different sizes can share a session.
func (s *Session) Input(sub *Sub, data []byte) error {
	s.mu.Lock()
	s.lastDevice = sub.Device
	steal := sub.sized && (sub.cols != s.cols || sub.rows != s.rows)
	cols, rows := sub.cols, sub.rows
	s.mu.Unlock()
	if steal {
		s.Resize(sub, cols, rows)
	}
	s.inMu.Lock()
	defer s.inMu.Unlock()
	_, err := s.stdin.Write(data)
	return err
}

// Size returns the terminal size currently in force.
func (s *Session) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

func (s *Session) LatencyMs() int64 { return s.latency.Load() }

// Attached is the number of browsers currently connected.
func (s *Session) Attached() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// Close ends the remote shell and disconnects every browser.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for sub := range s.subs {
		close(sub.C)
	}
	s.subs = map[*Sub]struct{}{}
	s.mu.Unlock()
	s.sess.Close()
	s.client.Close()
	s.onEnd(s)
}

func (s *Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// keepalive detects dead connections and measures round-trip time.
func (s *Session) keepalive() {
	for !s.isClosed() {
		start := time.Now()
		errc := make(chan error, 1)
		go func() {
			_, _, err := s.client.SendRequest("keepalive@openssh.com", true, nil)
			errc <- err
		}()
		select {
		case err := <-errc:
			if err != nil {
				s.Close()
				return
			}
			s.latency.Store(max(1, time.Since(start).Milliseconds()))
		case <-time.After(keepaliveWait):
			s.Close()
			return
		}
		time.Sleep(keepaliveGap)
	}
}

func (s *Session) Info() Info {
	s.mu.Lock()
	tail := s.buf[max(0, len(s.buf)-4096):]
	info := Info{
		ID: s.ID, HostID: s.HostID, Name: s.Name, Addr: s.Addr, StartedAt: s.StartedAt,
		LastDevice: s.lastDevice, LastLine: lastLine(tail), Attached: len(s.subs),
	}
	s.mu.Unlock()
	info.LatencyMs = s.latency.Load()
	return info
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?<=>]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[@-Z\\^_=>]|[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)

// lastLine extracts the last visible line of output for the "still running"
// preview.
func lastLine(tail []byte) string {
	lines := strings.Split(ansiRe.ReplaceAllString(string(tail), ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], "\r")
		if j := strings.LastIndexByte(line, '\r'); j >= 0 {
			line = line[j+1:] // progress bars overwrite the line in place
		}
		line = strings.TrimSpace(strings.ToValidUTF8(line, ""))
		if line != "" {
			if r := []rune(line); len(r) > 200 {
				line = string(r[:200])
			}
			return line
		}
	}
	return ""
}
