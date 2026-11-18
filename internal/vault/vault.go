// Package vault encrypts secrets at rest. A random data key (DEK) encrypts
// every secret; the DEK itself is wrapped by a key derived from the master
// passphrase with Argon2id. The DEK only ever exists in memory while unlocked.
package vault

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"cotta/internal/store"
)

var (
	ErrLocked          = errors.New("vault is locked")
	ErrWrongPassphrase = errors.New("wrong passphrase")
	ErrAlreadySetUp    = errors.New("vault already set up")
	ErrNotSetUp        = errors.New("vault not set up")
	ErrWeakPassphrase  = errors.New("passphrase must be at least 8 characters")
)

const settingKey = "vault"

type header struct {
	V       int    `json:"v"`
	Salt    []byte `json:"salt"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"` // KiB
	Threads uint8  `json:"threads"`
	Wrapped []byte `json:"wrapped"` // nonce || AEAD(DEK)
}

type Vault struct {
	st  *store.Store
	mu  sync.RWMutex
	dek []byte
}

func New(st *store.Store) *Vault { return &Vault{st: st} }

func (v *Vault) Initialized() (bool, error) {
	_, err := v.st.GetSetting(settingKey)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (v *Vault) Unlocked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dek != nil
}

func kek(pass string, h *header) []byte {
	return argon2.IDKey([]byte(pass), h.Salt, h.Time, h.Memory, h.Threads, chacha20poly1305.KeySize)
}

// Setup creates the vault with a fresh data key and leaves it unlocked.
func (v *Vault) Setup(pass string) error {
	if len(pass) < 8 {
		return ErrWeakPassphrase
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, err := v.st.GetSetting(settingKey); err == nil {
		return ErrAlreadySetUp
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	h := &header{V: 1, Salt: make([]byte, 16), Time: 3, Memory: 64 * 1024, Threads: 4}
	dek := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(h.Salt); err != nil {
		return err
	}
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	wrapped, err := seal(kek(pass, h), "cotta/dek", dek)
	if err != nil {
		return err
	}
	h.Wrapped = wrapped
	raw, _ := json.Marshal(h)
	if err := v.st.SetSetting(settingKey, raw); err != nil {
		return err
	}
	v.dek = dek
	return nil
}

// Unlock verifies the passphrase and loads the data key into memory. It is
// also the passphrase check for signing in while already unlocked.
func (v *Vault) Unlock(pass string) error {
	raw, err := v.st.GetSetting(settingKey)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotSetUp
	}
	if err != nil {
		return err
	}
	var h header
	if err := json.Unmarshal(raw, &h); err != nil {
		return err
	}
	dek, err := open(kek(pass, &h), "cotta/dek", h.Wrapped)
	if err != nil {
		return ErrWrongPassphrase
	}
	v.mu.Lock()
	v.dek = dek
	v.mu.Unlock()
	return nil
}

// Lock drops the data key from memory.
func (v *Vault) Lock() {
	v.mu.Lock()
	for i := range v.dek {
		v.dek[i] = 0
	}
	v.dek = nil
	v.mu.Unlock()
}

// Encrypt seals plaintext. purpose is bound as associated data so a blob
// cannot be swapped between columns.
func (v *Vault) Encrypt(purpose string, plain []byte) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dek == nil {
		return nil, ErrLocked
	}
	return seal(v.dek, purpose, plain)
}

func (v *Vault) Decrypt(purpose string, blob []byte) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.dek == nil {
		return nil, ErrLocked
	}
	return open(v.dek, purpose, blob)
}

func seal(key []byte, aad string, plain []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, []byte(aad)), nil
}

func open(key []byte, aad string, blob []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], []byte(aad))
}
