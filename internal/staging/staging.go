// Package staging is bifrost's private Cloud Storage staging area (SPEC
// section 18): V4 signed URLs for uploads and downloads, and the few object
// lookups the tools need. File bytes never pass through bifrost.
//
// Signing uses the IAM Credentials signBlob API as a service account, so no
// private key file exists anywhere. Tests replace SignBytes with a local key.
package staging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Object is one staged object.
type Object struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Generation string    `json:"generation"`
	Created    time.Time `json:"created"`
}

// Client is what the core needs from the staging area.
type Client interface {
	// SignURL returns a V4 signed URL for one object. Extra headers are signed
	// (the client must send them exactly).
	SignURL(ctx context.Context, method, object string, ttl time.Duration, headers map[string]string) (string, error)
	// List returns the objects under a prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Stat returns one object (found=false when it does not exist).
	Stat(ctx context.Context, object string) (Object, bool, error)
	// Bucket is the bucket name.
	Bucket() string
}

// MaxTTL is the longest a V4 signed URL may live.
const MaxTTL = 7 * 24 * time.Hour

// GCS is the real staging area.
type GCS struct {
	BucketName string
	// SignAs is the service account that signs URLs (Cloud Run: the service's
	// own account, discovered from the metadata server when empty).
	SignAs string
	// Token returns an OAuth access token for storage and IAM calls.
	Token func(context.Context) (string, error)
	// SignBytes signs with the service account's key (default: IAM signBlob).
	SignBytes func(ctx context.Context, b []byte) ([]byte, error)
	HTTP      *http.Client
	Now       func() time.Time

	// StorageBase and IAMBase are overridable for tests.
	StorageBase string
	IAMBase     string

	mu sync.Mutex
}

// Bucket is the bucket name.
func (g *GCS) Bucket() string { return g.BucketName }

func (g *GCS) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GCS) http() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *GCS) storageBase() string {
	if g.StorageBase != "" {
		return g.StorageBase
	}
	return "https://storage.googleapis.com"
}

func (g *GCS) iamBase() string {
	if g.IAMBase != "" {
		return g.IAMBase
	}
	return "https://iamcredentials.googleapis.com"
}

func (g *GCS) signer(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.SignAs != "" {
		return g.SignAs, nil
	}
	email, err := metadataGet(ctx, "instance/service-accounts/default/email")
	if err != nil {
		return "", fmt.Errorf("staging.sign_as is not set and the metadata server did not say: %w", err)
	}
	g.SignAs = strings.TrimSpace(email)
	return g.SignAs, nil
}

// uriEncode percent-encodes everything except RFC 3986 unreserved characters.
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func encodePath(object string) string {
	parts := strings.Split(object, "/")
	for i, p := range parts {
		parts[i] = uriEncode(p)
	}
	return strings.Join(parts, "/")
}

