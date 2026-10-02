package backend

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StoredKey is one user's bifrost SSH key: the private half, the exact line
// imported to OS Login (its sha256 is the key id), the POSIX user and expiry.
type StoredKey struct {
	PrivateKey []byte    `json:"private_key"` // OpenSSH PEM
	Line       string    `json:"line"`
	User       string    `json:"user"`
	Expires    time.Time `json:"expires"`
}

// KeyStore keeps StoredKeys by Google email.
type KeyStore interface {
	Get(email string) (*StoredKey, error)
	Put(email string, k *StoredKey) error
	Delete(email string) error
}

// MemKeyStore is an in-process KeyStore (tests, single-process server use).
type MemKeyStore struct {
	mu sync.Mutex
	m  map[string]*StoredKey
}

// NewMemKeyStore returns an empty MemKeyStore.
func NewMemKeyStore() *MemKeyStore { return &MemKeyStore{m: map[string]*StoredKey{}} }

// Get returns the stored key or nil.
func (s *MemKeyStore) Get(email string) (*StoredKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[email], nil
}

// Put stores k.
func (s *MemKeyStore) Put(email string, k *StoredKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[email] = k
	return nil
}

// Delete removes the key.
func (s *MemKeyStore) Delete(email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, email)
	return nil
}

// FileKeyStore keeps one JSON file per user under Dir (mode 0600, dir 0700).
// Laptop mode: equivalent to gcloud's ~/.ssh/google_compute_engine, but the
// key is limited to KeyTTL on the OS Login side.
type FileKeyStore struct{ Dir string }

func (s FileKeyStore) path(email string) string {
	return filepath.Join(s.Dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(email))))
}

// Get returns the stored key, or nil when there is none or it has expired.
func (s FileKeyStore) Get(email string) (*StoredKey, error) {
	b, err := os.ReadFile(s.path(email))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var k StoredKey
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, nil // unreadable: treat as absent, a new key will replace it
	}
	if time.Now().After(k.Expires) {
		return nil, nil
	}
	return &k, nil
}

// Put writes the key atomically with mode 0600.
func (s FileKeyStore) Put(email string, k *StoredKey) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	tmp := s.path(email) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(email))
}

// Delete removes the key file.
func (s FileKeyStore) Delete(email string) error {
	err := os.Remove(s.path(email))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
