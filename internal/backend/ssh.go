package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
)

// SSH runs commands on the login node over one multiplexed ssh connection.
type SSH struct {
	cfg  config.SSH
	mu   sync.Mutex
	argv []string // ssh argv without destination
	dest string
}

// NewSSH returns an SSH backend; the connection is set up on first use.
func NewSSH(cfg config.SSH) *SSH { return &SSH{cfg: cfg} }

// Name is the backend label used in `source`.
func (s *SSH) Name() string {
	if s.cfg.GCloud != nil {
		return "ssh-iap:" + s.cfg.GCloud.Instance
	}
	return "ssh:" + s.cfg.Host
}

func (s *SSH) base(ctx context.Context) ([]string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.argv != nil {
		return s.argv, s.dest, nil
	}
	var argv []string
	var dest string
	if g := s.cfg.GCloud; g != nil {
		// gcloud prints the exact ssh command it would run (key, IAP proxy, known hosts).
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "gcloud", "compute", "ssh", g.Instance,
			"--zone="+g.Zone, "--project="+g.Project, "--tunnel-through-iap", "--dry-run").Output()
		if err != nil {
			var ee *exec.ExitError
			msg := err.Error()
			if errors.As(err, &ee) && len(ee.Stderr) > 0 {
				msg = lastLine(string(ee.Stderr))
			}
			return nil, "", fmt.Errorf("%w: gcloud: %s (try `gcloud auth login`)", ErrUnreachable, msg)
		}
		parts, err := splitShell(strings.TrimSpace(string(out)))
		if err != nil || len(parts) < 2 {
			return nil, "", fmt.Errorf("%w: cannot parse gcloud ssh command", ErrUnreachable)
		}
		for _, p := range parts[:len(parts)-1] {
			if p != "-t" {
				argv = append(argv, p)
			}
		}
		dest = parts[len(parts)-1]
	} else {
		argv = []string{"ssh"}
		dest = s.cfg.Host
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	persist := s.cfg.ControlPersist
	if persist <= 0 {
		persist = 900
	}
	timeout := s.cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 30
	}
	argv = append(argv,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(dir, "bifrost-%C"),
		"-o", fmt.Sprintf("ControlPersist=%d", persist),
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=30",
		"-o", fmt.Sprintf("ConnectTimeout=%d", timeout),
		"-T",
	)
	s.argv, s.dest = argv, dest
	return argv, dest, nil
}

// Run executes c on the login node.
func (s *SSH) Run(ctx context.Context, c Command) ([]byte, error) {
	argv, dest, err := s.base(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout(c))
	defer cancel()
	full := append(append([]string{}, argv[1:]...), "--", dest, RemoteLine(c))
	cmd := exec.CommandContext(ctx, argv[0], full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s timed out after %s", c.argv[0], Timeout(c))
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code := ee.ExitCode()
			if code == 255 { // ssh itself failed; rebuild next time
				s.mu.Lock()
				s.argv = nil
				s.mu.Unlock()
				return nil, fmt.Errorf("%w: %s", ErrUnreachable, lastLine(stderr.String()))
			}
			if okExit(c, code) {
				return stdout.Bytes(), nil
			}
			return stdout.Bytes(), fmt.Errorf("%s exited %d: %s", c.argv[0], code, lastLine(stderr.String()))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 300 {
		l = l[:300]
	}
	if l == "" {
		return "no error text"
	}
	return l
}

// splitShell splits a command line printed by gcloud (double/single quotes, no
// variables). It is used only on gcloud's own output.
func splitShell(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inArg := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inArg = true
		case r == ' ' || r == '\t' || r == '\n':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out, nil
}
