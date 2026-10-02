package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
)

// fakeGoogle is a fake Google OAuth server: it "signs in" whichever email the
// test sets in next, issues RS256 ID tokens signed with a test key, and
// supports refresh. It records every request so tests can prove that no
// bifrost token ever reaches Google.
type fakeGoogle struct {
	t       *testing.T
	key     *rsa.PrivateKey
	srv     *httptest.Server
	mu      sync.Mutex
	next    struct{ email, hd string }
	codes   map[string]string // code -> email
	refresh map[string]string // refresh -> email
	seen    []string          // every Authorization header / token value Google received
	noCloud bool              // user declines the cloud-platform scope
	revoked map[string]bool
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeGoogle{t: t, key: k, codes: map[string]string{}, refresh: map[string]string{}, revoked: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) client() *Google {
	g := NewGoogle("test-client.apps.googleusercontent.com", "test-secret")
	g.AuthURL, g.TokenURL, g.CertsURL = f.srv.URL+"/auth", f.srv.URL+"/token", f.srv.URL+"/certs"
	return g
}

func (f *fakeGoogle) idToken(email, hd string) string {
	b64 := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(map[string]any{"iss": "https://accounts.google.com", "aud": "test-client.apps.googleusercontent.com",
		"exp": time.Now().Add(time.Hour).Unix(), "email": email, "email_verified": true, "hd": hd})
	signing := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	return signing + "." + b64(sig)
}

func (f *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/certs":
		n := base64.RawURLEncoding.EncodeToString(f.key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "k1", "kty": "RSA", "n": n, "e": e}}})
	case "/auth":
		// the browser lands here; the test follows the redirect itself
		q := r.URL.Query()
		code := "gcode-" + randToken("")
		f.codes[code] = f.next.email + "|" + f.next.hd
		u, _ := url.Parse(q.Get("redirect_uri"))
		v := url.Values{"code": {code}, "state": {q.Get("state")}}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	case "/token":
		_ = r.ParseForm()
		for _, k := range []string{"code", "refresh_token"} {
			if v := r.PostForm.Get(k); v != "" {
				f.seen = append(f.seen, v)
			}
		}
		scope := "openid email https://www.googleapis.com/auth/cloud-platform"
		if f.noCloud {
			scope = "openid email"
		}
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			who, ok := f.codes[r.PostForm.Get("code")]
			delete(f.codes, r.PostForm.Get("code"))
			if !ok || r.PostForm.Get("code_verifier") == "" {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			parts := strings.SplitN(who, "|", 2)
			rt := "grefresh-" + randToken("")
			f.refresh[rt] = parts[0]
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gaccess-" + parts[0], "expires_in": 3600,
				"refresh_token": rt, "scope": scope, "id_token": f.idToken(parts[0], parts[1])})
		case "refresh_token":
			email, ok := f.refresh[r.PostForm.Get("refresh_token")]
			if !ok || f.revoked[email] {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gaccess-" + email, "expires_in": 3600, "scope": scope})
		}
	}
}

// harness: a bifrost server with fixture backends per user (the fixture's
// cluster user is derived from the email, so we can see who a call ran as).
type harness struct {
	t      *testing.T
	g      *fakeGoogle
	s      *Server
	ts     *httptest.Server
	users  string
	tokens map[string]string // email -> google token the backend was given
	mu     sync.Mutex

	stopAtCode bool // signIn returns the auth code instead of exchanging it
}