// StringToSign builds the V4 canonical request and string to sign. It is
// exported for tests, which check the signature against a local key.
func StringToSign(method, bucket, object, email string, now time.Time, ttl time.Duration, headers map[string]string) (sts, canonURI, canonQuery string) {
	now = now.UTC()
	ts := now.Format("20060102T150405Z")
	scope := now.Format("20060102") + "/auto/storage/goog4_request"
	hdr := map[string]string{"host": "storage.googleapis.com"}
	for k, v := range headers {
		hdr[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	keys := make([]string, 0, len(hdr))
	for k := range hdr {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ch strings.Builder
	for _, k := range keys {
		ch.WriteString(k + ":" + hdr[k] + "\n")
	}
	signed := strings.Join(keys, ";")
	q := map[string]string{
		"X-Goog-Algorithm":     "GOOG4-RSA-SHA256",
		"X-Goog-Credential":    email + "/" + scope,
		"X-Goog-Date":          ts,
		"X-Goog-Expires":       strconv.Itoa(int(ttl / time.Second)),
		"X-Goog-SignedHeaders": signed,
	}
	qk := make([]string, 0, len(q))
	for k := range q {
		qk = append(qk, k)
	}
	sort.Strings(qk)
	qs := make([]string, len(qk))
	for i, k := range qk {
		qs[i] = uriEncode(k) + "=" + uriEncode(q[k])
	}
	canonQuery = strings.Join(qs, "&")
	canonURI = "/" + bucket + "/" + encodePath(object)
	creq := method + "\n" + canonURI + "\n" + canonQuery + "\n" + ch.String() + "\n" + signed + "\nUNSIGNED-PAYLOAD"
	h := sha256.Sum256([]byte(creq))
	sts = "GOOG4-RSA-SHA256\n" + ts + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	return sts, canonURI, canonQuery
}

// SignURL returns a V4 signed URL.
func (g *GCS) SignURL(ctx context.Context, method, object string, ttl time.Duration, headers map[string]string) (string, error) {
	if method != http.MethodGet && method != http.MethodPut {
		return "", fmt.Errorf("method %s not allowed", method)
	}
	if ttl < time.Second || ttl > MaxTTL {
		return "", fmt.Errorf("link lifetime must be 1 s to 7 days")
	}
	if object == "" || strings.HasPrefix(object, "/") || strings.Contains(object, "..") {
		return "", fmt.Errorf("bad object name %q", object)
	}
	email, err := g.signer(ctx)
	if err != nil {
		return "", err
	}
	sts, uri, q := StringToSign(method, g.BucketName, object, email, g.now(), ttl, headers)
	sign := g.SignBytes
	if sign == nil {
		sign = g.signBlob
	}
	sig, err := sign(ctx, []byte(sts))
	if err != nil {
		return "", fmt.Errorf("signing the link: %w", err)
	}
	return "https://storage.googleapis.com" + uri + "?" + q + "&X-Goog-Signature=" + hex.EncodeToString(sig), nil
}

func (g *GCS) do(ctx context.Context, method, u string, body []byte, out any) (int, error) {
	if g.Token == nil {
		return 0, errors.New("staging has no token source")
	}
	tok, err := g.Token(ctx)
	if err != nil {
		return 0, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusNotFound {
		return resp.StatusCode, nil
	}
	if resp.StatusCode/100 != 2 {
		msg := string(b)
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d: %s", method, strings.SplitN(u, "?", 2)[0], resp.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (g *GCS) signBlob(ctx context.Context, b []byte) ([]byte, error) {
	email, err := g.signer(ctx)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(b)})
	var out struct {
		SignedBlob string `json:"signedBlob"`
	}
	u := g.iamBase() + "/v1/projects/-/serviceAccounts/" + url.PathEscape(email) + ":signBlob"
	code, err := g.do(ctx, http.MethodPost, u, body, &out)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound || out.SignedBlob == "" {
		return nil, fmt.Errorf("signBlob: service account %s not found", email)
	}
	return base64.StdEncoding.DecodeString(out.SignedBlob)
}

type apiObject struct {
	Name        string `json:"name"`
	Size        string `json:"size"`
	Generation  string `json:"generation"`
	TimeCreated string `json:"timeCreated"`
}

func (a apiObject) object() Object {
	n, _ := strconv.ParseInt(a.Size, 10, 64)
	t, _ := time.Parse(time.RFC3339, a.TimeCreated)
	return Object{Name: a.Name, Size: n, Generation: a.Generation, Created: t}
}

// List returns every object under prefix (paged through the JSON API).
func (g *GCS) List(ctx context.Context, prefix string) ([]Object, error) {
	var all []Object
	page := ""
	for i := 0; i < 50; i++ {
		u := g.storageBase() + "/storage/v1/b/" + url.PathEscape(g.BucketName) + "/o?prefix=" + url.QueryEscape(prefix) +
			"&fields=" + url.QueryEscape("items(name,size,generation,timeCreated),nextPageToken")
		if page != "" {
			u += "&pageToken=" + url.QueryEscape(page)
		}
		var out struct {
			Items         []apiObject `json:"items"`
			NextPageToken string      `json:"nextPageToken"`
		}
		code, err := g.do(ctx, http.MethodGet, u, nil, &out)
		if err != nil {
			return nil, err
		}
		if code == http.StatusNotFound {
			return nil, fmt.Errorf("staging bucket %s not found", g.BucketName)
		}
		for _, it := range out.Items {
			all = append(all, it.object())
		}
		if out.NextPageToken == "" {
			return all, nil
		}
		page = out.NextPageToken
	}
	return all, nil
}

// Stat returns one object's metadata.
func (g *GCS) Stat(ctx context.Context, object string) (Object, bool, error) {
	u := g.storageBase() + "/storage/v1/b/" + url.PathEscape(g.BucketName) + "/o/" + url.PathEscape(object) +
		"?fields=" + url.QueryEscape("name,size,generation,timeCreated")
	var out apiObject
	code, err := g.do(ctx, http.MethodGet, u, nil, &out)
	if err != nil {
		return Object{}, false, err
	}
	if code == http.StatusNotFound {
		return Object{}, false, nil
	}
	return out.object(), true, nil
}

// ---- token sources -----------------------------------------------------------

const metadataBase = "http://metadata.google.internal/computeMetadata/v1/"

func metadataGet(ctx context.Context, p string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataBase+p, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("metadata %s: HTTP %d", p, resp.StatusCode)
	}
	return string(b), nil
}

// MetadataToken returns the attached service account's token (Cloud Run, GCE),
// cached until a minute before it expires.
func MetadataToken() func(context.Context) (string, error) {
	var mu sync.Mutex
	var tok string
	var exp time.Time
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if tok != "" && time.Now().Before(exp) {
			return tok, nil
		}
		b, err := metadataGet(ctx, "instance/service-accounts/default/token")
		if err != nil {
			return "", err
		}
		var t struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.Unmarshal([]byte(b), &t); err != nil || t.AccessToken == "" {
			return "", fmt.Errorf("metadata token: unexpected answer")
		}
		tok, exp = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn-60)*time.Second)
		return tok, nil
	}
}

// CommandToken runs argv (`gcloud auth print-access-token`) and caches the
// answer for 10 minutes (laptop CLI).
func CommandToken(argv []string) func(context.Context) (string, error) {
	var mu sync.Mutex
	var tok string
	var at time.Time
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if tok != "" && time.Since(at) < 10*time.Minute {
			return tok, nil
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).Output()
		if err != nil {
			return "", fmt.Errorf("%s: %v", argv[0], err)
		}
		tok, at = strings.TrimSpace(string(out)), time.Now()
		return tok, nil
	}
}
