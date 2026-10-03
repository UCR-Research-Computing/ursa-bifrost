package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// fakeGoogle is a fake OS Login API plus a fake IAP relay in front of a real
// in-process SSH server. The relay speaks the same frames as
// tunnel.cloudproxy.app; the SSH server accepts only keys currently on the
// fake OS Login profile and runs nothing: it echoes the command line.
type fakeGoogle struct {
	t        *testing.T
	mu       sync.Mutex
	token    string            // the only accepted bearer token
	email    string            // the only user with a POSIX account
	posix    string            // their POSIX name
	keys     map[string]string // key id -> authorized_keys line
	imports  int
	deletes  int
	commands []string
	hostKey  ssh.Signer
	osl      *httptest.Server
	relay    *httptest.Server
	denyIAP  bool
	// renameKeys simulates Google changing how keys are named (key id != sha256(line))
	renameKeys bool
	// maxSessions refuses channels beyond this many open at once (OpenSSH
	// MaxSessions; 0 = unlimited). slow makes each command take this long.
	maxSessions int
	open        int
	peak        int
	slow        time.Duration
	// acceptAfter: the SSH server refuses a key until this long after its
	// import (the login node's key cache; live: 5-30 s, sometimes over a minute).
	acceptAfter time.Duration
	imported    map[string]time.Time // key id -> import time
	expiry      map[string]string    // key id -> expirationTimeUsec
	patches     int
	failPatch   bool // PATCH answers 500 (extension unavailable)
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	_, hpriv, _ := ed25519.GenerateKey(rand.Reader)
	hs, _ := ssh.NewSignerFromKey(hpriv)
	f := &fakeGoogle{t: t, token: "tok-alice", email: "alice@ucr.edu", posix: "alice_ucr_edu", keys: map[string]string{}, hostKey: hs,
		imported: map[string]time.Time{}, expiry: map[string]string{}}
	f.osl = httptest.NewServer(http.HandlerFunc(f.serveOSLogin))
	f.relay = httptest.NewServer(http.HandlerFunc(f.serveRelay))
	t.Cleanup(func() { f.osl.Close(); f.relay.Close() })
	return f
}

func (f *fakeGoogle) authOK(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+f.token
}

