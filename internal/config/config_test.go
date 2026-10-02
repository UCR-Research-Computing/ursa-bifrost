package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPartialLimitsKeepDefaults: a config that sets only limits.log_lines must
// keep the other limit defaults (yaml.v3 merges into the defaulted struct).
func TestPartialLimitsKeepDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(p, []byte("backend: fixture\nfixtures_dir: /tmp\nlimits:\n  log_lines: 1000\nstaging:\n  bucket: b\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.LogLines != 1000 || c.Limits.CallsPerMin != 60 || c.Limits.UntrustedCh != 16000 || c.Limits.ScriptBytes != 65536 {
		t.Fatalf("limits: %+v", c.Limits)
	}
	if c.Staging.Bucket != "b" || c.Staging.UploadMinutes != 15 || c.Staging.RetainDays != 7 || c.Staging.MaxUploadBytes != 5<<30 {
		t.Fatalf("staging: %+v", c.Staging)
	}
}
