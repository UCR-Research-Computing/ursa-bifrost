package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T, tiers string) string {
	t.Helper()
	dir := t.TempDir()
	td, _ := filepath.Abs("../../testdata")
	p := filepath.Join(dir, "config.yaml")
	body := "backend: fixture\nfixtures_dir: " + td + "\ncluster_user: alice_ucr_edu\ntiers: [" + tiers + "]\naudit_path: " + filepath.Join(dir, "audit.jsonl") +
		"\nstate_path: " + filepath.Join(dir, "a1.json") + "\nresults_dir: " + filepath.Join(dir, "results") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return out.String(), errb.String(), code
}

func TestCLIJSONIsPureJSON(t *testing.T) {
	cfg := testConfig(t, "R1")
	for _, args := range [][]string{
		{"status"}, {"partitions"}, {"jobs"}, {"job", "show", "236"}, {"job", "explain", "236"},
		{"job", "log", "236"}, {"modules", "gcc"}, {"recipes", "gromacs"}, {"usage"}, {"waste"},
	} {
		out, errOut, code := runCLI(t, append(args, "--json", "--config", cfg)...)
		if code != 0 {
			t.Errorf("%v: exit %d: %s %s", args, code, out, errOut)
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Errorf("%v: not JSON: %v\n%s", args, err, out)
			continue
		}
		if _, ok := v["data"]; !ok {
			t.Errorf("%v: no data envelope", args)
		}
	}
}

func TestCLIErrorsAsJSON(t *testing.T) {
	cfg := testConfig(t, "R1")
	out, _, code := runCLI(t, "jobs", "--all", "--json", "--config", cfg)
	if code != 1 || !strings.Contains(out, `"error"`) || !strings.Contains(out, "needs tier R2") {
		t.Fatalf("code %d out %s", code, out)
	}
	out, _, code = runCLI(t, "job", "show", "1;id", "--json", "--config", cfg)
	if code != 1 || !strings.Contains(out, "expected digits") {
		t.Fatalf("code %d out %s", code, out)
	}
}

func TestCLICheckExitCode(t *testing.T) {
	cfg := testConfig(t, "R1")
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.sh")
	_ = os.WriteFile(bad, []byte("#SBATCH -p nope\n"), 0o600)
	_, _, code := runCLI(t, "check", bad, "--config", cfg)
	if code != 2 {
		t.Errorf("bad script exit %d, want 2", code)
	}
	good := filepath.Join(dir, "good.sh")
	_ = os.WriteFile(good, []byte("#!/bin/bash\n#SBATCH -p standard\n#SBATCH -t 30\necho hi\n"), 0o600)
	out, _, code := runCLI(t, "check", good, "--config", cfg)
	if code != 0 || !strings.HasPrefix(out, "OK") {
		t.Errorf("good script exit %d: %s", code, out)
	}
}

func TestCLIStaffWithR2(t *testing.T) {
	cfg := testConfig(t, "R1, R2")
	out, errOut, code := runCLI(t, "jobs", "--all", "--config", cfg)
	if code != 0 || !strings.Contains(out, "USER") {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
}

func TestCLIHelpAndVersion(t *testing.T) {
	out, _, code := runCLI(t, "--help")
	if code != 0 || !strings.Contains(out, "bifrost mcp") {
		t.Fatal(out)
	}
	out, _, _ = runCLI(t, "version", "--json")
	if !strings.Contains(out, `"version"`) {
		t.Fatal(out)
	}
	_, errOut, code := runCLI(t, "frobnicate", "--config", testConfig(t, "R1"))
	if code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Fatal(errOut)
	}
}

func TestConfigInitRefusesOverwrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c", "config.yaml")
	if _, _, code := runCLI(t, "config", "init", "--config", p); code != 0 {
		t.Fatal("init failed")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if _, _, code := runCLI(t, "config", "init", "--config", p); code != 1 {
		t.Fatal("second init overwrote the file")
	}
}

