package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file is bifrost's OAuth 2.1 authorization server for MCP clients
// (MCP authorization spec 2025-06-18): protected-resource metadata,
// authorization-server metadata, dynamic client registration (RFC 7591),
// authorization code + PKCE S256, refresh tokens, revocation. The person signs
// in with Google (hd = the configured domain); bifrost then issues its OWN
// tokens. Google tokens never leave the server and client tokens are never sent
// to Google (no token passthrough).

const (
	codeTTL    = 5 * time.Minute
	refreshTTL = 30 * 24 * time.Hour
)

// client is a registered OAuth client (public client, PKCE required).
type client struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}

// pendingAuth is an authorization request waiting for Google sign-in.
type pendingAuth struct {
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
	Created       time.Time
	GoogleVerif   string // PKCE verifier for the Google leg
}

// authCode is issued to the MCP client after Google sign-in.
type authCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Email         string
	Created       time.Time
}

// session is a signed-in person (sealed on disk: holds their Google refresh token).
type session struct {
	Email              string    `json:"email"`
	GoogleRefreshToken string    `json:"google_refresh_token"`
	Created            time.Time `json:"created"`
}

// refreshRec maps a bifrost refresh token (by hash) to a client + person.
type refreshRec struct {
	ClientID string    `json:"client_id"`
	Email    string    `json:"email"`
	Expires  time.Time `json:"expires"`
}

// accessRec is a live bifrost access token (memory only; lost on restart, the
// client refreshes).
type accessRec struct {
	Email    string
	ClientID string
	Expires  time.Time
}

type authState struct {
	mu       sync.Mutex
	pending  map[string]pendingAuth // by google state
	codes    map[string]authCode    // by code hash
	access   map[string]accessRec   // by token hash
	attempts map[string][]time.Time // by remote IP, for /register and /token
}

func newAuthState() *authState {
	return &authState{pending: map[string]pendingAuth{}, codes: map[string]authCode{},
		access: map[string]accessRec{}, attempts: map[string][]time.Time{}}
}

func randToken(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hashTok(t string) string {
	h := sha256.Sum256([]byte(t))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func s256(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// allowRate permits n requests per minute per key (simple sliding window).
func (a *authState) allowRate(key string, n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	keep := a.attempts[key][:0]
	for _, t := range a.attempts[key] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= n {
		a.attempts[key] = keep
		return false
	}
	a.attempts[key] = append(keep, now)
	return true
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, code int, kind, desc string) {
	writeJSON(w, code, map[string]string{"error": kind, "error_description": desc})
}

// validRedirect accepts https URLs and loopback http URLs (native clients).
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	}
	return false
}

// redirectMatches compares a requested redirect URI with a registered one;
// for loopback URIs the port may differ (RFC 8252 7.3).
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, err1 := url.Parse(registered)
	b, err2 := url.Parse(requested)
	if err1 != nil || err2 != nil || a.Scheme != "http" || b.Scheme != "http" {
		return false
	}
	la := a.Hostname() == "127.0.0.1" || a.Hostname() == "localhost"
	return la && a.Hostname() == b.Hostname() && a.Path == b.Path && a.RawQuery == b.RawQuery
}

// ---- metadata -------------------------------------------------------------------

func (s *Server) handleResourceMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"resource":                 s.base + "/mcp",
		"authorization_servers":    []string{s.base},
		"scopes_supported":         []string{"bifrost"},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "ursa-bifrost (Ursa Major HPC)",
	})
}

func (s *Server) handleASMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"issuer":                                s.base,
		"authorization_endpoint":                s.base + "/authorize",
		"token_endpoint":                        s.base + "/token",
		"registration_endpoint":                 s.base + "/register",
		"revocation_endpoint":                   s.base + "/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"bifrost"},
	})
}

