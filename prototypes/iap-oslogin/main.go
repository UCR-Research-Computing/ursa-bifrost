// Prototype: reach the Ursa Major login node AS A USER, from plain Go, with only
// that user's Google OAuth access token. No gcloud, no ssh binary, no service
// account. This is what a Cloud Run MCP server would do per caller.
//
//  1. OS Login API: register a fresh ed25519 public key on the caller's
//     profile with a short expiry; read back their POSIX username.
//  2. IAP TCP forwarding: open a WebSocket to tunnel.cloudproxy.app with the
//     caller's bearer token (relay subprotocol: CONNECT_SUCCESS_SID, DATA, ACK).
//  3. SSH over that byte stream as the POSIX user with the ephemeral key.
//  4. Run one allow-listed command; delete the key.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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

const (
	project  = "ucr-ursa-major-hpc-cluster"
	zone     = "us-central1-a"
	instance = "ucrslurmcl-slurm-login-001"

	tagConnectSID = 0x0001
	tagData       = 0x0004
	tagAck        = 0x0007
	maxFrame      = 16384
)

// ---- 1. OS Login -------------------------------------------------------------

type loginProfile struct {
	PosixAccounts []struct {
		Username string `json:"username"`
		Primary  bool   `json:"primary"`
	} `json:"posixAccounts"`
}

