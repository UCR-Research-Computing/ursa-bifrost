package backend

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CommandToken returns a Token func that runs argv (e.g. `gcloud auth
// print-access-token`) and caches the answer for 10 minutes. Laptop mode only:
// the HTTP server gets tokens from the signed-in user instead.
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
		if len(argv) == 0 {
			return "", errors.New("no token_command configured")
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).Output()
		if err != nil {
			return "", fmt.Errorf("%s: %v (try `gcloud auth login`)", argv[0], err)
		}
		tok, at = strings.TrimSpace(string(out)), time.Now()
		if tok == "" {
			return "", fmt.Errorf("%s printed no token", argv[0])
		}
		return tok, nil
	}
}