// ---- dynamic client registration -------------------------------------------------

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthErr(w, 405, "invalid_request", "POST only")
		return
	}
	if !s.auth.allowRate("register:"+clientIP(r), 10) {
		oauthErr(w, 429, "slow_down", "too many registrations")
		return
	}
	var in struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		oauthErr(w, 400, "invalid_client_metadata", "bad JSON")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 5 {
		oauthErr(w, 400, "invalid_redirect_uri", "1-5 redirect_uris required")
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirect(u) {
			oauthErr(w, 400, "invalid_redirect_uri", "redirect URIs must be https or http loopback: "+u)
			return
		}
	}
	if in.AuthMethod != "" && in.AuthMethod != "none" {
		oauthErr(w, 400, "invalid_client_metadata", "only public clients (token_endpoint_auth_method none) with PKCE")
		return
	}
	name := in.ClientName
	if len(name) > 100 {
		name = name[:100]
	}
	c := client{ID: randToken("bfc_"), Name: name, RedirectURIs: in.RedirectURIs, Created: time.Now()}
	if err := s.store.Put("client", c.ID, c); err != nil {
		oauthErr(w, 500, "server_error", "storing client")
		return
	}
	writeJSON(w, 201, map[string]any{
		"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.RedirectURIs,
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "client_id_issued_at": c.Created.Unix(),
	})
}

func (s *Server) lookupClient(id string) (*client, error) {
	var c client
	ok, err := s.store.Get("client", id, &c)
	if err != nil || !ok {
		return nil, errors.New("unknown client")
	}
	return &c, nil
}

// ---- authorize: hand off to Google ------------------------------------------------

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.lookupClient(q.Get("client_id"))
	if err != nil {
		http.Error(w, "unknown client_id", 400)
		return
	}
	ru := q.Get("redirect_uri")
	okRU := false
	for _, reg := range c.RedirectURIs {
		if redirectMatches(reg, ru) {
			okRU = true
		}
	}
	if !okRU {
		http.Error(w, "redirect_uri is not registered for this client", 400) // never redirect to an unknown URI
		return
	}
	fail := func(kind, desc string) {
		u, _ := url.Parse(ru)
		v := u.Query()
		v.Set("error", kind)
		v.Set("error_description", desc)
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "code only")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with S256 is required")
		return
	}
	if res := q.Get("resource"); res != "" && strings.TrimRight(res, "/") != s.base+"/mcp" && strings.TrimRight(res, "/") != s.base {
		fail("invalid_target", "unknown resource")
		return
	}
	gState := randToken("")
	gVerif := randToken("")
	s.auth.mu.Lock()
	s.auth.pending[gState] = pendingAuth{ClientID: c.ID, RedirectURI: ru, State: q.Get("state"),
		CodeChallenge: q.Get("code_challenge"), Resource: q.Get("resource"), Created: time.Now(), GoogleVerif: gVerif}
	s.auth.mu.Unlock()

	v := url.Values{
		"client_id":             {s.google.ClientID},
		"redirect_uri":          {s.base + "/oauth/google/callback"},
		"response_type":         {"code"},
		"scope":                 {"openid email https://www.googleapis.com/auth/cloud-platform"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {gState},
		"code_challenge":        {s256(gVerif)},
		"code_challenge_method": {"S256"},
		"hd":                    {s.users.Domain()},
	}
	s.consentPage(w, c, s.google.AuthURL+"?"+v.Encode())
}

var consentTmpl = template.Must(template.New("c").Parse(`<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>ursa-bifrost sign-in</title>
<style>body{font-family:system-ui,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1rem;line-height:1.5}
a.b{display:inline-block;background:#1a73e8;color:#fff;padding:.6rem 1.2rem;border-radius:6px;text-decoration:none}
code{background:#f3f3f3;padding:0 .3rem}</style></head><body>
<h1>Sign in to ursa-bifrost</h1>
<p><b>{{.Client}}</b> wants to use the Ursa Major HPC cluster <b>as you</b>.</p>
<p>You will sign in with your {{.Domain}} Google account. Google will ask to let ursa-bifrost
"see, edit, configure and delete your Google Cloud data". bifrost needs that only to open an
IAP tunnel to the cluster login node and add a short-lived SSH key to your OS Login profile;
it does nothing else with it. Your Google token stays on the server, encrypted, and is deleted
when you sign out.</p>
<p>Everything still runs as your own cluster account, limited to what you can already do with
<code>gcloud compute ssh</code>.</p>
<p><a class="b" href="{{.URL}}">Continue with Google</a></p></body></html>`))

