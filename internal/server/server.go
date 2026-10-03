// Package server is the hosted ursa-bifrost: the same MCP tools over
// Streamable HTTP, with OAuth sign-in through Google, and every cluster call
// made as the signed-in person (backend.IAP with their own Google token).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/core"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/mcpserver"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/staging"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/version"
)

// Server is the hosted bifrost.
type Server struct {
	cfg    config.Config
	base   string
	users  *Users
	store  *Store
	google *Google
	auth   *authState
	tokens *tokenCache
	audits *policy.Audit
	logger *log.Logger

	mu    sync.Mutex
	conns map[string]*userConn // per signed-in email (+ "|program-id" for program clients)
	// backends holds one backend (one SSH connection, one OS Login key) per
	// person, shared by their own connection and any program acting for them.
	backends map[string]backend.Backend

	// newBackend builds a user's backend (tests replace it with fixtures).
	newBackend func(email string) backend.Backend
	// staging is the shared staging area (nil = file tools off).
	staging staging.Client
	// shared caches output that is the same for every user (core.SharedCache).
	shared *core.SharedCache
	// gate caps work across everyone (accounting runs at once).
	gate *core.Gate
}

type userConn struct {
	svc  *core.Service
	srv  *mcp.Server
	sig  string // tiers + caps the service was built with (rebuild on change)
	last time.Time
}