func newHarness(t *testing.T, usersYAML string) *harness {
	t.Helper()
	h := &harness{t: t, g: newFakeGoogle(t), tokens: map[string]string{}}
	dir := t.TempDir()
	h.users = filepath.Join(dir, "users.yaml")
	_ = os.WriteFile(h.users, []byte(usersYAML), 0o600)
	cfg := config.Default()
	cfg.Backend = "iap"
	cfg.IAP = config.IAPConfig{Project: "p", Zone: "us-central1-a", Instance: "login"}
	cfg.AuditPath = filepath.Join(dir, "audit.jsonl")
	cfg.Server = config.ServerConfig{BaseURL: "http://placeholder", DataDir: filepath.Join(dir, "data"), UsersFile: h.users, GoogleClientID: "test-client.apps.googleusercontent.com"}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	// bind the listener first so base_url is known
	ts := httptest.NewUnstartedServer(nil)
	cfg.Server.BaseURL = "http://" + ts.Listener.Addr().String()
	s, err := New(cfg, h.g.client(), base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	fixtures, _ := filepath.Abs("../../testdata")
	s.newBackend = func(email string) backend.Backend {
		return &tokenCheckingFixture{h: h, email: email, Fixture: backend.Fixture{Dir: fixtures,
			User: strings.ReplaceAll(strings.ReplaceAll(email, "@", "_"), ".", "_"), Logs: map[string]string{}}}
	}
	ts.Config.Handler = s.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	h.s, h.ts = s, ts
	return h
}

// tokenCheckingFixture asks the server for the user's Google token on every
// run, exactly like backend.IAP does, and records it.
type tokenCheckingFixture struct {
	backend.Fixture
	h     *harness
	email string
}

func (f *tokenCheckingFixture) Run(ctx context.Context, c backend.Command) ([]byte, error) {
	tok, err := f.h.s.tokens.get(ctx, f.email)
	if err != nil {
		return nil, err
	}
	f.h.mu.Lock()
	f.h.tokens[f.email] = tok
	f.h.mu.Unlock()
	return f.Fixture.Run(ctx, c)
}

// signIn runs the whole OAuth flow as an MCP client would: register, authorize
// with PKCE, Google sign-in, code exchange. Returns the bifrost token response.
func (h *harness) signIn(email, hd string) (map[string]any, error) {
	h.g.mu.Lock()
	h.g.next.email, h.g.next.hd = email, hd
	h.g.mu.Unlock()
	base := h.ts.URL
	reg, _ := json.Marshal(map[string]any{"redirect_uris": []string{"http://127.0.0.1:33418/callback"}, "client_name": "test client", "token_endpoint_auth_method": "none"})
	resp, err := http.Post(base+"/register", "application/json", strings.NewReader(string(reg)))
	if err != nil {
		return nil, err
	}
	var c map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&c)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		return nil, fmt.Errorf("register: %d %v", resp.StatusCode, c)
	}
	verifier := randToken("")
	q := url.Values{"response_type": {"code"}, "client_id": {c["client_id"].(string)}, "redirect_uri": {"http://127.0.0.1:33418/callback"},
		"code_challenge": {s256(verifier)}, "code_challenge_method": {"S256"}, "state": {"st123"}, "resource": {base + "/mcp"}}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// consent page -> Google link
	resp, err = noFollow.Get(base + "/authorize?" + q.Encode())
	if err != nil {
		return nil, err
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	i := strings.Index(string(page), `href="`)
	if resp.StatusCode != 200 || i < 0 {
		return nil, fmt.Errorf("authorize: %d %s", resp.StatusCode, page)
	}
	gURL := html.UnescapeString(string(page[i+6 : i+6+strings.Index(string(page[i+6:]), `"`)]))
	// Google -> bifrost callback -> client redirect
	resp, err = noFollow.Get(gURL)
	if err != nil {
		return nil, err
	}
	cb := resp.Header.Get("Location")
	resp.Body.Close()
	if cb == "" {
		return nil, fmt.Errorf("google step: %d (url %s)", resp.StatusCode, gURL)
	}
	resp, err = noFollow.Get(cb)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Location") == "" {
		return nil, fmt.Errorf("callback: %d %s", resp.StatusCode, body)
	}
	final, _ := url.Parse(resp.Header.Get("Location"))
	if e := final.Query().Get("error"); e != "" {
		return nil, fmt.Errorf("%s: %s", e, final.Query().Get("error_description"))
	}
	if final.Query().Get("state") != "st123" {
		return nil, fmt.Errorf("state not returned: %v", final)
	}
	if h.stopAtCode {
		return map[string]any{"code": final.Query().Get("code"), "verifier": verifier, "client_id": c["client_id"]}, nil
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {final.Query().Get("code")}, "client_id": {c["client_id"].(string)},
		"redirect_uri": {"http://127.0.0.1:33418/callback"}, "code_verifier": {verifier}}
	resp, err = http.PostForm(base+"/token", form)
	if err != nil {
		return nil, err
	}
	var tok map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token: %v", tok)
	}
	tok["client_id"] = c["client_id"]
	return tok, nil
}