func (s *Server) consentPage(w http.ResponseWriter, c *client, googleURL string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	name := c.Name
	if name == "" {
		name = "An MCP client"
	}
	_ = consentTmpl.Execute(w, map[string]string{"Client": name, "Domain": s.users.Domain(), "URL": googleURL})
}

// ---- Google callback -------------------------------------------------------------

func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.auth.mu.Lock()
	p, ok := s.auth.pending[q.Get("state")]
	delete(s.auth.pending, q.Get("state"))
	s.auth.mu.Unlock()
	if !ok || time.Since(p.Created) > 10*time.Minute {
		http.Error(w, "sign-in expired or unknown; start again from your MCP client", 400)
		return
	}
	back := func(params url.Values) {
		u, _ := url.Parse(p.RedirectURI)
		v := u.Query()
		for k, vs := range params {
			v[k] = vs
		}
		if p.State != "" {
			v.Set("state", p.State)
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if e := q.Get("error"); e != "" {
		back(url.Values{"error": {"access_denied"}, "error_description": {"Google sign-in: " + e}})
		return
	}
	tok, err := s.google.Exchange(r.Context(), q.Get("code"), s.base+"/oauth/google/callback", p.GoogleVerif)
	if err != nil {
		back(url.Values{"error": {"server_error"}, "error_description": {"Google token exchange failed"}})
		s.logf("google exchange: %v", err)
		return
	}
	id, err := s.google.VerifyIDToken(r.Context(), tok.IDToken)
	if err != nil {
		back(url.Values{"error": {"access_denied"}, "error_description": {"Google identity could not be verified"}})
		s.logf("id token: %v", err)
		return
	}
	if !id.EmailVerified || id.HD != s.users.Domain() {
		back(url.Values{"error": {"access_denied"}, "error_description": {"only verified " + s.users.Domain() + " accounts may sign in"}})
		s.audit("signin", id.Email, "denied", "domain or unverified email")
		return
	}
	if s.users.Lookup(id.Email) == nil {
		back(url.Values{"error": {"access_denied"}, "error_description": {id.Email + " is not on the ursa-bifrost user list; ask Research Computing"}})
		s.audit("signin", id.Email, "denied", "not on user list")
		return
	}
	if !strings.Contains(tok.Scope, "https://www.googleapis.com/auth/cloud-platform") {
		back(url.Values{"error": {"access_denied"}, "error_description": {"Google Cloud access was not granted; bifrost needs it to reach the cluster as you"}})
		return
	}
	if tok.RefreshToken != "" {
		if err := s.store.Put("session", id.Email, session{Email: id.Email, GoogleRefreshToken: tok.RefreshToken, Created: time.Now()}); err != nil {
			back(url.Values{"error": {"server_error"}, "error_description": {"storing session"}})
			return
		}
	} else if ok, _ := s.store.Get("session", id.Email, &session{}); !ok {
		back(url.Values{"error": {"server_error"}, "error_description": {"Google returned no refresh token; remove ursa-bifrost from your Google account permissions and sign in again"}})
		return
	}
	s.tokens.seed(id.Email, tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn)*time.Second))
	code := randToken("bfa_")
	s.auth.mu.Lock()
	s.auth.codes[hashTok(code)] = authCode{ClientID: p.ClientID, RedirectURI: p.RedirectURI, CodeChallenge: p.CodeChallenge, Email: id.Email, Created: time.Now()}
	s.auth.mu.Unlock()
	s.audit("signin", id.Email, "allowed", "")
	back(url.Values{"code": {code}})
}