// New builds the server from config. secret is the base64 sealing key.
func New(cfg config.Config, google *Google, secret string) (*Server, error) {
	sc := cfg.Server
	if sc.BaseURL == "" || !strings.HasPrefix(sc.BaseURL, "http") {
		return nil, fmt.Errorf("server.base_url must be the public URL")
	}
	users, err := LoadUsers(config.Expand(sc.UsersFile))
	if err != nil {
		return nil, err
	}
	sealer, err := NewSealer(secret)
	if err != nil {
		return nil, err
	}
	dataDir := config.Expand(sc.DataDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	audit, err := policy.NewAudit(cfg.AuditPath)
	if err != nil {
		return nil, err
	}
	store := NewStore(dataDir, sealer)
	s := &Server{cfg: cfg, base: strings.TrimRight(sc.BaseURL, "/"), users: users, store: store, google: google,
		auth: newAuthState(), tokens: &tokenCache{g: google, store: store, m: map[string]cachedTok{}},
		audits: audit, logger: log.New(os.Stderr, "bifrost-serve ", log.LstdFlags), conns: map[string]*userConn{},
		backends: map[string]backend.Backend{}, shared: core.NewSharedCache(), gate: core.NewGate()}
	s.staging = core.NewStaging(cfg.Staging, os.Getenv("K_SERVICE") != "")
	s.newBackend = func(email string) backend.Backend {
		ic := cfg.IAP
		return &backend.IAP{Project: ic.Project, Zone: ic.Zone, Instance: ic.Instance, Email: email,
			Token:    func(ctx context.Context) (string, error) { return s.tokens.get(ctx, email) },
			HostKeys: ic.HostKeys, Idle: 10 * time.Minute, Keys: keyStore{store}}
	}
	return s, nil
}

func (s *Server) logf(format string, a ...any) { s.logger.Printf(format, a...) }

// SetStaging replaces the staging client (tests).
func (s *Server) SetStaging(c staging.Client) { s.staging = c }

func (s *Server) audit(tool, email, decision, reason string) {
	_ = s.audits.Write(policy.Record{Time: time.Now(), Caller: email, Client: "http", Tool: tool, Decision: decision, Reason: reason})
}

// userConfig is the global config narrowed to one person: their tiers, their
// caps, their own ledger file and no local results folder.
func (s *Server) userConfig(u *User) config.Config {
	c := s.cfg
	c.Tiers = append([]string(nil), u.Tiers...)
	if u.MaxCostPerDayUSD > 0 {
		c.Caps.MaxCostPerDayUSD = u.MaxCostPerDayUSD
	}
	if u.MaxCostPerJobUSD > 0 {
		c.Caps.MaxCostPerJobUSD = u.MaxCostPerJobUSD
	}
	if u.MaxNodes > 0 {
		c.Caps.MaxNodes = u.MaxNodes
	}
	c.StatePath = filepath.Join(config.Expand(s.cfg.Server.DataDir), "ledger", hashTok(u.Email)+".json")
	c.ClusterUser = "" // learned from the login node (id -un) as this person
	return c
}

// programConfig narrows a person's config for a program client: tiers are the
// person's intersected with the program's ceiling, and the call budget is the
// program's own (0 = the global limit).
func programConfig(c config.Config, p *ProgramClient) config.Config {
	c.Tiers = ceiling(c.Tiers, p.Tiers)
	if p.CallsPerMin > 0 {
		c.Limits.CallsPerMin = p.CallsPerMin
	}
	if p.MaxCostPerDayUSD > 0 {
		// own budget, own ledger: never the person's day cap or submission count
		c.Caps.MaxCostPerDayUSD = p.MaxCostPerDayUSD
		if p.MaxCostPerJobUSD > 0 {
			c.Caps.MaxCostPerJobUSD = p.MaxCostPerJobUSD
		}
		if c.Caps.MaxCostPerJobUSD > c.Caps.MaxCostPerDayUSD {
			c.Caps.MaxCostPerJobUSD = c.Caps.MaxCostPerDayUSD
		}
		if p.MaxSubmitsPerDay > 0 {
			c.Caps.MaxSubmitsPerDay = p.MaxSubmitsPerDay
		}
		c.StatePath = strings.TrimSuffix(c.StatePath, ".json") + "-" + p.ID + ".json"
	}
	return c
}

// connKey names a person's connection, or a program's connection for them.
func connKey(email string, p *ProgramClient) string {
	if p == nil {
		return email
	}
	return email + "|" + p.ID
}

// backendFor returns the person's backend, creating it once. Caller holds s.mu.
func (s *Server) backendFor(email string) backend.Backend {
	if b, ok := s.backends[email]; ok {
		return b
	}
	b := s.newBackend(email)
	s.backends[email] = b
	return b
}

// conn returns the person's service + MCP server (or a program's, acting for
// them), building or rebuilding it when their tiers or caps change.
func (s *Server) conn(u *User, p *ProgramClient) *userConn {
	cfg := s.userConfig(u)
	if p != nil {
		cfg = programConfig(cfg, p)
	}
	sigB, _ := json.Marshal([]any{cfg.Tiers, cfg.Caps, cfg.Limits.CallsPerMin})
	sig := string(sigB)
	key := connKey(u.Email, p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.conns[key]; ok && c.sig == sig {
		c.last = time.Now()
		return c
	}
	svc := core.NewService(cfg, s.backendFor(u.Email), s.audits)
	svc.Principal = u.Email
	svc.Remote = true
	svc.Staging = s.staging
	svc.Shared = s.shared
	svc.Gate = s.gate
	svc.MaxInFlight = core.MaxInFlight
	if p != nil {
		svc.Program = p.ID
	}
	c := &userConn{svc: svc, srv: mcpserver.New(svc), sig: sig, last: time.Now()}
	s.conns[key] = c
	return c
}

type ctxKey struct{}

// caller is what requireAuth attaches to a request: the person and, for a
// program client, the program.
type caller struct {
	user    *User
	program *ProgramClient
}

// programFor resolves a token's client: nil for a dynamic client, the program
// for an enabled program client, and an error for a disabled program.
func (s *Server) programFor(clientID string) (*ProgramClient, error) {
	if !s.users.isProgramID(clientID) {
		return nil, nil
	}
	p := s.users.Program(clientID)
	if p == nil {
		return nil, fmt.Errorf("client %s is disabled", clientID)
	}
	return p, nil
}

// requireAuth checks the bearer token, then hands the request to the MCP
// handler with the person attached. 401s carry the resource metadata URL so
// MCP clients can discover how to sign in.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			s.challenge(w, "")
			return
		}
		a, u, err := s.verifyAccess(tok)
		if err != nil {
			s.challenge(w, err.Error())
			return
		}
		p, err := s.programFor(a.ClientID)
		if err != nil {
			s.audit("mcp", u.Email, "denied", err.Error())
			s.challenge(w, err.Error())
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, caller{user: u, program: p})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) challenge(w http.ResponseWriter, desc string) {
	v := fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, s.base)
	if desc != "" {
		v += fmt.Sprintf(`, error="invalid_token", error_description=%q`, desc)
	}
	w.Header().Set("WWW-Authenticate", v)
	http.Error(w, "sign-in required", http.StatusUnauthorized)
}

