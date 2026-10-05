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

// env_check's partition comes from config and reaches srun -p: only a plain
// partition name is accepted (v0.9.6).
func TestEnvPartitionValidated(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"check", "standard", "my_part-2"} {
		p := filepath.Join(dir, v+".yaml")
		_ = os.WriteFile(p, []byte("backend: fixture\nfixtures_dir: /tmp\nenv_check_partition: "+v+"\n"), 0o600)
		if c, err := Load(p); err != nil || c.EnvPartition != v {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for i, v := range []string{`"check; id"`, `"-w node1"`, `"CHECK"`, `"a b"`} {
		p := filepath.Join(dir, "bad"+string(rune('a'+i))+".yaml")
		_ = os.WriteFile(p, []byte("backend: fixture\nfixtures_dir: /tmp\nenv_check_partition: "+v+"\n"), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("env_check_partition %s accepted", v)
		}
	}
}