func osLogin(ctx context.Context, token, email string, pub ssh.PublicKey) (user, fingerprint string, err error) {
	body, _ := json.Marshal(map[string]any{
		"key":                strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " bifrost-ephemeral",
		"expirationTimeUsec": fmt.Sprint(time.Now().Add(5 * time.Minute).UnixMicro()),
	})
	u := fmt.Sprintf("https://oslogin.googleapis.com/v1/users/%s:importSshPublicKey?projectId=%s", url.PathEscape(email), project)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("importSshPublicKey: %s: %s", resp.Status, b)
	}
	var out struct {
		LoginProfile loginProfile `json:"loginProfile"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", "", err
	}
	for _, a := range out.LoginProfile.PosixAccounts {
		if a.Primary || user == "" {
			user = a.Username
		}
	}
	if user == "" {
		return "", "", errors.New("no POSIX account on the OS Login profile")
	}
	return user, ssh.FingerprintSHA256(pub), nil
}

func deleteKey(token, email string, pub ssh.PublicKey) error {
	// OS Login names a key by sha256 of the exact authorized_keys line that was
	// imported (comment included), not of the key blob. Found by testing: the
	// blob hash returns 200 on a non-existent key and deletes nothing.
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " bifrost-ephemeral"
	fp := fmt.Sprintf("%x", sha256.Sum256([]byte(line)))
	u := fmt.Sprintf("https://oslogin.googleapis.com/v1/users/%s/sshPublicKeys/%s", url.PathEscape(email), fp)
	req, _ := http.NewRequest("DELETE", u, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("delete key: %s", resp.Status)
	}
	return nil
}

// ---- 2. IAP tunnel as a net.Conn -----------------------------------------------

type iapConn struct {
	ws      *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	buf     bytes.Buffer
	readMu  sync.Mutex
	recvd   uint64
	lastAck uint64
}

func dialIAP(ctx context.Context, token string) (*iapConn, error) {
	q := url.Values{"project": {project}, "zone": {zone}, "instance": {instance}, "interface": {"nic0"},
		"port": {"22"}, "newWebsocket": {"True"}}
	u := "wss://tunnel.cloudproxy.app/v4/connect?" + q.Encode()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Origin", "bot:iap-tunneler")
	h.Set("User-Agent", "ursa-bifrost-iap-prototype")
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h, Subprotocols: []string{"relay.tunnel.cloudproxy.app"}})
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("IAP dial: %v (%s %s)", err, resp.Status, b)
		}
		return nil, fmt.Errorf("IAP dial: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	cctx, cancel := context.WithCancel(context.Background())
	c := &iapConn{ws: ws, ctx: cctx, cancel: cancel}
	// first frame must be CONNECT_SUCCESS_SID
	_, msg, err := ws.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("IAP: no connect frame: %w", err)
	}
	if len(msg) < 2 || binary.BigEndian.Uint16(msg) != tagConnectSID {
		return nil, fmt.Errorf("IAP: unexpected first frame tag %x", msg[:2])
	}
	return c, nil
}

func (c *iapConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for c.buf.Len() == 0 {
		_, msg, err := c.ws.Read(c.ctx)
		if err != nil {
			return 0, io.EOF
		}
		if len(msg) < 2 {
			continue
		}
		switch binary.BigEndian.Uint16(msg) {
		case tagData:
			n := binary.BigEndian.Uint32(msg[2:6])
			c.buf.Write(msg[6 : 6+n])
			c.recvd += uint64(n)
			if c.recvd-c.lastAck > 2*maxFrame { // acknowledge what we have received
				ack := make([]byte, 10)
				binary.BigEndian.PutUint16(ack, tagAck)
				binary.BigEndian.PutUint64(ack[2:], c.recvd)
				c.mu.Lock()
				err := c.ws.Write(c.ctx, websocket.MessageBinary, ack)
				c.mu.Unlock()
				if err != nil {
					return 0, err
				}
				c.lastAck = c.recvd
			}
		case tagAck: // server acknowledging our bytes; nothing to do here
		}
	}
	return c.buf.Read(p)
}

func (c *iapConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), maxFrame)
		f := make([]byte, 6+n)
		binary.BigEndian.PutUint16(f, tagData)
		binary.BigEndian.PutUint32(f[2:], uint32(n))
		copy(f[6:], p[:n])
		c.mu.Lock()
		err := c.ws.Write(c.ctx, websocket.MessageBinary, f)
		c.mu.Unlock()
		if err != nil {
			return total, err
		}
		p, total = p[n:], total+n
	}
	return total, nil
}

func (c *iapConn) Close() error                     { c.cancel(); return c.ws.Close(websocket.StatusNormalClosure, "") }
func (c *iapConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *iapConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *iapConn) SetDeadline(time.Time) error      { return nil }
func (c *iapConn) SetReadDeadline(time.Time) error  { return nil }
func (c *iapConn) SetWriteDeadline(time.Time) error { return nil }

// ---- 3+4. SSH and one command ---------------------------------------------------

func main() {
	token := strings.TrimSpace(os.Getenv("ACCESS_TOKEN"))
	email := os.Getenv("EMAIL")
	if token == "" || email == "" {
		fmt.Fprintln(os.Stderr, "set ACCESS_TOKEN and EMAIL")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t0 := time.Now()

	pubRaw, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	pub, _ := ssh.NewPublicKey(pubRaw)
	user, fp, err := osLogin(ctx, token, email, pub)
	if err != nil {
		fmt.Println("FAIL oslogin:", err)
		os.Exit(1)
	}
	defer func() {
		if err := deleteKey(token, email, pub); err != nil {
			fmt.Println("warn:", err)
		} else {
			fmt.Println("ephemeral key deleted from the OS Login profile")
		}
	}()
	fmt.Printf("1. OS Login: key %s registered for posix user %q (%v)\n", fp[:20], user, time.Since(t0).Round(time.Millisecond))

	t1 := time.Now()
	conn, err := dialIAP(ctx, token)
	if err != nil {
		fmt.Println("FAIL iap:", err)
		os.Exit(1)
	}
	fmt.Printf("2. IAP tunnel to %s:22 open (%v)\n", instance, time.Since(t1).Round(time.Millisecond))

	t2 := time.Now()
	var sc *ssh.Client
	for attempt := 0; attempt < 6; attempt++ { // OS Login key propagation can lag a few seconds
		if attempt > 0 {
			conn, err = dialIAP(ctx, token)
			if err != nil {
				fmt.Println("FAIL iap redial:", err)
				os.Exit(1)
			}
		}
		cc, chans, reqs, err2 := ssh.NewClientConn(conn, "login-001:22", &ssh.ClientConfig{
			User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), // prototype only: production pins the host key
			Timeout:         20 * time.Second,
		})
		if err2 == nil {
			sc = ssh.NewClient(cc, chans, reqs)
			break
		}
		err = err2
		_ = conn.Close()
		time.Sleep(2 * time.Second)
	}
	if sc == nil {
		fmt.Println("FAIL ssh:", err)
		os.Exit(1)
	}
	defer sc.Close()
	fmt.Printf("3. SSH as %s established (%v)\n", user, time.Since(t2).Round(time.Millisecond))

	sess, _ := sc.NewSession()
	out, err := sess.CombinedOutput("id -un; squeue --me --noheader | wc -l; sinfo --summarize --noheader | head -3")
	fmt.Printf("4. command output (err=%v):\n%s", err, out)
	fmt.Printf("total %v\n", time.Since(t0).Round(time.Millisecond))
}