type bearer struct {
	tok string
	rt  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return b.rt.RoundTrip(r)
}

func (h *harness) mcp(tok string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{tok, http.DefaultTransport}}}, nil)
}

func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var m map[string]any
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			_ = json.Unmarshal([]byte(tc.Text), &m)
			if m == nil {
				m = map[string]any{"error": tc.Text}
			}
		}
	}
	return m, res.IsError
}

func toolSet(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	r, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, x := range r.Tools {
		m[x.Name] = true
	}
	return m
}

const twoUsers = `domain: ucr.edu
users:
  - email: alice@ucr.edu
    tiers: [R1, R2, A1]
  - email: bob@ucr.edu
    tiers: [R1]
`

func TestDiscoveryAndChallenge(t *testing.T) {
	h := newHarness(t, twoUsers)
	if r, _ := http.Get(h.ts.URL + "/health"); r.StatusCode != 200 {
		t.Errorf("/health: %d", r.StatusCode)
	}
	resp, _ := http.Post(h.ts.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource") {
		t.Fatalf("challenge: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	var meta map[string]any
	r2, _ := http.Get(h.ts.URL + "/.well-known/oauth-authorization-server")
	_ = json.NewDecoder(r2.Body).Decode(&meta)
	if meta["code_challenge_methods_supported"].([]any)[0] != "S256" || meta["registration_endpoint"] == nil {
		t.Errorf("AS metadata: %v", meta)
	}
}

func TestUsersAreSeparate(t *testing.T) {
	h := newHarness(t, twoUsers)
	a, err := h.signIn("alice@ucr.edu", "ucr.edu")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.signIn("bob@ucr.edu", "ucr.edu")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := h.mcp(a["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	cb, err := h.mcp(b["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	// tiers per person: alice sees staff + act tools, bob only R1
	ta, tb := toolSet(t, ca), toolSet(t, cb)
	if !ta["health"] || !ta["job_submit"] {
		t.Errorf("alice tools: %v", ta)
	}
	if tb["health"] || tb["job_submit"] || !tb["jobs_list"] {
		t.Errorf("bob tools: %v", tb)
	}
	// each person's calls use their own Google token, never the other's
	callJSON(t, ca, "jobs_list", nil)
	callJSON(t, cb, "jobs_list", nil)
	if h.tokens["alice@ucr.edu"] != "gaccess-alice@ucr.edu" || h.tokens["bob@ucr.edu"] != "gaccess-bob@ucr.edu" {
		t.Errorf("tokens used: %v", h.tokens)
	}
	// bob cannot call a staff tool even by name: it does not exist on his server
	if _, err := cb.CallTool(context.Background(), &mcp.CallToolParams{Name: "health"}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("bob called a staff tool: %v", err)
	}
	// the audit log names the person, not the server
	raw, _ := os.ReadFile(h.s.cfg.AuditPath)
	if !strings.Contains(string(raw), `"caller":"alice@ucr.edu"`) || !strings.Contains(string(raw), `"caller":"bob@ucr.edu"`) {
		t.Errorf("audit callers: %s", raw)
	}
	// results download is refused on the hosted server
	if m, isErr := callJSON(t, ca, "job_results", map[string]any{"job_id": "236", "download": true}); !isErr || !strings.Contains(fmt.Sprint(m), "hosted server") {
		t.Errorf("download on server: %v", m)
	}
	// no bifrost token ever reached Google
	for _, v := range h.g.seen {
		if strings.HasPrefix(v, "bfx_") || strings.HasPrefix(v, "bfr_") {
			t.Errorf("bifrost token sent to Google: %s", v[:8])
		}
	}
}

func TestSignInRefused(t *testing.T) {
	h := newHarness(t, twoUsers)
	if _, err := h.signIn("mallory@ucr.edu", "ucr.edu"); err == nil || !strings.Contains(err.Error(), "not on the ursa-bifrost user list") {
		t.Errorf("unlisted user: %v", err)
	}
	if _, err := h.signIn("alice@gmail.com", ""); err == nil || !strings.Contains(err.Error(), "only verified ucr.edu") {
		t.Errorf("other domain: %v", err)
	}
	h.g.noCloud = true
	if _, err := h.signIn("alice@ucr.edu", "ucr.edu"); err == nil || !strings.Contains(err.Error(), "Google Cloud access was not granted") {
		t.Errorf("no cloud scope: %v", err)
	}
}

func TestRemovingAUserCutsThemOff(t *testing.T) {
	h := newHarness(t, twoUsers)
	b, err := h.signIn("bob@ucr.edu", "ucr.edu")
	if err != nil {
		t.Fatal(err)
	}
	cb, err := h.mcp(b["access_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	callJSON(t, cb, "jobs_list", nil)
	// remove bob (new mtime) -> next request is refused, refresh refused
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(h.users, []byte("domain: ucr.edu\nusers:\n  - email: alice@ucr.edu\n    tiers: [R1]\n"), 0o600)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(h.users, future, future)
	if _, err := h.mcp(b["access_token"].(string)); err == nil {
		t.Error("removed user still connected")
	}
	resp, _ := http.PostForm(h.ts.URL+"/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {b["refresh_token"].(string)}, "client_id": {b["client_id"].(string)}})
	if resp.StatusCode == 200 {
		t.Error("removed user refreshed a token")
	}
}

func TestTierChangeTakesEffect(t *testing.T) {
	h := newHarness(t, twoUsers)
	b, _ := h.signIn("bob@ucr.edu", "ucr.edu")
	cb, _ := h.mcp(b["access_token"].(string))
	if toolSet(t, cb)["health"] {
		t.Fatal("bob has R2 already")
	}
	time.Sleep(10 * time.Millisecond)
	_ = os.WriteFile(h.users, []byte(strings.Replace(twoUsers, "tiers: [R1]\n", "tiers: [R1, R2]\n", 1)), 0o600)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(h.users, future, future)
	cb2, _ := h.mcp(b["access_token"].(string))
	if !toolSet(t, cb2)["health"] {
		t.Error("granted R2 not visible")
	}
}

func TestTokenEndpointRules(t *testing.T) {
	h := newHarness(t, twoUsers)
	a, err := h.signIn("alice@ucr.edu", "ucr.edu")
	if err != nil {
		t.Fatal(err)
	}
	post := func(v url.Values) int {
		r, _ := http.PostForm(h.ts.URL+"/token", v)
		return r.StatusCode
	}
	rt, cid := a["refresh_token"].(string), a["client_id"].(string)
	// refresh with the wrong client is refused
	if post(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {"bfc_other"}}) == 200 {
		t.Error("refresh by another client")
	}
	// refresh rotates: first use works, second use of the same token fails
	if post(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}}) != 200 {
		t.Error("valid refresh failed")
	}
	if post(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {cid}}) == 200 {
		t.Error("refresh token reused after rotation")
	}
	// a made-up access token is refused
	if _, err := h.mcp("bfx_madeup"); err == nil {
		t.Error("made-up token accepted")
	}
}

func TestAuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	h := newHarness(t, twoUsers)
	reg, _ := json.Marshal(map[string]any{"redirect_uris": []string{"http://127.0.0.1:1/callback"}})
	resp, _ := http.Post(h.ts.URL+"/register", "application/json", strings.NewReader(string(reg)))
	var c map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&c)
	q := url.Values{"response_type": {"code"}, "client_id": {c["client_id"].(string)}, "redirect_uri": {"https://evil.example/cb"},
		"code_challenge": {"x"}, "code_challenge_method": {"S256"}}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r2, _ := noFollow.Get(h.ts.URL + "/authorize?" + q.Encode())
	if r2.StatusCode != 400 || r2.Header.Get("Location") != "" {
		t.Errorf("unregistered redirect: %d %q", r2.StatusCode, r2.Header.Get("Location"))
	}
	// registration refuses non-loopback http and non-https schemes
	for _, bad := range []string{"http://evil.example/cb", "javascript:alert(1)", "myapp://cb"} {
		reg, _ := json.Marshal(map[string]any{"redirect_uris": []string{bad}})
		r3, _ := http.Post(h.ts.URL+"/register", "application/json", strings.NewReader(string(reg)))
		if r3.StatusCode == 201 {
			t.Errorf("registered %s", bad)
		}
	}
	// PKCE is required
	q.Set("redirect_uri", "http://127.0.0.1:1/callback")
	q.Del("code_challenge")
	r4, _ := noFollow.Get(h.ts.URL + "/authorize?" + q.Encode())
	if loc := r4.Header.Get("Location"); !strings.Contains(loc, "error=invalid_request") {
		t.Errorf("no PKCE: %d %q", r4.StatusCode, loc)
	}
}

func TestSignoutAndGoogleRevocation(t *testing.T) {
	h := newHarness(t, twoUsers)
	a, _ := h.signIn("alice@ucr.edu", "ucr.edu")
	ca, _ := h.mcp(a["access_token"].(string))
	callJSON(t, ca, "jobs_list", nil)
	// person revokes bifrost in their Google account: next token refresh fails, session ends
	h.g.mu.Lock()
	h.g.revoked["alice@ucr.edu"] = true
	h.g.mu.Unlock()
	h.s.tokens.drop("alice@ucr.edu")
	if m, isErr := callJSON(t, ca, "jobs_list", map[string]any{"since": "now-1days"}); !isErr || !strings.Contains(fmt.Sprint(m), "revoked") {
		t.Errorf("after Google revocation: %v", m)
	}
	// sign-out
	b, _ := h.signIn("bob@ucr.edu", "ucr.edu")
	req, _ := http.NewRequest("POST", h.ts.URL+"/signout", nil)
	req.Header.Set("Authorization", "Bearer "+b["access_token"].(string))
	r, _ := http.DefaultClient.Do(req)
	if r.StatusCode != 200 {
		t.Fatalf("signout %d", r.StatusCode)
	}
	if _, err := h.mcp(b["access_token"].(string)); err == nil {
		t.Error("token works after sign-out")
	}
	resp, _ := http.PostForm(h.ts.URL+"/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {b["refresh_token"].(string)}, "client_id": {b["client_id"].(string)}})
	if resp.StatusCode == 200 {
		t.Error("refresh after sign-out")
	}
}

func TestSealedStorage(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s, _ := NewSealer(base64.StdEncoding.EncodeToString(key))
	st := NewStore(t.TempDir(), s)
	_ = st.Put("session", "alice@ucr.edu", session{Email: "alice@ucr.edu", GoogleRefreshToken: "1//secret-refresh"})
	// on disk: no plaintext
	files, _ := filepath.Glob(filepath.Join(st.dir, "session", "*"))
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "secret-refresh") || strings.Contains(string(raw), "alice") {
		t.Error("plaintext on disk")
	}
	if fi, _ := os.Stat(files[0]); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	// a record cannot be moved to another user's slot
	_ = os.WriteFile(st.path("session", "bob@ucr.edu"), raw, 0o600)
	if ok, err := st.Get("session", "bob@ucr.edu", &session{}); ok || err == nil {
		t.Error("swapped record accepted")
	}
	// wrong key fails
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	s2, _ := NewSealer(base64.StdEncoding.EncodeToString(other))
	if ok, _ := NewStore(st.dir, s2).Get("session", "alice@ucr.edu", &session{}); ok {
		t.Error("opened with the wrong key")
	}
}

func TestCodeExchangeAttacks(t *testing.T) {
	h := newHarness(t, twoUsers)
	h.stopAtCode = true
	got, err := h.signIn("alice@ucr.edu", "ucr.edu")
	if err != nil {
		t.Fatal(err)
	}
	code, verifier, cid := got["code"].(string), got["verifier"].(string), got["client_id"].(string)
	post := func(v url.Values) int {
		r, _ := http.PostForm(h.ts.URL+"/token", v)
		return r.StatusCode
	}
	base := func() url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {cid},
			"redirect_uri": {"http://127.0.0.1:33418/callback"}, "code_verifier": {verifier}}
	}
	// a stolen code without the right PKCE verifier is useless
	v := base()
	v.Set("code_verifier", "wrong-verifier-"+randToken(""))
	if post(v) == 200 {
		t.Error("code exchanged with the wrong PKCE verifier")
	}
	// (the failed attempt burned the code: codes are single use, even on failure)
	if post(base()) == 200 {
		t.Error("code still valid after a failed exchange")
	}
	// fresh code: exchange once, then replay
	got, _ = h.signIn("alice@ucr.edu", "ucr.edu")
	code, verifier, cid = got["code"].(string), got["verifier"].(string), got["client_id"].(string)
	if post(base()) != 200 {
		t.Fatal("valid exchange failed")
	}
	if post(base()) == 200 {
		t.Error("authorization code replayed")
	}
}

