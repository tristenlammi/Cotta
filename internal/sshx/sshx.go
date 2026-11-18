// Package sshx wraps golang.org/x/crypto/ssh with Cotta's host key policy
// (trust on first use, pinned afterwards) and private key handling.
package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const dialTimeout = 10 * time.Second

// HostKeyError means the connection was refused because of the host key:
// either the host has never been trusted, or its key no longer matches the
// pinned one.
type HostKeyError struct {
	Changed           bool
	Offered           string // authorized_keys format, ready to pin
	KeyType           string
	Fingerprint       string
	PinnedFingerprint string
}

func (e *HostKeyError) Error() string {
	if e.Changed {
		return "host key has changed"
	}
	return "host key is not trusted yet"
}

type Params struct {
	Addr      string
	User      string
	Auth      []ssh.AuthMethod
	PinnedKey string // authorized_keys format; "" = nothing trusted yet
}

// Dial connects and authenticates, enforcing the pinned host key.
func Dial(ctx context.Context, p Params) (*ssh.Client, error) {
	var pinned ssh.PublicKey
	if p.PinnedKey != "" {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(p.PinnedKey))
		if err != nil {
			return nil, errors.New("stored host key is unreadable")
		}
		pinned = k
	}
	var hkErr *HostKeyError
	cfg := &ssh.ClientConfig{
		User:    p.User,
		Auth:    p.Auth,
		Timeout: dialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if pinned != nil && bytes.Equal(pinned.Marshal(), key.Marshal()) {
				return nil
			}
			hkErr = describe(key, pinned)
			return hkErr
		},
	}
	if pinned != nil {
		// Ask for the pinned key's type so a server holding several host
		// keys offers the one we know.
		cfg.HostKeyAlgorithms = algorithmsFor(pinned.Type())
	}
	client, err := dial(ctx, p.Addr, cfg)
	if err == nil {
		return client, nil
	}
	if hkErr != nil {
		return nil, hkErr
	}
	if pinned != nil && strings.Contains(err.Error(), "no common algorithm") {
		// The server no longer has a key of the pinned type at all.
		if offered, _, perr := Probe(ctx, p.Addr); perr == nil {
			return nil, describe(offered, pinned)
		}
	}
	return nil, err
}

// Probe fetches a host's public key without authenticating, and measures how
// long the TCP connection took.
func Probe(ctx context.Context, addr string) (ssh.PublicKey, time.Duration, error) {
	var got ssh.PublicKey
	stop := errors.New("probe complete")
	cfg := &ssh.ClientConfig{
		User:    "cotta-probe",
		Timeout: dialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got = key
			return stop
		},
	}
	start := time.Now()
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, 0, err
	}
	latency := time.Since(start)
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(dialTimeout))
	_, _, _, err = ssh.NewClientConn(conn, addr, cfg)
	if got == nil {
		if err == nil {
			err = errors.New("no host key received")
		}
		return nil, latency, err
	}
	return got, latency, nil
}

func dial(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(2 * dialTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func describe(offered, pinned ssh.PublicKey) *HostKeyError {
	e := &HostKeyError{
		Offered:     strings.TrimSpace(string(ssh.MarshalAuthorizedKey(offered))),
		KeyType:     offered.Type(),
		Fingerprint: ssh.FingerprintSHA256(offered),
	}
	if pinned != nil {
		e.Changed = true
		e.PinnedFingerprint = ssh.FingerprintSHA256(pinned)
	}
	return e
}

func algorithmsFor(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

// Fingerprint returns the SHA256 fingerprint and type of a key in
// authorized_keys format, or empty strings if it cannot be read.
func Fingerprint(authorizedKey string) (fingerprint, keyType string) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return "", ""
	}
	return ssh.FingerprintSHA256(k), k.Type()
}

// IsAuthFailure reports whether err is the server rejecting our credentials.
func IsAuthFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unable to authenticate")
}

// PasswordAuth offers a password both directly and through
// keyboard-interactive, which many PAM setups require instead.
func PasswordAuth(password string) []ssh.AuthMethod {
	return []ssh.AuthMethod{
		ssh.Password(password),
		ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = password
			}
			return answers, nil
		}),
	}
}

// ---- private keys ----

var ErrKeyNeedsPassphrase = errors.New("this key is passphrase-protected")

// NormalizeKey parses a pasted private key (decrypting it if needed) and
// returns it as unencrypted OpenSSH PEM plus its public half. The PEM is
// only ever stored encrypted by the vault.
func NormalizeKey(pemText, passphrase, comment string) (privatePEM []byte, publicKey string, err error) {
	var raw any
	if passphrase != "" {
		raw, err = ssh.ParseRawPrivateKeyWithPassphrase([]byte(pemText), []byte(passphrase))
	} else {
		raw, err = ssh.ParseRawPrivateKey([]byte(pemText))
	}
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, "", ErrKeyNeedsPassphrase
		}
		return nil, "", errors.New("couldn't read that private key")
	}
	return marshal(raw, comment)
}

// GenerateKey makes a fresh ed25519 key pair.
func GenerateKey(comment string) (privatePEM []byte, publicKey string, err error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return marshal(priv, comment)
}

func marshal(raw any, comment string) ([]byte, string, error) {
	signer, err := ssh.NewSignerFromKey(raw)
	if err != nil {
		return nil, "", errors.New("unsupported key type")
	}
	block, err := ssh.MarshalPrivateKey(raw, comment)
	if err != nil {
		return nil, "", errors.New("unsupported key type")
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		pub += " " + comment
	}
	return pem.EncodeToMemory(block), pub, nil
}

// KeyAuth builds an auth method from stored (decrypted) OpenSSH PEM.
func KeyAuth(privatePEM []byte) (ssh.AuthMethod, error) {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return nil, err
	}
	return ssh.PublicKeys(signer), nil
}
