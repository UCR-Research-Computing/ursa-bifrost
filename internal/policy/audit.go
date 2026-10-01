package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one audit line.
type Record struct {
	Time       time.Time      `json:"time"`
	Caller     string         `json:"caller"` // local OS user (personal phase)
	Client     string         `json:"client"` // mcp client name or "cli"
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args,omitempty"`
	Decision   string         `json:"decision"` // allowed | denied | error | rate_limited
	Reason     string         `json:"reason,omitempty"`
	Commands   []string       `json:"commands,omitempty"`
	Bytes      int            `json:"bytes"`
	DurationMS int64          `json:"duration_ms"`
}

// Audit appends JSON lines to a file. A nil *Audit is a no-op.
type Audit struct {
	mu   sync.Mutex
	path string
}

// NewAudit opens (creating parent dirs) the audit file at path.
func NewAudit(path string) (*Audit, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	return &Audit{path: path}, nil
}

// Write appends one record; argument strings are redacted first.
func (a *Audit) Write(r Record) error {
	if a == nil {
		return nil
	}
	for k, v := range r.Args {
		if s, ok := v.(string); ok {
			if len(s) > 500 {
				s = s[:500] + "..."
			}
			r.Args[k] = Redact(s)
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Path is the audit file location.
func (a *Audit) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// ---- tiers --------------------------------------------------------------------

// ErrDenied is returned when the caller lacks the tier a tool needs.
var ErrDenied = errors.New("denied")

// Require returns ErrDenied unless have contains tier.
func Require(have []string, tier, tool string) error {
	for _, t := range have {
		if t == tier {
			return nil
		}
	}
	return fmt.Errorf("%w: %s needs tier %s (configured tiers: %v)", ErrDenied, tool, tier, have)
}

// ---- rate limit ------------------------------------------------------------------

// Limiter is a token bucket: perMin calls per minute, bursting to perMin.
type Limiter struct {
	mu     sync.Mutex
	perMin float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewLimiter returns a limiter; perMin <= 0 disables it.
func NewLimiter(perMin int) *Limiter {
	return &Limiter{perMin: float64(perMin), tokens: float64(perMin), now: time.Now}
}

// ErrRateLimited means the caller is over the call budget.
var ErrRateLimited = errors.New("rate limited")

// Allow takes one token or returns ErrRateLimited.
func (l *Limiter) Allow() error {
	if l == nil || l.perMin <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if !l.last.IsZero() {
		l.tokens += t.Sub(l.last).Minutes() * l.perMin
		if l.tokens > l.perMin {
			l.tokens = l.perMin
		}
	}
	l.last = t
	if l.tokens < 1 {
		return fmt.Errorf("%w: more than %.0f calls per minute; wait a moment", ErrRateLimited, l.perMin)
	}
	l.tokens--
	return nil
}