func TestCLIP2StaffCommands(t *testing.T) {
	r1 := testConfig(t, "R1")
	for _, args := range [][]string{{"health"}, {"ticket", "236"}, {"waste", "--all"}} {
		out, _, code := runCLI(t, append(args, "--json", "--config", r1)...)
		if code != 1 || !strings.Contains(out, "needs tier R2") {
			t.Errorf("%v at R1: code %d %s", args, code, out)
		}
	}
	r2 := testConfig(t, "R1, R2")
	out, _, code := runCLI(t, "ticket", "236", "--config", r2)
	if code != 0 || !strings.Contains(out, "reply draft") || !strings.Contains(out, "pandas") {
		t.Errorf("ticket: %d %s", code, out)
	}
	out, _, code = runCLI(t, "health", "--json", "--config", r2)
	if code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Errorf("health: %d %s", code, out)
	}
}

func TestCLISubmitNeedsConfirmation(t *testing.T) {
	cfg := testConfig(t, "R1, A1")
	dir := t.TempDir()
	sc := filepath.Join(dir, "j.sh")
	_ = os.WriteFile(sc, []byte("#!/bin/bash\n#SBATCH -p standard\n#SBATCH -t 20\necho hi\n"), 0o600)
	// --json without --yes only prepares
	out, _, code := runCLI(t, "submit", sc, "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, "confirm_token") || strings.Contains(out, `"job_id"`) {
		t.Fatalf("prepare: %d %s", code, out)
	}
	var v struct {
		Data struct {
			Token string `json:"confirm_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &v)
	out, _, code = runCLI(t, "confirm", v.Data.Token, "--config", cfg)
	if code != 0 || !strings.Contains(out, "Submitted job 9001") {
		t.Fatalf("confirm: %d %s", code, out)
	}
	// R1-only config cannot submit
	out, _, code = runCLI(t, "submit", sc, "--yes", "--json", "--config", testConfig(t, "R1"))
	if code != 1 || !strings.Contains(out, "needs tier A1") {
		t.Fatalf("R1 submit: %d %s", code, out)
	}
}

func TestElapsedText(t *testing.T) {
	for _, c := range []struct {
		state string
		sec   int64
		want  string
	}{{"COMPLETED", 0, "0m00s"}, {"PENDING", 0, "-"}, {"CANCELLED by 1", 0, "-"}, {"FAILED", 61, "1m01s"}, {"RUNNING", 0, "0m00s"}} {
		if got := elapsedText(c.state, c.sec); got != c.want {
			t.Errorf("%s %d: %q, want %q", c.state, c.sec, got, c.want)
		}
	}
}

// TestConfirmRaceAcrossProcesses runs the built CLI five times in parallel on one
// token. Exactly one may submit (live finding 2026-10-01: before the file lock,
// all five reached sbatch).
func TestConfirmRaceAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "bifrost")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	cfg := testConfig(t, "R1, A1")
	sc := filepath.Join(t.TempDir(), "j.sh")
	_ = os.WriteFile(sc, []byte("#!/bin/bash\n#SBATCH -p standard\n#SBATCH -t 20\necho hi\n"), 0o600)
	out, err := exec.Command(bin, "submit", sc, "--json", "--config", cfg).Output()
	if err != nil {
		t.Fatalf("prepare: %v %s", err, out)
	}
	var v struct {
		Data struct {
			Token string `json:"confirm_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Data.Token == "" {
		t.Fatalf("no token: %s", out)
	}
	type res struct {
		out  string
		code int
	}
	ch := make(chan res, 5)
	for i := 0; i < 5; i++ {
		go func() {
			c := exec.Command(bin, "confirm", v.Data.Token, "--json", "--config", cfg)
			o, _ := c.CombinedOutput()
			ch <- res{string(o), c.ProcessState.ExitCode()}
		}()
	}
	ok := 0
	for i := 0; i < 5; i++ {
		r := <-ch
		if r.code == 0 && strings.Contains(r.out, `"job_id"`) {
			ok++
		} else if !strings.Contains(r.out, "confirm") {
			t.Errorf("unexpected refusal text: %s", r.out)
		}
	}
	if ok != 1 {
		t.Fatalf("%d confirms succeeded for one token, want exactly 1", ok)
	}
}
