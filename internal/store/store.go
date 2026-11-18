// Package store is the SQLite persistence layer. Secrets arrive here already
// encrypted by the vault; this package never sees plaintext credentials.
package store

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

type Host struct {
	ID          int64
	Name        string
	User        string
	Host        string
	Port        int
	AuthType    string // "key" | "password" | "ask"
	KeyID       *int64
	PasswordEnc []byte
	HostKey     string // pinned key, authorized_keys format; "" = not yet trusted
	LastUsedAt  *time.Time
	CreatedAt   time.Time
}

type Key struct {
	ID         int64
	Name       string
	PublicKey  string
	PrivateEnc []byte
	CreatedAt  time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS keys (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	public_key  TEXT NOT NULL,
	private_enc BLOB NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS hosts (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	user         TEXT NOT NULL,
	host         TEXT NOT NULL,
	port         INTEGER NOT NULL,
	auth_type    TEXT NOT NULL,
	key_id       INTEGER REFERENCES keys(id) ON DELETE SET NULL,
	password_enc BLOB,
	host_key     TEXT NOT NULL DEFAULT '',
	last_used_at INTEGER,
	created_at   INTEGER NOT NULL
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---- settings ----

func (s *Store) GetSetting(key string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(key string, value []byte) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---- hosts ----

const hostCols = `id, name, user, host, port, auth_type, key_id, password_enc, host_key, last_used_at, created_at`

type scanner interface{ Scan(dest ...any) error }

func scanHost(r scanner) (*Host, error) {
	var h Host
	var keyID, lastUsed sql.NullInt64
	var created int64
	if err := r.Scan(&h.ID, &h.Name, &h.User, &h.Host, &h.Port, &h.AuthType, &keyID, &h.PasswordEnc, &h.HostKey, &lastUsed, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if keyID.Valid {
		h.KeyID = &keyID.Int64
	}
	if lastUsed.Valid {
		t := time.Unix(lastUsed.Int64, 0)
		h.LastUsedAt = &t
	}
	h.CreatedAt = time.Unix(created, 0)
	return &h, nil
}

func (s *Store) ListHosts() ([]*Host, error) {
	rows, err := s.db.Query(`SELECT ` + hostCols + ` FROM hosts ORDER BY COALESCE(last_used_at, 0) DESC, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) GetHost(id int64) (*Host, error) {
	return scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE id = ?`, id))
}

func (s *Store) CreateHost(h *Host) error {
	h.CreatedAt = time.Now()
	res, err := s.db.Exec(`INSERT INTO hosts(name, user, host, port, auth_type, key_id, password_enc, host_key, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.Name, h.User, h.Host, h.Port, h.AuthType, h.KeyID, h.PasswordEnc, h.HostKey, h.CreatedAt.Unix())
	if err != nil {
		return err
	}
	h.ID, err = res.LastInsertId()
	return err
}

// UpdateHost rewrites the editable fields, including the pinned host key.
func (s *Store) UpdateHost(h *Host) error {
	_, err := s.db.Exec(`UPDATE hosts SET name = ?, user = ?, host = ?, port = ?, auth_type = ?, key_id = ?, password_enc = ?, host_key = ? WHERE id = ?`,
		h.Name, h.User, h.Host, h.Port, h.AuthType, h.KeyID, h.PasswordEnc, h.HostKey, h.ID)
	return err
}

func (s *Store) SetHostKey(id int64, hostKey string) error {
	_, err := s.db.Exec(`UPDATE hosts SET host_key = ? WHERE id = ?`, hostKey, id)
	return err
}

func (s *Store) TouchHost(id int64) error {
	_, err := s.db.Exec(`UPDATE hosts SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

func (s *Store) DeleteHost(id int64) error {
	_, err := s.db.Exec(`DELETE FROM hosts WHERE id = ?`, id)
	return err
}

// ---- keys ----

func (s *Store) ListKeys() ([]*Key, error) {
	rows, err := s.db.Query(`SELECT id, name, public_key, private_enc, created_at FROM keys ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Key
	for rows.Next() {
		var k Key
		var created int64
		if err := rows.Scan(&k.ID, &k.Name, &k.PublicKey, &k.PrivateEnc, &created); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(created, 0)
		out = append(out, &k)
	}
	return out, rows.Err()
}

func (s *Store) GetKey(id int64) (*Key, error) {
	var k Key
	var created int64
	err := s.db.QueryRow(`SELECT id, name, public_key, private_enc, created_at FROM keys WHERE id = ?`, id).
		Scan(&k.ID, &k.Name, &k.PublicKey, &k.PrivateEnc, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.CreatedAt = time.Unix(created, 0)
	return &k, nil
}

func (s *Store) CreateKey(k *Key) error {
	k.CreatedAt = time.Now()
	res, err := s.db.Exec(`INSERT INTO keys(name, public_key, private_enc, created_at) VALUES(?, ?, ?, ?)`,
		k.Name, k.PublicKey, k.PrivateEnc, k.CreatedAt.Unix())
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	return err
}

func (s *Store) DeleteKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM keys WHERE id = ?`, id)
	return err
}