// Handler is the whole HTTP surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", s.handleResourceMeta)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.handleResourceMeta)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleASMeta)
	mux.HandleFunc("/register", s.handleRegister)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/oauth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/revoke", s.handleRevoke)
	mux.HandleFunc("/signout", s.handleSignout)
	mux.HandleFunc("/whoami", s.handleWhoami)
	// /healthz is reserved by Google's front end on run.app URLs (returns
	// Google's 404), so the health check lives at /health; /healthz is kept for
	// local runs.
	health := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "version": version.Version})
	}
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ursa-bifrost %s: MCP server for the Ursa Major HPC cluster.\nMCP endpoint: %s/mcp (OAuth sign-in with your %s Google account)\n", version.Version, s.base, s.users.Domain())
	})
	mcpH := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, ok := r.Context().Value(ctxKey{}).(caller)
		if !ok || c.user == nil {
			return nil
		}
		return s.conn(c.user, c.program).srv
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux.Handle("/mcp", s.requireAuth(mcpH))
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		h.ServeHTTP(w, r)
	})
}

// handleSignout (POST, bearer token) ends the person's bifrost session:
// deletes their Google refresh token, their stored SSH key (also from their
// OS Login profile) and every refresh token is invalidated on next use.
func (s *Server) handleSignout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	a, ok := s.lookupAccess(tok)
	if !ok {
		s.challenge(w, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s.mu.Lock()
	b := s.backends[a.Email]
	delete(s.backends, a.Email)
	for k, c := range s.conns {
		if k == a.Email || strings.HasPrefix(k, a.Email+"|") {
			if b == nil {
				b = c.svc.Backend
			}
			delete(s.conns, k)
		}
	}
	s.mu.Unlock()
	if iap, ok := b.(*backend.IAP); ok {
		_ = iap.Revoke(ctx)
	}
	_ = s.store.Delete("session", a.Email)
	s.tokens.drop(a.Email)
	s.auth.mu.Lock()
	for k, v := range s.auth.access {
		if v.Email == a.Email {
			delete(s.auth.access, k)
		}
	}
	s.auth.mu.Unlock()
	_ = s.store.Delete("access", hashTok(tok))
	// Other stored access/refresh tokens for this person are refused from now
	// on: both paths require a live session record, deleted above.
	s.audit("signout", a.Email, "allowed", "")
	writeJSON(w, 200, map[string]string{"status": "signed out"})
}

// Run serves until ctx ends; it also expires idle per-user connections.
func (s *Server) Run(ctx context.Context, addr string) error {
	hs := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.auth.gc()
				s.expireIdle(30 * time.Minute)
				s.shared.Sweep(2 * time.Hour)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	s.logf("ursa-bifrost %s serving %s on %s", version.Version, s.base, addr)
	err := hs.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// expireIdle drops connections unused for longer than idle, and closes a
// person's backend (SSH connection) only once none of their connections, own
// or program, is left.
func (s *Server) expireIdle(idle time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, c := range s.conns {
		if time.Since(c.last) > idle {
			delete(s.conns, k)
		}
	}
	for email, b := range s.backends {
		inUse := false
		for k := range s.conns {
			if k == email || strings.HasPrefix(k, email+"|") {
				inUse = true
				break
			}
		}
		if !inUse {
			if c, ok := b.(interface{ Close() error }); ok {
				_ = c.Close()
			}
			delete(s.backends, email)
		}
	}
}

// handleWhoami tells a client (e.g. the ursa-agent web app) who its bearer
// token belongs to: the email and tiers. It reveals nothing the token holder
// could not learn by calling tools, and nothing about anyone else.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		s.challenge(w, "")
		return
	}
	a, u, err := s.verifyAccess(tok)
	if err != nil {
		s.challenge(w, err.Error())
		return
	}
	p, err := s.programFor(a.ClientID)
	if err != nil {
		s.challenge(w, err.Error())
		return
	}
	if p == nil {
		writeJSON(w, 200, map[string]any{"email": u.Email, "tiers": u.Tiers})
		return
	}
	cpm := p.CallsPerMin
	if cpm == 0 {
		cpm = s.cfg.Limits.CallsPerMin
	}
	out := map[string]any{"email": u.Email, "tiers": ceiling(u.Tiers, p.Tiers),
		"program": p.ID, "calls_per_min": cpm}
	if p.MaxCostPerDayUSD > 0 {
		caps := programConfig(s.userConfig(u), p).Caps
		out["own_caps"] = map[string]any{"max_cost_usd_per_day": caps.MaxCostPerDayUSD,
			"max_cost_usd_per_job": caps.MaxCostPerJobUSD, "max_submits_per_day": caps.MaxSubmitsPerDay}
	}
	writeJSON(w, 200, out)
}
