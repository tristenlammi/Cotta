package vault

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"cotta/internal/store"
)

func TestVaultLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v := New(st)
	if _, err := v.Encrypt("p", []byte("x")); !errors.Is(err, ErrLocked) {
		t.Fatalf("encrypt before setup: %v", err)
	}
	if err := v.Setup("hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	if err := v.Setup("another-passphrase"); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("second setup: %v", err)
	}
	blob, err := v.Encrypt("host-password", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("s3cret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if _, err := v.Decrypt("key-private", blob); err == nil {
		t.Fatal("a blob decrypted under the wrong purpose")
	}

	// Reopen as after a container restart: locked until the passphrase returns.
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v = New(st)
	if v.Unlocked() {
		t.Fatal("vault unlocked after restart")
	}
	if err := v.Unlock("wrong passphrase"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if err := v.Unlock("hunter2hunter2"); err != nil {
		t.Fatal(err)
	}
	plain, err := v.Decrypt("host-password", blob)
	if err != nil || string(plain) != "s3cret" {
		t.Fatalf("decrypt after restart: %q %v", plain, err)
	}
	v.Lock()
	if _, err := v.Decrypt("host-password", blob); !errors.Is(err, ErrLocked) {
		t.Fatalf("decrypt while locked: %v", err)
	}
}