// ---- token endpoint ----------------------------------------------------------------

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthErr(w, 405, "invalid_request", "POST only")
		return
	}
	if !s.auth.allowRate("token:"+clientIP(r), 60) {
		oauthErr(w, 429, "slow_down", "too many token requests")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, 400, "invalid_request", "bad form")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.tokenFromCode(w, r)
	case "refresh_token":
		s.tokenFromRefresh(w, r)
	default:
		oauthErr(w, 400, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

func (s *Server) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	s.auth.mu.Lock()
	c, ok := s.auth.codes[hashTok(f.Get("code"))]
	delete(s.auth.codes, hashTok(f.Get("code"))) // single use
	s.auth.mu.Unlock()
	switch {
	case !ok || time.Since(c.Created) > codeTTL:
		oauthErr(w, 400, "invalid_grant", "code unknown, used or expired")
	case c.ClientID != f.Get("client_id"):
		oauthErr(w, 400, "invalid_grant", "code was issued to another client")
	case !redirectMatches(c.RedirectURI, f.Get("redirect_uri")) && c.RedirectURI != f.Get("redirect_uri"):
		oauthErr(w, 400, "invalid_grant", "redirect_uri mismatch")
	case f.Get("code_verifier") == "" || s256(f.Get("code_verifier")) != c.CodeChallenge:
		oauthErr(w, 400, "invalid_grant", "PKCE verification failed")
	default:
		s.issue(w, c.ClientID, c.Email)
	}
}

func (s *Server) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	rt := f.Get("refresh_token")
	var rec refreshRec
	ok, err := s.store.Get("refresh", hashTok(rt), &rec)
	if err != nil || !ok || time.Now().After(rec.Expires) {
		oauthErr(w, 400, "invalid_grant", "refresh token unknown or expired")
		return
	}
	if rec.ClientID != f.Get("client_id") {
		oauthErr(w, 400, "invalid_grant", "refresh token belongs to another client")
		return
	}
	_ = s.store.Delete("refresh", hashTok(rt)) // rotate
	if s.users.Lookup(rec.Email) == nil {
		oauthErr(w, 400, "invalid_grant", "user is no longer allowed")
		s.audit("refresh", rec.Email, "denied", "not on user list")
		return
	}
	if ok, _ := s.store.Get("session", rec.Email, &session{}); !ok {
		oauthErr(w, 400, "invalid_grant", "signed out; sign in again")
		return
	}
	s.issue(w, rec.ClientID, rec.Email)
}

func (s *Server) issue(w http.ResponseWriter, clientID, email string) {
	at := randToken("bfx_")
	rt := randToken("bfr_")
	ttl := time.Duration(s.cfg.Server.AccessTokenMinutes) * time.Minute
	if ttl <= 0 {
		ttl = time.Hour
	}
	s.auth.mu.Lock()
	s.auth.access[hashTok(at)] = accessRec{Email: email, ClientID: clientID, Expires: time.Now().Add(ttl)}
	s.auth.mu.Unlock()
	if err := s.store.Put("refresh", hashTok(rt), refreshRec{ClientID: clientID, Email: email, Expires: time.Now().Add(refreshTTL)}); err != nil {
		oauthErr(w, 500, "server_error", "storing refresh token")
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": int(ttl.Seconds()),
		"refresh_token": rt, "scope": "bifrost"})
}

// ---- revocation and sign-out -----------------------------------------------------

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthErr(w, 405, "invalid_request", "POST only")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	_ = r.ParseForm()
	t := r.PostForm.Get("token")
	s.auth.mu.Lock()
	delete(s.auth.access, hashTok(t))
	s.auth.mu.Unlock()
	_ = s.store.Delete("refresh", hashTok(t))
	w.WriteHeader(200) // RFC 7009: always 200
}

// verifyAccess checks a bifrost access token and the user list (re-checked on
// every request so removing someone takes effect immediately).
func (s *Server) verifyAccess(tok string) (*accessRec, *User, error) {
	s.auth.mu.Lock()
	a, ok := s.auth.access[hashTok(tok)]
	s.auth.mu.Unlock()
	if !ok || time.Now().After(a.Expires) {
		return nil, nil, errors.New("invalid or expired token")
	}
	u := s.users.Lookup(a.Email)
	if u == nil {
		return nil, nil, fmt.Errorf("%s is no longer allowed", a.Email)
	}
	return &a, u, nil
}

// gc drops expired in-memory records.
func (a *authState) gc() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, v := range a.pending {
		if now.Sub(v.Created) > 15*time.Minute {
			delete(a.pending, k)
		}
	}
	for k, v := range a.codes {
		if now.Sub(v.Created) > codeTTL {
			delete(a.codes, k)
		}
	}
	for k, v := range a.access {
		if now.After(v.Expires) {
			delete(a.access, k)
		}
	}
}
