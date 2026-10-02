package backend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// IAP runs allow-listed commands on the login node AS ONE GOOGLE USER, using
// only that user's OAuth access token: no gcloud, no ssh binary, no service
// account. Per connection it
//
//  1. registers a short-lived ed25519 key on the user's OS Login profile and
//     learns their POSIX username (oslogin.googleapis.com),
//  2. opens an IAP TCP-forwarding tunnel to port 22 (tunnel.cloudproxy.app),
//  3. speaks SSH over it as that POSIX user, pinning the host key.
//
// Authorization is entirely Google's: IAP tunnelResourceAccessor, OS Login and
// the slurm-login group decide who gets in, exactly as for `gcloud compute ssh`.
// One IAP value serves one user; the HTTP server keeps one per signed-in user.
type IAP struct {
	Project, Zone, Instance string
	Email                   string
	// Token returns a current Google OAuth access token for Email.
	Token func(ctx context.Context) (string, error)
	// HostKeys are the login node's accepted host keys (authorized_keys format).
	// Empty = trust on first use for this IAP value, then pinned.
	HostKeys []string
	// KeyTTL is the OS Login key lifetime (default 8 h). The key is reused
	// across connections until 10 minutes before it expires, because the login
	// node caches a user's key list: a key imported seconds after a lookup is
	// rejected for ~5-30 s (measured live 2026-10-01). OS Login removes it at
	// expiry even if bifrost never runs again.
	KeyTTL time.Duration
	// Keys stores the per-user key between connections and processes
	// (laptop: FileKeyStore; server: encrypted store). Nil = in-memory only.
	Keys KeyStore
	// Idle closes the SSH connection after this long unused (default 5 min).
	Idle time.Duration

	// test hooks
	osloginBase string // default https://oslogin.googleapis.com
	dial        func(ctx context.Context, token string) (net.Conn, error)

	connMu   sync.Mutex // serializes connection setup
	mu       sync.Mutex // guards the fields below
	client   *ssh.Client
	user     string
	keyLine  string // the imported authorized_keys line (its sha256 is the key id)
	lastUsed time.Time
	timer    *time.Timer
	pinned   ssh.PublicKey
}

// Name is the backend label used in `source`.
func (b *IAP) Name() string { return "iap-oslogin:" + b.Instance }

// PosixUser is the OS Login username (empty before the first connection).
func (b *IAP) PosixUser() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.user
}

func (b *IAP) base() string {
	if b.osloginBase != "" {
		return b.osloginBase
	}
	return "https://oslogin.googleapis.com"
}

// ---- OS Login ----------------------------------------------------------------

type osloginProfile struct {
	PosixAccounts []struct {
		Username string `json:"username"`
		Primary  bool   `json:"primary"`
	} `json:"posixAccounts"`
	SSHPublicKeys map[string]struct {
		Key         string `json:"key"`
		Fingerprint string `json:"fingerprint"`
	} `json:"sshPublicKeys"`
}

func (b *IAP) osloginDo(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	tok, err := b.Token(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: google token: %v", ErrUnreachable, err)
	}
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.base()+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: os login: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return out, resp.StatusCode, nil
}

// keyID is how OS Login names a key: sha256 of the exact imported line
// (comment included). Verified live 2026-10-01: the key-blob hash returns 200
// on DELETE and removes nothing.
func keyID(line string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(line))) }

