package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"export API_TOKEN=abc123secretvalue":                                   "abc123secretvalue",
		"SLURM_JWT=eyJhbGciOiJIUzI1NiJ9.e30.sig":                               "eyJhbGciOiJIUzI1NiJ9",
		"key AIzaSyA1234567890abcdefghijklmnopqrstu":                           "AIzaSyA1234567890",
		"Authorization: Bearer abcdefghijklmnop123456":                         "abcdefghijklmnop123456",
		"aws AKIAABCDEFGHIJKLMNOP":                                             "AKIAABCDEFGHIJKLMNOP",
		"DB_PASSWORD='hunter2 x'":                                              "hunter2",
		"tok ghp_abcdefghijklmnopqrstuvwxyz0123456789":                         "ghp_abcdefghijklmnop",
		"https://user:pa55w0rd@example.com/data.tar.gz":                        "pa55w0rd",
		"sk-proj-abcdefghijklmnopqrstuvwxyz":                                   "abcdefghijklmnopqrstuvwxyz",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----": "MIIE",
		`{"private_key": "-----BEGIN"}`:                                        "-----BEGIN",
		"OPENAI_API_KEY: sk-abc":                                               "sk-abc",
	}
	for in, secret := range cases {
		out := Redact(in)
		if strings.Contains(out, secret) {
			t.Errorf("secret survived:\n in  %q\n out %q", in, out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("no marker: %q", out)
		}
	}
	// ordinary text is untouched
	for _, s := range []string{"ModuleNotFoundError: No module named 'pandas'", "token count 512", "KEY_COLUMN missing"} {
		if Redact(s) != s {
			t.Errorf("changed harmless text: %q -> %q", s, Redact(s))
		}
	}
}

func TestWrapKeepsTailAndMarks(t *testing.T) {
	u := Wrap(strings.Repeat("a", 100)+"END", 10)
	if !u.Truncated || !strings.HasSuffix(u.Text, "END") || len(u.Text) != 10 {
		t.Fatalf("%+v", u)
	}
	if u.Note != UntrustedNote {
		t.Error("missing untrusted note")
	}
	// multi-byte characters are not split
	u = Wrap("ééééé", 3)
	if !strings.HasPrefix(u.Text, "é") && u.Text != "é" {
		t.Errorf("split utf-8: %q", u.Text)
	}
}

func TestRequire(t *testing.T) {
	if err := Require([]string{"R1"}, "R2", "x"); err == nil {
		t.Fatal("R1 granted R2")
	}
	if err := Require([]string{"R1", "R2"}, "R2", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewLimiter(3)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if err := l.Allow(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if err := l.Allow(); err == nil {
		t.Fatal("4th call allowed")
	}
	now = now.Add(20 * time.Second) // 1 token back at 3/min
	if err := l.Allow(); err != nil {
		t.Fatal("token did not refill")
	}
}

func TestAuditRedactsArgs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "audit.jsonl")
	a, err := NewAudit(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Write(Record{Tool: "x", Decision: "allowed", Args: map[string]any{"script": "export MY_TOKEN=supersecret1"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "supersecret1") {
		t.Fatalf("secret in audit: %s", b)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("audit mode %v", st.Mode().Perm())
	}
}

func TestRedactWholeAssignedValue(t *testing.T) {
	// live finding 2026-10-01: a key id inside an assigned value used to be redacted
	// alone, leaving the secret part after it
	in := "aws_secret_access_key = AKIAABCDEFGHIJKLMNOP/fakeSecretKey1234567890abcd\nAPI_KEY=sk-test-0123456789abcdefghijklmnop\nbare AKIAABCDEFGHIJKLMNOP in a log\nnormal line 42"
	out := Redact(in)
	for _, leak := range []string{"fakeSecretKey", "AKIAABCDEFGHIJKLMNOP", "sk-test"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	if !strings.Contains(out, "normal line 42") {
		t.Error("over-redacted")
	}
}
