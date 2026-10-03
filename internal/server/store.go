package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"gopkg.in/yaml.v3"
)

// ---- users: who may sign in -------------------------------------------------------

// User is one allowed person. Access to the cluster itself is still decided by
// Google (IAP + OS Login); this list decides who may use bifrost and with
// which tiers and caps.
type User struct {
	Email string   `yaml:"email"`
	Tiers []string `yaml:"tiers"`
	// Optional per-user overrides of the global A1 caps (0 = global value).
	MaxCostPerDayUSD float64 `yaml:"max_cost_usd_per_day,omitempty"`
	MaxCostPerJobUSD float64 `yaml:"max_cost_usd_per_job,omitempty"`
	MaxNodes         int     `yaml:"max_nodes,omitempty"`
	Disabled         bool    `yaml:"disabled,omitempty"`
}

// ProgramClient is a pre-registered OAuth client for a program (Ultra, a
// dashboard, a batch job) rather than a person's chat client. It signs in as a
// person like any client, but bifrost narrows what it may do: its tools are
// the person's tiers intersected with Tiers (a ceiling, never a grant), and it
// gets its own call budget, so a busy program neither starves nor is starved
// by the person's chat clients. Its audit records say "program:<id>".
type ProgramClient struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	Tiers        []string `yaml:"tiers"`         // ceiling: R1, R2, A1
	CallsPerMin  int      `yaml:"calls_per_min"` // 0 = the global limit
	RedirectURIs []string `yaml:"redirect_uris"`
	Disabled     bool     `yaml:"disabled,omitempty"`
	// Own A1 caps (v0.9.3). When MaxCostPerDayUSD is set the program submits
	// against its own ledger and these caps instead of the person's, so a
	// program that confirms its own tokens (the deep-research Lab after a
	// Submit click) has a hard server-side budget and the person's own clients
	// keep theirs. Unset = the person's caps and ledger, as before.
	MaxCostPerDayUSD float64 `yaml:"max_cost_usd_per_day,omitempty"`
	MaxCostPerJobUSD float64 `yaml:"max_cost_usd_per_job,omitempty"`
	MaxSubmitsPerDay int     `yaml:"max_submits_per_day,omitempty"`
}

// Bounds on a program's own caps (a typo must not mean an unlimited program).
const (
	MaxProgramDayUSD     = 500
	MaxProgramSubmitsDay = 1000
)

// MaxProgramCallsPerMin bounds a program client's calls_per_min.
const MaxProgramCallsPerMin = 600

var programIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{2,40}$`)

// UsersFile is the allow-list file.
type UsersFile struct {
	// Domain every user must belong to (Google hd claim), e.g. ucr.edu.
	Domain  string          `yaml:"domain"`
	Users   []User          `yaml:"users"`
	Clients []ProgramClient `yaml:"clients,omitempty"`
}

// Users is a reloadable allow-list. Edits to the file take effect on the next
// sign-in or token refresh, and existing sessions are re-checked per request.
type Users struct {
	path string
	mu   sync.Mutex
	mod  time.Time
	f    UsersFile
}

// LoadUsers reads the allow-list.
func LoadUsers(path string) (*Users, error) {
	u := &Users{path: path}
	if err := u.reload(); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *Users) reload() error {
	st, err := os.Stat(u.path)
	if err != nil {
		return fmt.Errorf("users file: %w", err)
	}
	if !st.ModTime().After(u.mod) && u.mod != (time.Time{}) {
		return nil
	}
	b, err := os.ReadFile(u.path)
	if err != nil {
		return err
	}
	var f UsersFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("users file %s: %w", u.path, err)
	}
	if f.Domain == "" {
		return errors.New("users file: domain is required (e.g. ucr.edu)")
	}
	for i, x := range f.Users {
		f.Users[i].Email = strings.ToLower(strings.TrimSpace(x.Email))
		if !strings.HasSuffix(f.Users[i].Email, "@"+f.Domain) {
			return fmt.Errorf("users file: %s is not in domain %s", x.Email, f.Domain)
		}
		for _, t := range x.Tiers {
			if t != "R1" && t != "R2" && t != "A1" {
				return fmt.Errorf("users file: %s has unknown tier %q", x.Email, t)
			}
		}
	}
	if err := checkClients(f.Clients); err != nil {
		return err
	}
	u.f, u.mod = f, st.ModTime()
	return nil
}

func checkClients(cs []ProgramClient) error {
	seen := map[string]bool{}
	for _, c := range cs {
		if !programIDRe.MatchString(c.ID) {
			return fmt.Errorf("users file: client id %q must be 3-41 of a-z, 0-9, - (starting with a letter)", c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("users file: client id %q is listed twice", c.ID)
		}
		seen[c.ID] = true
		if len(c.Tiers) == 0 {
			return fmt.Errorf("users file: client %s needs tiers (its ceiling)", c.ID)
		}
		for _, t := range c.Tiers {
			if t != "R1" && t != "R2" && t != "A1" {
				return fmt.Errorf("users file: client %s has unknown tier %q", c.ID, t)
			}
		}
		if c.CallsPerMin < 0 || c.CallsPerMin > MaxProgramCallsPerMin {
			return fmt.Errorf("users file: client %s calls_per_min must be 0-%d", c.ID, MaxProgramCallsPerMin)
		}
		if c.MaxCostPerDayUSD < 0 || c.MaxCostPerDayUSD > MaxProgramDayUSD {
			return fmt.Errorf("users file: client %s max_cost_usd_per_day must be 0-%d", c.ID, MaxProgramDayUSD)
		}
		if c.MaxCostPerJobUSD < 0 || (c.MaxCostPerJobUSD > 0 && c.MaxCostPerJobUSD > c.MaxCostPerDayUSD) {
			return fmt.Errorf("users file: client %s max_cost_usd_per_job needs max_cost_usd_per_day and must not exceed it", c.ID)
		}
		if c.MaxSubmitsPerDay < 0 || c.MaxSubmitsPerDay > MaxProgramSubmitsDay {
			return fmt.Errorf("users file: client %s max_submits_per_day must be 0-%d", c.ID, MaxProgramSubmitsDay)
		}
		if c.MaxSubmitsPerDay > 0 && c.MaxCostPerDayUSD == 0 {
			return fmt.Errorf("users file: client %s max_submits_per_day needs max_cost_usd_per_day (own caps come as a set)", c.ID)
		}
		if len(c.RedirectURIs) == 0 {
			return fmt.Errorf("users file: client %s needs redirect_uris", c.ID)
		}
		for _, r := range c.RedirectURIs {
			if !validRedirect(r) {
				return fmt.Errorf("users file: client %s redirect URI must be https or http loopback: %s", c.ID, r)
			}
		}
	}
	return nil
}

// Program returns the enabled program client with this id, or nil (dynamic
// clients, unknown ids and disabled programs).
func (u *Users) Program(id string) *ProgramClient {
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = u.reload()
	for _, c := range u.f.Clients {
		if c.ID == id && !c.Disabled {
			x := c
			return &x
		}
	}
	return nil
}

// isProgramID reports whether id is listed as a program client at all
// (enabled or disabled), so a disabled program is refused rather than
// looked up as a dynamic client.
func (u *Users) isProgramID(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = u.reload()
	for _, c := range u.f.Clients {
		if c.ID == id {
			return true
		}
	}
	return false
}

// ceiling narrows a person's tiers to a program's ceiling (order kept).
func ceiling(have, limit []string) []string {
	out := []string{}
	for _, t := range have {
		for _, l := range limit {
			if t == l {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

// Lookup returns the user entry for an email, or nil if not allowed.
func (u *Users) Lookup(email string) *User {
	u.mu.Lock()
	defer u.mu.Unlock()
	_ = u.reload() // keep the last good list if the file is mid-edit
	email = strings.ToLower(email)
	for _, x := range u.f.Users {
		if x.Email == email && !x.Disabled {
			c := x
			return &c
		}
	}
	return nil
}

// Domain is the required Google Workspace domain.
func (u *Users) Domain() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.f.Domain
}

// ---- sealed storage ----------------------------------------------------------------

// Sealer encrypts small records at rest with AES-256-GCM.
type Sealer struct{ aead cipher.AEAD }

// NewSealer takes a base64 32-byte key.
func NewSealer(b64 string) (*Sealer, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(k) != 32 {
		return nil, errors.New("secret key must be 32 random bytes, base64 (openssl rand -base64 32)")
	}
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: a}, nil
}

// Seal encrypts v (JSON) bound to a label (so a record cannot be swapped for another).
func (s *Sealer) Seal(label string, v any) ([]byte, error) {
	pt, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, pt, []byte(label)), nil
}

// Open decrypts into v.
func (s *Sealer) Open(label string, b []byte, v any) error {
	n := s.aead.NonceSize()
	if len(b) < n {
		return errors.New("sealed record too short")
	}
	pt, err := s.aead.Open(nil, b[:n], b[n:], []byte(label))
	if err != nil {
		return errors.New("sealed record failed authentication")
	}
	return json.Unmarshal(pt, v)
}

// Store keeps sealed records as files: <dir>/<kind>/<sha256(id)>.
type Store struct {
	dir string
	s   *Sealer
	mu  sync.Mutex
}

// NewStore returns a Store under dir.
func NewStore(dir string, s *Sealer) *Store { return &Store{dir: dir, s: s} }

func (st *Store) path(kind, id string) string {
	h := sha256.Sum256([]byte(id))
	return filepath.Join(st.dir, kind, hex.EncodeToString(h[:]))
}

// Put seals and writes v.
func (st *Store) Put(kind, id string, v any) error {
	b, err := st.s.Seal(kind+"/"+id, v)
	if err != nil {
		return err
	}
	p := st.path(kind, id)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Get reads v; ok=false when absent.
func (st *Store) Get(kind, id string, v any) (bool, error) {
	b, err := os.ReadFile(st.path(kind, id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := st.s.Open(kind+"/"+id, b, v); err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes a record.
func (st *Store) Delete(kind, id string) error {
	err := os.Remove(st.path(kind, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// keyStore adapts Store to backend.KeyStore (per-user SSH keys, sealed).
type keyStore struct{ st *Store }

func (k keyStore) Get(email string) (*backend.StoredKey, error) {
	var v backend.StoredKey
	ok, err := k.st.Get("sshkey", email, &v)
	if err != nil || !ok {
		return nil, err
	}
	if time.Now().After(v.Expires) {
		return nil, nil
	}
	return &v, nil
}
func (k keyStore) Put(email string, v *backend.StoredKey) error { return k.st.Put("sshkey", email, v) }
func (k keyStore) Delete(email string) error                    { return k.st.Delete("sshkey", email) }
