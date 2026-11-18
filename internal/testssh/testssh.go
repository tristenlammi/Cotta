// Package testssh is a tiny in-process SSH server with a fake line-echo
// shell. It backs the automated tests and the devsshd command, so Cotta can
// be exercised without a real machine to log in to. Never expose it.
package testssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Server struct {
	Addr     string
	User     string
	Password string
	HostKey  ssh.Signer

	ln net.Listener
	mu sync.Mutex
	// Resizes records window-change requests as "colsxrows".
	Resizes []string
	// AuthorizedKey, in authorized_keys format, may sign in as User.
	AuthorizedKey string
	// NoSFTP makes the server refuse file access, like much network gear.
	NoSFTP bool

	files sftp.Handlers // in-memory filesystem shared by all connections
}

// Authorize lets the given public key sign in.
func (s *Server) Authorize(authorizedKey string) {
	s.mu.Lock()
	s.AuthorizedKey = authorizedKey
	s.mu.Unlock()
}

// Start listens on addr ("127.0.0.1:0" for a random port).
func Start(addr, user, password string) (*Server, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Addr: ln.Addr().String(), User: user, Password: password, HostKey: signer, ln: ln, files: sftp.InMemHandler()}
	go s.serve()
	return s, nil
}

func (s *Server) Close() { s.ln.Close() }

// Rekey swaps the host key, simulating a reinstalled or impersonated host.
func (s *Server) Rekey() {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	s.mu.Lock()
	s.HostKey = signer
	s.mu.Unlock()
}

func (s *Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == s.User && string(pw) == s.Password {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			allowed := s.AuthorizedKey
			s.mu.Unlock()
			if want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(allowed)); err == nil &&
				c.User() == s.User && string(want.Marshal()) == string(key.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	s.mu.Lock()
	cfg.AddHostKey(s.HostKey)
	s.mu.Unlock()
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go func() {
		for r := range reqs {
			if r.WantReply {
				r.Reply(r.Type == "keepalive@openssh.com", nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs)
	}
}

func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	started := make(chan string, 1) // "shell" or "sftp"
	go func() {
		for r := range reqs {
			ok := false
			switch r.Type {
			case "pty-req":
				ok = true
			case "shell":
				ok = true
				started <- "shell"
			case "subsystem":
				var sub struct{ Name string }
				if ssh.Unmarshal(r.Payload, &sub) == nil && sub.Name == "sftp" && !s.NoSFTP {
					ok = true
					started <- "sftp"
				}
			case "window-change":
				var wc struct{ Cols, Rows, W, H uint32 }
				if ssh.Unmarshal(r.Payload, &wc) == nil {
					s.mu.Lock()
					s.Resizes = append(s.Resizes, fmt.Sprintf("%dx%d", wc.Cols, wc.Rows))
					s.mu.Unlock()
				}
				ok = true
			}
			if r.WantReply {
				r.Reply(ok, nil)
			}
		}
	}()
	if <-started == "sftp" {
		// File access is served from memory: nothing on the real disk is
		// read or written.
		sftp.NewRequestServer(ch, s.files).Serve()
		return
	}

	const prompt = "\x1b[38;5;209mdemo@testssh\x1b[0m:~$ "
	io.WriteString(ch, "Cotta test shell. Lines are echoed back; try: help, colors, flood, vim, exit\r\n"+prompt)
	var line []byte
	buf := make([]byte, 1024)
	for {
		n, err := ch.Read(buf)
		if err != nil {
			return
		}
		for _, b := range buf[:n] {
			switch b {
			case '\r', '\n':
				io.WriteString(ch, "\r\n")
				if !s.run(ch, strings.TrimSpace(string(line))) {
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				}
				line = line[:0]
				io.WriteString(ch, prompt)
			case 0x7f, 0x08:
				if len(line) > 0 {
					line = line[:len(line)-1]
					io.WriteString(ch, "\b \b")
				}
			case 0x03: // Ctrl-C
				line = line[:0]
				io.WriteString(ch, "^C\r\n"+prompt)
			case 0x04: // Ctrl-D
				return
			default:
				if b >= 0x20 {
					line = append(line, b)
					ch.Write([]byte{b})
				}
			}
		}
	}
}

// run executes one fake command; false means the shell should exit.
func (s *Server) run(ch ssh.Channel, cmd string) bool {
	switch cmd {
	case "":
	case "exit", "logout":
		io.WriteString(ch, "logout\r\n")
		return false
	case "help":
		io.WriteString(ch, "help    this text\r\ncolors  print a colour test\r\nflood   print 2000 lines\r\nvim     enter a fake full-screen app (any key leaves)\r\nexit    end the session\r\n")
	case "colors":
		for i := 31; i <= 36; i++ {
			fmt.Fprintf(ch, "\x1b[%dmcolour %d\x1b[0m ", i, i)
		}
		io.WriteString(ch, "\r\n")
	case "flood":
		for i := 1; i <= 2000; i++ {
			fmt.Fprintf(ch, "line %04d  the quick brown fox jumps over the lazy dog\r\n", i)
		}
	case "vim":
		io.WriteString(ch, "\x1b[?1049h\x1b[2J\x1b[H~ fake full-screen app ~\r\n~ press any key to leave ~")
		one := make([]byte, 1)
		ch.Read(one)
		io.WriteString(ch, "\x1b[?1049l")
	default:
		io.WriteString(ch, "you said: "+cmd+"\r\n")
	}
	return true
}