func (b *IAP) importKey(ctx context.Context, pub ssh.PublicKey) (user, line string, expires time.Time, err error) {
	ttl := b.KeyTTL
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	expires = time.Now().Add(ttl)
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " ursa-bifrost"
	// regions=<zone's region> makes the call return only after the key is
	// written where the node reads it (gcloud does the same). Without it, the
	// node did not see the key for 30 s+ in live tests.
	region := b.Zone
	if i := strings.LastIndex(region, "-"); i > 0 {
		region = region[:i]
	}
	out, code, err := b.osloginDo(ctx, "POST",
		fmt.Sprintf("/v1/users/%s:importSshPublicKey?projectId=%s&regions=%s", url.PathEscape(b.Email), url.QueryEscape(b.Project), url.QueryEscape(region)),
		map[string]any{"key": line, "expirationTimeUsec": fmt.Sprint(expires.UnixMicro())})
	if err != nil {
		return "", "", time.Time{}, err
	}
	if code != 200 {
		return "", "", time.Time{}, fmt.Errorf("%w: OS Login refused the key for %s (HTTP %d): %s", ErrUnreachable, b.Email, code, oneLine(out))
	}
	var r struct {
		LoginProfile osloginProfile `json:"loginProfile"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", "", time.Time{}, fmt.Errorf("os login: %w", err)
	}
	for _, a := range r.LoginProfile.PosixAccounts {
		if a.Primary || user == "" {
			user = a.Username
		}
	}
	if user == "" {
		return "", "", time.Time{}, fmt.Errorf("%w: %s has no POSIX account on OS Login (not allowed on the cluster?)", ErrUnreachable, b.Email)
	}
	if err := ValidUser(user); err != nil {
		return "", "", time.Time{}, err
	}
	if _, ok := r.LoginProfile.SSHPublicKeys[keyID(line)]; !ok {
		// the API changed how it names keys: refuse rather than leak keys we cannot delete
		_ = b.deleteKeyLine(ctx, line)
		return "", "", time.Time{}, errors.New("os login: imported key not found under its expected id; refusing to continue")
	}
	return user, line, expires, nil
}

func (b *IAP) deleteKeyLine(ctx context.Context, line string) error {
	_, code, err := b.osloginDo(ctx, "DELETE",
		fmt.Sprintf("/v1/users/%s/sshPublicKeys/%s", url.PathEscape(b.Email), keyID(line)), nil)
	if err != nil {
		return err
	}
	if code != 200 && code != 404 {
		return fmt.Errorf("os login: delete key: HTTP %d", code)
	}
	return nil
}

// ---- IAP relay ---------------------------------------------------------------

const (
	iapTagConnectSID = 0x0001
	iapTagData       = 0x0004
	iapTagAck        = 0x0007
	iapMaxFrame      = 16384
)

// iapConn is a net.Conn over the IAP relay WebSocket subprotocol
// (relay.tunnel.cloudproxy.app): CONNECT_SUCCESS_SID, then DATA frames
// (tag, uint32 length, bytes) both ways and ACK frames (tag, uint64 total).
type iapConn struct {
	ws      *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	wmu     sync.Mutex
	rmu     sync.Mutex
	buf     bytes.Buffer
	recvd   uint64
	lastAck uint64
}

func dialIAPRelay(ctx context.Context, rawURL, token string) (*iapConn, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Origin", "bot:iap-tunneler")
	h.Set("User-Agent", "ursa-bifrost")
	ws, resp, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{HTTPHeader: h, Subprotocols: []string{"relay.tunnel.cloudproxy.app"}})
	if err != nil {
		msg := err.Error()
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
			msg = fmt.Sprintf("%s: %s", resp.Status, oneLine(b))
			if resp.StatusCode == 403 || resp.StatusCode == 401 {
				return nil, fmt.Errorf("%w: IAP denied the tunnel (needs roles/iap.tunnelResourceAccessor): %s", ErrUnreachable, msg)
			}
		}
		return nil, fmt.Errorf("%w: IAP tunnel: %s", ErrUnreachable, msg)
	}
	ws.SetReadLimit(1 << 20)
	cctx, cancel := context.WithCancel(context.Background())
	c := &iapConn{ws: ws, ctx: cctx, cancel: cancel}
	_, msg, err := ws.Read(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: IAP: no connect frame: %v", ErrUnreachable, err)
	}
	if len(msg) < 2 || binary.BigEndian.Uint16(msg) != iapTagConnectSID {
		cancel()
		_ = ws.CloseNow()
		return nil, fmt.Errorf("%w: IAP: unexpected first frame", ErrUnreachable)
	}
	return c, nil
}

func (c *iapConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for c.buf.Len() == 0 {
		_, msg, err := c.ws.Read(c.ctx)
		if err != nil {
			return 0, io.EOF
		}
		if len(msg) < 2 {
			continue
		}
		if binary.BigEndian.Uint16(msg) != iapTagData {
			continue // ACKs for our own bytes; nothing to do
		}
		if len(msg) < 6 {
			return 0, errors.New("iap: short data frame")
		}
		n := binary.BigEndian.Uint32(msg[2:6])
		if int(n) > len(msg)-6 {
			return 0, errors.New("iap: data frame length mismatch")
		}
		c.buf.Write(msg[6 : 6+n])
		c.recvd += uint64(n)
		if c.recvd-c.lastAck >= 2*iapMaxFrame {
			ack := make([]byte, 10)
			binary.BigEndian.PutUint16(ack, iapTagAck)
			binary.BigEndian.PutUint64(ack[2:], c.recvd)
			c.wmu.Lock()
			err := c.ws.Write(c.ctx, websocket.MessageBinary, ack)
			c.wmu.Unlock()
			if err != nil {
				return 0, err
			}
			c.lastAck = c.recvd
		}
	}
	return c.buf.Read(p)
}

func (c *iapConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), iapMaxFrame)
		f := make([]byte, 6+n)
		binary.BigEndian.PutUint16(f, iapTagData)
		binary.BigEndian.PutUint32(f[2:], uint32(n))
		copy(f[6:], p[:n])
		c.wmu.Lock()
		err := c.ws.Write(c.ctx, websocket.MessageBinary, f)
		c.wmu.Unlock()
		if err != nil {
			return total, err
		}
		p, total = p[n:], total+n
	}
	return total, nil
}

func (c *iapConn) Close() error {
	c.cancel()
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
func (c *iapConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *iapConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *iapConn) SetDeadline(time.Time) error      { return nil }
func (c *iapConn) SetReadDeadline(time.Time) error  { return nil }
func (c *iapConn) SetWriteDeadline(time.Time) error { return nil }

func (b *IAP) dialTunnel(ctx context.Context) (net.Conn, error) {
	tok, err := b.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: google token: %v", ErrUnreachable, err)
	}
	if b.dial != nil {
		return b.dial(ctx, tok)
	}
	q := url.Values{"project": {b.Project}, "zone": {b.Zone}, "instance": {b.Instance},
		"interface": {"nic0"}, "port": {"22"}, "newWebsocket": {"True"}}
	return dialIAPRelay(ctx, "wss://tunnel.cloudproxy.app/v4/connect?"+q.Encode(), tok)
}

// ---- SSH connection lifecycle -----------------------------------------------

func (b *IAP) hostKeyCallback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.HostKeys) > 0 {
		for _, hk := range b.HostKeys {
			pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hk))
			if err == nil && bytes.Equal(pk.Marshal(), key.Marshal()) {
				return nil
			}
		}
		return fmt.Errorf("login node host key %s is not one of the pinned keys", ssh.FingerprintSHA256(key))
	}
	if b.pinned == nil {
		b.pinned = key
		return nil
	}
	if !bytes.Equal(b.pinned.Marshal(), key.Marshal()) {
		return fmt.Errorf("login node host key changed (%s)", ssh.FingerprintSHA256(key))
	}
	return nil
}

// connect returns a live SSH client, building one if needed. Building is
// single-flight (connMu): parallel callers wait for one connection instead of
// each registering an OS Login key (live finding: a lost race left a key that
// the background cleanup never deleted before the CLI exited).
func (b *IAP) connect(ctx context.Context) (*ssh.Client, error) {
	b.connMu.Lock()
	defer b.connMu.Unlock()
	t0 := time.Now()
	defer func() { traceStep("connect total", t0) }()
	b.mu.Lock()
	if b.client != nil {
		c := b.client
		b.touch()
		b.mu.Unlock()
		return c, nil
	}
	b.mu.Unlock()

	// Reuse the stored key when it has life left; it is already on the node.
	if k, err := b.keys().Get(b.Email); err == nil && k != nil && time.Until(k.Expires) > 10*time.Minute {
		if signer, err := ssh.ParsePrivateKey(k.PrivateKey); err == nil {
			client, err := b.handshake(ctx, k.User, signer, 1)
			if err == nil {
				traceStep("reused key", t0)
				b.mu.Lock()
				b.client, b.user, b.keyLine = client, k.User, k.Line
				b.touch()
				b.mu.Unlock()
				return client, nil
			}
			if errors.Is(err, errHostKey) || errors.Is(err, errDenied) {
				return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
			}
			// key gone from the profile (revoked, expired early): fall through and import a new one
			traceStep("stored key refused", t0)
		}
	}

	pubRaw, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	pub, _ := ssh.NewPublicKey(pubRaw)
	t1 := time.Now()
	user, line, expires, err := b.importKey(ctx, pub)
	traceStep("os login import", t1)
	if err != nil {
		return nil, err
	}
	client, err := b.handshake(ctx, user, signer, 14)
	if err != nil {
		_ = b.deleteKeyLine(context.Background(), line)
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	// replace any older stored key: remove it from the profile, then store the new one
	if old, err := b.keys().Get(b.Email); err == nil && old != nil && old.Line != line {
		_ = b.deleteKeyLine(context.Background(), old.Line)
	}
	if pemBlock, err := ssh.MarshalPrivateKey(priv, "ursa-bifrost"); err == nil {
		_ = b.keys().Put(b.Email, &StoredKey{PrivateKey: pem.EncodeToMemory(pemBlock), Line: line, User: user, Expires: expires})
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.client, b.user, b.keyLine = client, user, line
	b.touch()
	return client, nil
}

var (
	errHostKey = errors.New("host key")
	errDenied  = errors.New("denied")
)

// handshake opens the tunnel and logs in, retrying while a newly imported key
// propagates to the node (measured: ~5 s typical, up to ~30 s).
func (b *IAP) handshake(ctx context.Context, user string, signer ssh.Signer, tries int) (*ssh.Client, error) {
	var client *ssh.Client
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			wait := []time.Duration{2 * time.Second, 2 * time.Second, 3 * time.Second}[min(attempt-1, 2)]
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		t2 := time.Now()
		conn, err := b.dialTunnel(ctx)
		traceStep("iap tunnel", t2)
		if err != nil {
			lastErr = err
			if strings.Contains(err.Error(), "denied") {
				return nil, fmt.Errorf("%w: %v", errDenied, err)
			}
			continue
		}
		t3 := time.Now()
		cc, chans, reqs, err := ssh.NewClientConn(conn, b.Instance+":22", &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: b.hostKeyCallback, Timeout: 30 * time.Second,
		})
		if err != nil {
			_ = conn.Close()
			lastErr = err
			traceStep("ssh failed: "+oneLine([]byte(err.Error())), t3)
			if strings.Contains(err.Error(), "host key") {
				return nil, fmt.Errorf("%w: %v", errHostKey, err)
			}
			continue
		}
		traceStep("ssh handshake", t3)
		client = ssh.NewClient(cc, chans, reqs)
		break
	}
	if client == nil {
		return nil, fmt.Errorf("ssh as %s: %v", user, lastErr)
	}
	return client, nil
}

// touch resets the idle timer. Caller holds b.mu.
func (b *IAP) touch() {
	b.lastUsed = time.Now()
	idle := b.Idle
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(idle, func() { _ = b.Close() })
}

// Close drops the SSH connection. The OS Login key stays (it is reused by the
// next connection and expires on its own); Revoke removes it.
func (b *IAP) Close() error {
	b.mu.Lock()
	c := b.client
	b.client, b.keyLine = nil, ""
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	return nil
}

// Revoke closes the connection and deletes the user's bifrost key from their
// OS Login profile and from the key store (sign-out, user removed).
func (b *IAP) Revoke(ctx context.Context) error {
	_ = b.Close()
	k, err := b.keys().Get(b.Email)
	if err != nil || k == nil {
		return err
	}
	if err := b.deleteKeyLine(ctx, k.Line); err != nil {
		return err
	}
	return b.keys().Delete(b.Email)
}

func (b *IAP) keys() KeyStore {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Keys == nil {
		b.Keys = NewMemKeyStore()
	}
	return b.Keys
}

func (b *IAP) drop(c *ssh.Client) {
	b.mu.Lock()
	if b.client == c {
		b.mu.Unlock()
		_ = b.Close()
		return
	}
	b.mu.Unlock()
}

// Run executes c on the login node as the user.
func (b *IAP) Run(ctx context.Context, c Command) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout(c))
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ { // one retry on a dead pooled connection
		client, err := b.connect(ctx)
		if err != nil {
			return nil, err
		}
		sess, err := client.NewSession()
		if err != nil {
			lastErr = err
			b.drop(client)
			continue
		}
		var stdout, stderr bytes.Buffer
		sess.Stdout, sess.Stderr = &stdout, &stderr
		if c.merge {
			// x/crypto/ssh copies stdout and stderr in two goroutines: a shared
			// writer must be locked (race found by `go test -race`)
			lw := &lockedWriter{w: &stdout}
			sess.Stdout, sess.Stderr = lw, lw
		}
		if c.stdin != nil {
			sess.Stdin = bytes.NewReader(c.stdin)
		}
		t4 := time.Now()
		defer traceStep("run "+c.argv[0], t4)
		done := make(chan error, 1)
		go func() { done <- sess.Run(RemoteLine(c)) }()
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
			return nil, fmt.Errorf("%s timed out after %s", c.argv[0], Timeout(c))
		case err = <-done:
		}
		_ = sess.Close()
		if err == nil {
			return stdout.Bytes(), nil
		}
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			code := ee.ExitStatus()
			if okExit(c, code) {
				return stdout.Bytes(), nil
			}
			msg := stderr.String()
			if c.merge {
				msg = stdout.String()
			}
			return stdout.Bytes(), fmt.Errorf("%s exited %d: %s", c.argv[0], code, lastLine(msg))
		}
		// transport failure before an exit status: the connection is gone
		lastErr = err
		b.drop(client)
		if c.write {
			// never re-run a state-changing command whose outcome is unknown
			return nil, fmt.Errorf("%w: connection lost while running %s; check its state before retrying", ErrUnreachable, c.argv[0])
		}
	}
	return nil, fmt.Errorf("%w: %v", ErrUnreachable, lastErr)
}

// traceStep prints a timing line to stderr when BIFROST_TRACE is set.
func traceStep(what string, since time.Time) {
	if os.Getenv("BIFROST_TRACE") != "" {
		fmt.Fprintf(os.Stderr, "trace: %-16s %v\n", what, time.Since(since).Round(time.Millisecond))
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func oneLine(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
