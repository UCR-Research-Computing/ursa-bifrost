package server

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Google is the Google OAuth client bifrost uses for sign-in. Endpoints are
// fields so tests can point them at a fake.
type Google struct {
	ClientID     string
	ClientSecret string
	AuthURL      string // https://accounts.google.com/o/oauth2/v2/auth
	TokenURL     string // https://oauth2.googleapis.com/token
	CertsURL     string // https://www.googleapis.com/oauth2/v3/certs
	HTTP         *http.Client

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	keysAt  time.Time
	issuers []string
}

// NewGoogle returns a Google client with the real endpoints.
func NewGoogle(id, secret string) *Google {
	return &Google{ClientID: id, ClientSecret: secret,
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		CertsURL: "https://www.googleapis.com/oauth2/v3/certs",
		HTTP:     &http.Client{Timeout: 20 * time.Second},
		issuers:  []string{"https://accounts.google.com", "accounts.google.com"}}
}

// TokenResponse is Google's token endpoint answer.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (g *Google) token(ctx context.Context, form url.Values) (*TokenResponse, error) {
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	req, _ := http.NewRequestWithContext(ctx, "POST", g.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t TokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return nil, fmt.Errorf("google token: %w", err)
	}
	if resp.StatusCode != 200 || t.Error != "" {
		return &t, fmt.Errorf("google token: %s %s", t.Error, t.ErrorDesc)
	}
	return &t, nil
}

// Exchange trades an authorization code (with PKCE verifier) for tokens.
func (g *Google) Exchange(ctx context.Context, code, redirect, verifier string) (*TokenResponse, error) {
	return g.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirect}, "code_verifier": {verifier}})
}

// Refresh gets a new access token from a refresh token. invalid_grant means
// the person revoked access (or it expired): the session is over.
func (g *Google) Refresh(ctx context.Context, refresh string) (*TokenResponse, error) {
	return g.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

// IDClaims are the parts of a Google ID token bifrost uses.
type IDClaims struct {
	Iss           string `json:"iss"`
	Aud           string `json:"aud"`
	Exp           int64  `json:"exp"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	HD            string `json:"hd"`
}

// VerifyIDToken checks an RS256 Google ID token's signature, issuer,
// audience and expiry.
func (g *Google) VerifyIDToken(ctx context.Context, tok string) (*IDClaims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("id token: malformed")
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, errors.New("id token: bad encoding")
	}
	var h struct{ Alg, Kid string }
	if err := json.Unmarshal(hb, &h); err != nil || h.Alg != "RS256" {
		return nil, errors.New("id token: not RS256")
	}
	key, err := g.key(ctx, h.Kid)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return nil, errors.New("id token: bad signature")
	}
	var c IDClaims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, errors.New("id token: bad claims")
	}
	okIss := false
	for _, i := range g.issuers {
		if c.Iss == i {
			okIss = true
		}
	}
	switch {
	case !okIss:
		return nil, errors.New("id token: wrong issuer")
	case c.Aud != g.ClientID:
		return nil, errors.New("id token: wrong audience")
	case time.Now().Unix() > c.Exp+60:
		return nil, errors.New("id token: expired")
	case c.Email == "":
		return nil, errors.New("id token: no email")
	}
	c.Email = strings.ToLower(c.Email)
	return &c, nil
}

func (g *Google) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if k, ok := g.keys[kid]; ok && time.Since(g.keysAt) < 6*time.Hour {
		return k, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", g.CertsURL, nil)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google certs: %w", err)
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct{ Kid, N, E, Kty string } `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("google certs: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	g.keys, g.keysAt = keys, time.Now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("id token: unknown signing key")
}

// tokenCache holds each user's current Google access token (memory only) and
// refreshes it from the sealed refresh token when it is near expiry.
type tokenCache struct {
	g     *Google
	store *Store
	mu    sync.Mutex
	m     map[string]cachedTok
}

type cachedTok struct {
	tok string
	exp time.Time
}

func (c *tokenCache) seed(email, tok string, exp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[email] = cachedTok{tok, exp}
}

func (c *tokenCache) drop(email string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, email)
}

// get returns a valid Google access token for email.
func (c *tokenCache) get(ctx context.Context, email string) (string, error) {
	c.mu.Lock()
	t, ok := c.m[email]
	c.mu.Unlock()
	if ok && time.Until(t.exp) > 2*time.Minute {
		return t.tok, nil
	}
	var s session
	found, err := c.store.Get("session", email, &s)
	if err != nil || !found {
		return "", errors.New("not signed in to Google (sign in again)")
	}
	r, err := c.g.Refresh(ctx, s.GoogleRefreshToken)
	if err != nil {
		if r != nil && r.Error == "invalid_grant" {
			_ = c.store.Delete("session", email) // access revoked in the Google account
			return "", errors.New("Google access was revoked or expired; sign in again")
		}
		return "", err
	}
	c.seed(email, r.AccessToken, time.Now().Add(time.Duration(r.ExpiresIn)*time.Second))
	return r.AccessToken, nil
}
