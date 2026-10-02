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
	conns map[string]*userConn // per signed-in email

	// newBackend builds a user's backend (tests replace it with fixtures).
	newBackend func(email string) backend.Backend
	// staging is the shared staging area (nil = file tools off).
	staging staging.Client
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
		audits: audit, logger: log.New(os.Stderr, "bifrost-serve ", log.LstdFlags), conns: map[string]*userConn{}}
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

// conn returns the person's service + MCP server, building or rebuilding it.
func (s *Server) conn(u *User) *userConn {
	cfg := s.userConfig(u)
	sigB, _ := json.Marshal([]any{cfg.Tiers, cfg.Caps})
	sig := string(sigB)
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.conns[u.Email]; ok && c.sig == sig {
		c.last = time.Now()
		return c
	}
	if old, ok := s.conns[u.Email]; ok {
		_ = old.svc.Close()
	}
	svc := core.NewService(cfg, s.newBackend(u.Email), s.audits)
	svc.Principal = u.Email
	svc.Remote = true
	svc.Staging = s.staging
	c := &userConn{svc: svc, srv: mcpserver.New(svc), sig: sig, last: time.Now()}
	s.conns[u.Email] = c
	return c
}

type ctxKey struct{}

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
		ctx := context.WithValue(r.Context(), ctxKey{}, u)
		_ = a
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
		u, _ := r.Context().Value(ctxKey{}).(*User)
		if u == nil {
			return nil
		}
		return s.conn(u).srv
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
	c := s.conns[a.Email]
	delete(s.conns, a.Email)
	s.mu.Unlock()
	if c != nil {
		if iap, ok := c.svc.Backend.(*backend.IAP); ok {
			_ = iap.Revoke(ctx)
		}
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
				s.mu.Lock()
				for e, c := range s.conns {
					if time.Since(c.last) > 30*time.Minute {
						_ = c.svc.Close()
						delete(s.conns, e)
					}
				}
				s.mu.Unlock()
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

// handleWhoami tells a client (e.g. the ursa-agent web app) who its bearer
// token belongs to: the email and tiers. It reveals nothing the token holder
// could not learn by calling tools, and nothing about anyone else.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		s.challenge(w, "")
		return
	}
	_, u, err := s.verifyAccess(tok)
	if err != nil {
		s.challenge(w, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"email": u.Email, "tiers": u.Tiers})
}