func TestIDTokenForAnotherAppRejected(t *testing.T) {
	f := newFakeGoogle(t)
	g := f.client()
	if _, err := g.VerifyIDToken(context.Background(), f.idToken("alice@ucr.edu", "ucr.edu")); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	// same signer, but minted for a different OAuth client (another app's token)
	g.ClientID = "some-other-app.apps.googleusercontent.com"
	if _, err := g.VerifyIDToken(context.Background(), f.idToken("alice@ucr.edu", "ucr.edu")); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Errorf("token for another app accepted: %v", err)
	}
	// tampered claims break the signature
	g.ClientID = "test-client.apps.googleusercontent.com"
	parts := strings.Split(f.idToken("alice@ucr.edu", "ucr.edu"), ".")
	evil, _ := json.Marshal(map[string]any{"iss": "https://accounts.google.com", "aud": g.ClientID, "exp": time.Now().Add(time.Hour).Unix(),
		"email": "admin@ucr.edu", "email_verified": true, "hd": "ucr.edu"})
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(evil) + "." + parts[2]
	if _, err := g.VerifyIDToken(context.Background(), forged); err == nil {
		t.Error("forged ID token accepted")
	}
}

// TestTokensSurviveRestart: a new server process (Cloud Run scale-to-zero)
// still accepts a token issued before, and still refuses it after sign-out.
func TestTokensSurviveRestart(t *testing.T) {
	h := newHarness(t, twoUsers)
	a, err := h.signIn("alice@ucr.edu", "ucr.edu")
	if err != nil {
		t.Fatal(err)
	}
	tok := a["access_token"].(string)
	// simulate a restart: wipe every in-memory cache
	h.s.auth = newAuthState()
	h.s.tokens.m = map[string]cachedTok{}
	h.s.conns = map[string]*userConn{}
	cs, err := h.mcp(tok)
	if err != nil {
		t.Fatalf("token lost on restart: %v", err)
	}
	callJSON(t, cs, "jobs_list", nil)
	// sign out, restart again: the stored token must not come back
	req, _ := http.NewRequest("POST", h.ts.URL+"/signout", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Fatal("signout")
	}
	h.s.auth = newAuthState()
	if _, err := h.mcp(tok); err == nil {
		t.Error("token works after sign-out + restart")
	}
}