func (f *fakeGoogle) serveOSLogin(w http.ResponseWriter, r *http.Request) {
	if !f.authOK(r) {
		http.Error(w, `{"error":{"code":401}}`, 401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, ":importSshPublicKey"):
		if !strings.Contains(r.URL.Path, "/v1/users/"+f.email+":") {
			http.Error(w, `{"error":{"code":403,"message":"not your profile"}}`, 403)
			return
		}
		var body struct{ Key string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := keyID(body.Key)
		if f.renameKeys {
			id = "other-" + id[:16]
		}
		f.keys[id] = body.Key
		f.imported[id] = time.Now()
		f.imports++
		prof := map[string]any{"posixAccounts": []map[string]any{{"username": f.posix, "primary": true}}, "sshPublicKeys": map[string]any{}}
		for id, k := range f.keys {
			prof["sshPublicKeys"].(map[string]any)[id] = map[string]any{"key": k, "fingerprint": id}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"loginProfile": prof})
	case r.Method == "GET" && strings.Contains(r.URL.Path, "/sshPublicKeys/"):
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if _, ok := f.keys[id]; !ok {
			http.Error(w, `{"error":{"code":404}}`, 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"key": f.keys[id], "fingerprint": id})
	case r.Method == "PATCH":
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.patches++
		if f.failPatch {
			http.Error(w, `{"error":{"code":500}}`, 500)
			return
		}
		if _, ok := f.keys[id]; !ok || r.URL.Query().Get("updateMask") != "expirationTimeUsec" {
			http.Error(w, `{"error":{"code":404}}`, 404)
			return
		}
		var body struct {
			Key string
			Exp string `json:"expirationTimeUsec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Key != f.keys[id] {
			http.Error(w, `{"error":{"code":400,"message":"key mismatch"}}`, 400)
			return
		}
		f.expiry[id] = body.Exp
		_ = json.NewEncoder(w).Encode(map[string]any{"key": body.Key, "expirationTimeUsec": body.Exp, "fingerprint": id})
	case r.Method == "DELETE":
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		delete(f.keys, id)
		f.deletes++
		_, _ = w.Write([]byte("{}"))
	default:
		http.Error(w, "nope", 404)
	}
}

func (f *fakeGoogle) keyAllowed(key ssh.PublicKey) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, line := range f.keys {
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err == nil && string(pk.Marshal()) == string(key.Marshal()) {
			return time.Since(f.imported[id]) >= f.acceptAfter
		}
	}
	return false
}

func (f *fakeGoogle) serveRelay(w http.ResponseWriter, r *http.Request) {
	if f.denyIAP || !f.authOK(r) || r.Header.Get("Origin") != "bot:iap-tunneler" {
		http.Error(w, "forbidden", 403)
		return
	}
	q := r.URL.Query()
	if q.Get("port") != "22" || q.Get("instance") == "" {
		http.Error(w, "bad target", 400)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"relay.tunnel.cloudproxy.app"}, InsecureSkipVerify: true}) // IAP uses the non-URL Origin "bot:iap-tunneler"
	if err != nil {
		return
	}
	ctx := r.Context()
	sid := []byte{0, 1, 0, 0, 0, 3, 's', 'i', 'd'}
	_ = ws.Write(ctx, websocket.MessageBinary, sid)
	// pipe frames <-> an in-process SSH server
	srvSide, cliSide := net.Pipe()
	go f.serveSSH(srvSide)
	go func() { // relay -> ssh
		for {
			_, msg, err := ws.Read(ctx)
			if err != nil {
				_ = cliSide.Close()
				return
			}
			if binary.BigEndian.Uint16(msg) == iapTagData {
				n := binary.BigEndian.Uint32(msg[2:6])
				if _, err := cliSide.Write(msg[6 : 6+n]); err != nil {
					return
				}
			}
		}
	}()
	buf := make([]byte, 8192) // ssh -> relay, deliberately small frames
	for {
		n, err := cliSide.Read(buf)
		if err != nil {
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return
		}
		fr := make([]byte, 6+n)
		binary.BigEndian.PutUint16(fr, iapTagData)
		binary.BigEndian.PutUint32(fr[2:], uint32(n))
		copy(fr[6:], buf[:n])
		if ws.Write(ctx, websocket.MessageBinary, fr) != nil {
			return
		}
	}
}

func (f *fakeGoogle) serveSSH(c net.Conn) {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != f.posix || !f.keyAllowed(key) {
			return nil, fmt.Errorf("denied")
		}
		return nil, nil
	}}
	cfg.AddHostKey(f.hostKey)
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		f.mu.Lock()
		if f.maxSessions > 0 && f.open >= f.maxSessions {
			f.mu.Unlock()
			_ = nc.Reject(ssh.ConnectionFailed, "open failed")
			continue
		}
		f.open++
		if f.open > f.peak {
			f.peak = f.open
		}
		f.mu.Unlock()
		ch, creqs, err := nc.Accept()
		if err != nil {
			f.mu.Lock()
			f.open--
			f.mu.Unlock()
			continue
		}
		go func() {
			defer func() {
				f.mu.Lock()
				f.open--
				f.mu.Unlock()
			}()
			defer ch.Close()
			for req := range creqs {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				cmd := string(req.Payload[4:])
				f.mu.Lock()
				f.commands = append(f.commands, cmd)
				f.mu.Unlock()
				_ = req.Reply(true, nil)
				in, _ := io.ReadAll(io.LimitReader(ch, 1<<20))
				if f.slow > 0 {
					time.Sleep(f.slow)
				}
				code := 0
				out := "ran: " + cmd
				switch {
				case cmd == "id -un":
					out = f.posix
				case strings.HasPrefix(cmd, "squeue"):
					code = 2
					_, _ = ch.Stderr().Write([]byte("squeue: error: Invalid user\n"))
					out = ""
				case strings.HasPrefix(cmd, "sbatch"):
					out = fmt.Sprintf("stdin=%d", len(in))
				case strings.HasPrefix(cmd, "scancel"):
					// drop the connection with no exit status: outcome unknown
					_ = c.Close()
					return
				}
				_, _ = ch.Write([]byte(out))
				_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, uint32(code)))
				return
			}
		}()
	}
}

func (f *fakeGoogle) backend() *IAP {
	return &IAP{
		Project: "p", Zone: "z", Instance: "login-001", Email: f.email,
		Token:       func(context.Context) (string, error) { return f.token, nil },
		osloginBase: f.osl.URL,
		dial: func(ctx context.Context, tok string) (net.Conn, error) {
			return dialIAPRelay(ctx, "ws"+strings.TrimPrefix(f.relay.URL, "http")+"/v4/connect?port=22&instance=login-001", tok)
		},
	}
}

func TestIAPRunsAsTheUser(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend()
	ctx := context.Background()
	out, err := b.Run(ctx, Whoami())
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "alice_ucr_edu" || b.PosixUser() != "alice_ucr_edu" {
		t.Fatalf("whoami %q, posix %q", out, b.PosixUser())
	}
	// second call reuses the connection: no new key
	if _, err := b.Run(ctx, Sinfo()); err != nil {
		t.Fatal(err)
	}
	if f.imports != 1 {
		t.Errorf("imports = %d, want 1 (connection reuse)", f.imports)
	}
	// non-zero exit is reported with stderr, like the ssh backend
	c, _ := SqueueUser("alice_ucr_edu")
	if _, err := b.Run(ctx, c); err == nil || !strings.Contains(err.Error(), "Invalid user") {
		t.Errorf("exit error: %v", err)
	}
	// stdin reaches the remote command
	s, err := SbatchTestOnly(SubmitOpts{Partition: "standard", Nodes: 1, TimeMin: 10, Comment: "bifrost:0123456789ab"}, []byte("#!/bin/bash\necho hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	out, err = b.Run(ctx, s)
	if err != nil || string(out) != "stdin=20" {
		t.Errorf("stdin: %q %v (cmd %q)", out, err, s.String())
	}
	// the exact allow-listed command line arrives, nothing else
	for _, cmd := range f.commands {
		if !strings.HasPrefix(cmd, "id ") && !strings.HasPrefix(cmd, "sinfo ") && !strings.HasPrefix(cmd, "squeue ") && !strings.HasPrefix(cmd, "sbatch ") {
			t.Errorf("unexpected remote command %q", cmd)
		}
	}
	// Close keeps the key for reuse; Revoke deletes it
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 {
		t.Errorf("after Close: %d keys, want 1 kept for reuse", len(f.keys))
	}
	if err := b.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 0 || f.deletes != 1 {
		t.Errorf("after Revoke: %d keys left, %d deletes", len(f.keys), f.deletes)
	}
}

func TestIAPReusesStoredKeyAcrossProcesses(t *testing.T) {
	f := newFakeGoogle(t)
	store := FileKeyStore{Dir: t.TempDir()}
	ctx := context.Background()
	for i := 0; i < 3; i++ { // three "processes", one store on disk
		b := f.backend()
		b.Keys = store
		if _, err := b.Run(ctx, Whoami()); err != nil {
			t.Fatal(err)
		}
		_ = b.Close()
	}
	if f.imports != 1 {
		t.Errorf("imports = %d over 3 processes, want 1 (key reuse)", f.imports)
	}
	// the stored file is private
	ents, _ := os.ReadDir(store.Dir)
	if len(ents) != 1 {
		t.Fatalf("store files: %d", len(ents))
	}
	st, _ := os.Stat(filepath.Join(store.Dir, ents[0].Name()))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode().Perm())
	}
	// a key removed from the profile behind our back is replaced, and the store updated
	f.mu.Lock()
	f.keys = map[string]string{}
	f.mu.Unlock()
	b := f.backend()
	b.Keys = store
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	if f.imports != 2 {
		t.Errorf("imports = %d after revocation, want 2", f.imports)
	}
	// Revoke removes it everywhere
	if err := b.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	if k, _ := store.Get(f.email); k != nil || len(f.keys) != 0 {
		t.Errorf("after Revoke: store=%v profile keys=%d", k != nil, len(f.keys))
	}
}

// TestIAPExpiringKeyIsReplaced: a key close to expiry that cannot be extended
// (OS Login PATCH failing) is not used for a new connection: a new key is
// imported and the old one removed.
func TestIAPExpiringKeyIsReplaced(t *testing.T) {
	f := newFakeGoogle(t)
	f.failPatch = true
	store := NewMemKeyStore()
	b := f.backend()
	b.Keys = store
	b.KeyTTL = 5 * time.Minute // under the 10-minute reuse margin
	ctx := context.Background()
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	if f.imports != 2 {
		t.Errorf("imports %d, want 2 (near-expiry key replaced)", f.imports)
	}
	if len(f.keys) != 1 {
		t.Errorf("old key not removed when replaced: %d keys", len(f.keys))
	}
	_ = b.Revoke(ctx)
}

func TestIAPUsersAreSeparate(t *testing.T) {
	f := newFakeGoogle(t)
	bob := f.backend()
	bob.Email = "bob@ucr.edu" // has no OS Login profile in the fake
	bob.Token = func(context.Context) (string, error) { return "tok-bob", nil }
	if _, err := bob.Run(context.Background(), Whoami()); err == nil {
		t.Fatal("a user without access got in")
	}
	// alice's token cannot import a key onto bob's profile
	evil := f.backend()
	evil.Email = "bob@ucr.edu"
	if _, err := evil.Run(context.Background(), Whoami()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("cross-user import: %v", err)
	}
	if f.imports != 0 {
		t.Errorf("keys imported: %d", f.imports)
	}
}

func TestIAPDeniedTunnel(t *testing.T) {
	f := newFakeGoogle(t)
	f.denyIAP = true
	b := f.backend()
	_, err := b.Run(context.Background(), Whoami())
	if err == nil || !strings.Contains(err.Error(), "tunnelResourceAccessor") {
		t.Fatalf("denied tunnel: %v", err)
	}
	if len(f.keys) != 0 {
		t.Errorf("key left behind after a failed connect: %d", len(f.keys))
	}
}

func TestIAPHostKeyPinning(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os, _ := ssh.NewSignerFromKey(other)
	b.HostKeys = []string{string(ssh.MarshalAuthorizedKey(os.PublicKey()))}
	_, err := b.Run(context.Background(), Whoami())
	if err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("wrong host key accepted: %v", err)
	}
	if len(f.keys) != 0 {
		t.Errorf("key left behind after host key failure")
	}
	b.HostKeys = []string{string(ssh.MarshalAuthorizedKey(f.hostKey.PublicKey()))}
	if _, err := b.Run(context.Background(), Whoami()); err != nil {
		t.Fatalf("pinned key refused: %v", err)
	}
	_ = b.Revoke(context.Background())
}

func TestIAPIdleCloseReconnectsWithSameKey(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend()
	b.Idle = 200 * time.Millisecond
	if _, err := b.Run(context.Background(), Whoami()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	b.mu.Lock()
	open := b.client != nil
	b.mu.Unlock()
	if open {
		t.Error("idle connection not closed")
	}
	if _, err := b.Run(context.Background(), Whoami()); err != nil {
		t.Fatal(err)
	}
	if f.imports != 1 {
		t.Errorf("imports %d, want 1 (reconnect reuses the key)", f.imports)
	}
	_ = b.Revoke(context.Background())
}

func TestKeyIDIsHashOfImportedLine(t *testing.T) {
	// regression for the live finding: the id is sha256(line), not sha256(blob)
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample ursa-bifrost"
	if got := keyID(line); len(got) != 64 || got == keyID("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample") {
		t.Errorf("keyID must cover the comment: %s", got)
	}
}

func TestIAPParallelCallsMakeOneKey(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Run(context.Background(), Sinfo()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.imports != 1 {
		t.Errorf("%d OS Login keys registered for 8 parallel calls, want 1", f.imports)
	}
	_ = b.Close()
	_ = b.Revoke(context.Background())
	if len(f.keys) != 0 {
		t.Errorf("%d keys left after Revoke", len(f.keys))
	}
}

func TestIAPNeverRetriesAWriteWithUnknownOutcome(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend()
	c, _ := Scancel("123")
	_, err := b.Run(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "check its state") {
		t.Fatalf("lost write: %v", err)
	}
	n := 0
	for _, cmd := range f.commands {
		if strings.HasPrefix(cmd, "scancel") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("scancel sent %d times, want exactly 1", n)
	}
	_ = b.Revoke(context.Background())
}

func TestIAPRefusesUnknownKeyIDScheme(t *testing.T) {
	// if OS Login stops naming keys sha256(line), bifrost could no longer delete
	// them; it must refuse and not leave a key it cannot clean up
	f := newFakeGoogle(t)
	f.renameKeys = true
	b := f.backend()
	_, err := b.Run(context.Background(), Whoami())
	if err == nil || !strings.Contains(err.Error(), "expected id") {
		t.Fatalf("unknown key id scheme accepted: %v", err)
	}
	if len(f.commands) != 0 {
		t.Errorf("connected anyway: %v", f.commands)
	}
}

// TestIAPBurstRespectsSessionLimit: 30 concurrent calls over one connection
// to a server that allows 10 sessions all succeed, never more than
// MaxSessionsPerConn run at once, and one key is registered (live finding:
// v0.7.0 failed 9 of 30 burst calls with "open failed", and the refused
// channel tore down the connection under the others).
func TestIAPBurstRespectsSessionLimit(t *testing.T) {
	f := newFakeGoogle(t)
	f.maxSessions, f.slow = 10, 50*time.Millisecond
	b := f.backend()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Run(context.Background(), Sinfo()); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("%d of 30 calls failed, first: %v", len(errs), errs[0])
	}
	if f.peak > 8 || f.peak < 2 {
		t.Errorf("peak concurrent sessions %d, want 2..8", f.peak)
	}
	if f.imports != 1 {
		t.Errorf("%d keys registered, want 1", f.imports)
	}
	_ = b.Revoke(context.Background())
}

// TestIAPRefusedChannelKeepsConnection: a refused session (server at its
// limit, e.g. because of the person's own interactive ssh) is retried on the
// same connection instead of tearing it down.
func TestIAPRefusedChannelKeepsConnection(t *testing.T) {
	f := newFakeGoogle(t)
	b := f.backend()
	if _, err := b.Run(context.Background(), Sinfo()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	first := b.client
	b.mu.Unlock()
	f.mu.Lock()
	f.maxSessions, f.open = 1, 1 // full: the next channel is refused
	f.mu.Unlock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.mu.Lock()
		f.open = 0 // a slot frees up
		f.mu.Unlock()
	}()
	if _, err := b.Run(context.Background(), Sinfo()); err != nil {
		t.Fatalf("refused channel was not retried: %v", err)
	}
	b.mu.Lock()
	same := b.client == first
	b.mu.Unlock()
	if !same {
		t.Error("a refused channel tore down the shared connection")
	}
	_ = b.Revoke(context.Background())
}

// ---- v0.9.5: key renewal (outage 2026-10-03: an 8 h key lapsed overnight and
// every replacement was deleted before the login node accepted it) ----------

func storedKey(t *testing.T, s KeyStore, email string) *StoredKey {
	t.Helper()
	k, err := s.Get(email)
	if err != nil || k == nil {
		t.Fatalf("no stored key: %v", err)
	}
	return k
}

// A stored key close to expiry is extended in place (same key, no propagation
// wait), not replaced by a new import.
func TestIAPNearExpiryKeyIsExtendedNotReplaced(t *testing.T) {
	f := newFakeGoogle(t)
	store := NewMemKeyStore()
	b := f.backend()
	b.Keys = store
	ctx := context.Background()
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	k := storedKey(t, store, f.email)
	k.Expires = time.Now().Add(30 * time.Minute) // inside RenewWindow
	_ = store.Put(f.email, k)
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	if f.imports != 1 || f.patches != 1 {
		t.Errorf("imports %d patches %d, want 1 and 1 (extended, not replaced)", f.imports, f.patches)
	}
	if left := time.Until(storedKey(t, store, f.email).Expires); left < 7*time.Hour {
		t.Errorf("stored expiry not moved out: %v left", left)
	}
	if f.expiry[keyID(k.Line)] == "" {
		t.Error("OS Login expiry not updated")
	}
	_ = b.Revoke(ctx)
}

// A key the login node is still slow to accept is kept (stored before the
// first login, never deleted for being slow) and the next call waits for that
// same key instead of importing another and restarting the wait.
func TestIAPSlowNewKeyIsKeptAndReused(t *testing.T) {
	f := newFakeGoogle(t)
	f.acceptAfter = 2500 * time.Millisecond
	store := NewMemKeyStore()
	b := f.backend()
	b.Keys = store
	short, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := b.Run(short, Whoami())
	if err == nil {
		t.Fatal("first call should fail while the key propagates")
	}
	if len(f.keys) != 1 || f.deletes != 0 {
		t.Fatalf("slow key removed: %d keys on profile, %d deletes", len(f.keys), f.deletes)
	}
	if _, err := b.Run(context.Background(), Whoami()); err != nil {
		t.Fatalf("second call did not wait for the same key: %v", err)
	}
	if f.imports != 1 {
		t.Errorf("imports %d, want 1 (no replacement key)", f.imports)
	}
	_ = b.Revoke(context.Background())
}

// The error a caller sees while a new key propagates says so in plain words.
func TestIAPSlowKeyErrorExplains(t *testing.T) {
	f := newFakeGoogle(t)
	f.acceptAfter = time.Hour
	b := f.backend()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := b.Run(ctx, Whoami())
	if err == nil || !strings.Contains(err.Error(), "not accepted the new login key yet") {
		t.Fatalf("error: %v", err)
	}
}

// Parallel calls arriving while a new key propagates all succeed on one key.
func TestIAPParallelCallsDuringPropagation(t *testing.T) {
	f := newFakeGoogle(t)
	f.acceptAfter = 1500 * time.Millisecond
	b := f.backend()
	b.Keys = NewMemKeyStore()
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Run(context.Background(), Sinfo()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if f.imports != 1 {
		t.Errorf("imports %d, want 1", f.imports)
	}
	_ = b.Revoke(context.Background())
}

// A connection kept busy for hours renews its key in the background before
// it lapses, so the reconnect after the next idle close needs no new key.
func TestIAPLiveConnectionRenewsKeyInBackground(t *testing.T) {
	f := newFakeGoogle(t)
	store := NewMemKeyStore()
	b := f.backend()
	b.Keys = store
	ctx := context.Background()
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.keyExp = time.Now().Add(time.Hour) // inside RenewWindow
	b.mu.Unlock()
	if _, err := b.Run(ctx, Sinfo()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		b.mu.Lock()
		left, busy := time.Until(b.keyExp), b.renewing
		b.mu.Unlock()
		if left > 7*time.Hour && !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not renewed: %v left, %d patches", left, f.patches)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.patches != 1 || f.imports != 1 {
		t.Errorf("patches %d imports %d, want 1 and 1", f.patches, f.imports)
	}
	if time.Until(storedKey(t, store, f.email).Expires) < 7*time.Hour {
		t.Error("stored key expiry not updated by the background renewal")
	}
	_ = b.Revoke(ctx)
}

// A stored key that has vanished from the profile (PATCH 404) is replaced.
func TestIAPVanishedKeyOnExtendIsReplaced(t *testing.T) {
	f := newFakeGoogle(t)
	store := NewMemKeyStore()
	b := f.backend()
	b.Keys = store
	ctx := context.Background()
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	k := storedKey(t, store, f.email)
	k.Expires = time.Now().Add(30 * time.Minute)
	_ = store.Put(f.email, k)
	f.mu.Lock()
	f.keys = map[string]string{}
	f.mu.Unlock()
	if _, err := b.Run(ctx, Whoami()); err != nil {
		t.Fatal(err)
	}
	if f.imports != 2 {
		t.Errorf("imports %d, want 2", f.imports)
	}
	_ = b.Revoke(ctx)
}
