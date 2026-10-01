package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
)

// newTestService returns a Service over the recorded fixtures, acting as the
// fixture user, with a fixed clock just after the fixtures were captured.
func newTestService(t *testing.T, tiers ...string) (*Service, *backend.Fixture) {
	t.Helper()
	cfg := config.Default()
	cfg.Backend = "fixture"
	cfg.FixturesDir = "../../testdata"
	cfg.ClusterUser = "alice_ucr_edu"
	if len(tiers) > 0 {
		cfg.Tiers = tiers
	}
	cfg.AuditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fx := s.Backend.(*backend.Fixture)
	s.Now = func() time.Time { return time.Unix(1790875401, 0) }
	return s, fx
}

func TestJobsList(t *testing.T) {
	s, _ := newTestService(t)
	js, err := s.JobsList(context.Background(), JobsListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(js) == 0 || js[0].JobID != "261" {
		t.Fatalf("expected newest first, got %+v", js[:1])
	}
	var running *JobSummary
	for i := range js {
		if js[i].JobID == "260" {
			running = &js[i]
		}
	}
	if running == nil || running.State != "RUNNING" || running.CostUSD <= 0 {
		t.Fatalf("job 260 should be RUNNING with a cost estimate: %+v", running)
	}
	// finished jobs drop the stale pending reason
	for _, j := range js {
		if j.State != "PENDING" && j.Reason != "" {
			t.Errorf("job %s (%s) kept reason %q", j.JobID, j.State, j.Reason)
		}
	}
	failed, _ := s.JobsList(context.Background(), JobsListInput{State: "failed"})
	for _, j := range failed {
		if j.State != "FAILED" {
			t.Errorf("filter leaked %s", j.State)
		}
	}
}

func TestJobShowEfficiencyAndOwnership(t *testing.T) {
	s, _ := newTestService(t)
	d, err := s.JobShow(context.Background(), JobShowInput{JobID: "236"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Efficiency == nil || d.Efficiency.MemAllocMB != 63216 || d.Efficiency.MemPeakMB == 0 {
		t.Fatalf("efficiency: %+v", d.Efficiency)
	}
	if d.Script != nil {
		t.Error("script returned without include_script")
	}
	// another user's job is denied at R1
	s.user = "bob_ucr_edu"
	s.Cfg.ClusterUser = "bob_ucr_edu"
	_, err = s.JobShow(context.Background(), JobShowInput{JobID: "236"})
	if !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("want denied, got %v", err)
	}
	// ...and allowed with AnyUser (the R2 tools)
	if _, err := s.JobShow(context.Background(), JobShowInput{JobID: "236", AnyUser: true}); err != nil {
		t.Fatal(err)
	}
}

func TestIncludeScriptIsRedacted(t *testing.T) {
	s, _ := newTestService(t)
	d, err := s.JobShow(context.Background(), JobShowInput{JobID: "236", IncludeScript: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Script == nil || strings.Contains(d.Script.Text, "abc123secretvalue") || !strings.Contains(d.Script.Text, "REDACTED") {
		t.Fatalf("script not redacted: %+v", d.Script)
	}
	if d.Script.Note != policy.UntrustedNote {
		t.Error("script must carry the untrusted note")
	}
}

func TestJobExplainReadsLog(t *testing.T) {
	s, fx := newTestService(t)
	ex, err := s.JobExplain(context.Background(), "236", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Findings) == 0 || ex.Findings[0].Rule != "python-import" {
		t.Fatalf("findings: %+v", ex.Findings)
	}
	if ex.LogTail == nil || ex.LogTail.Note != policy.UntrustedNote {
		t.Fatal("log tail must be wrapped as untrusted")
	}
	found := false
	for _, c := range fx.Calls {
		if strings.HasPrefix(c, "tail -n 80 -- /home/alice_ucr_edu/deep-research-lab/run_76/job.log") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tail of the recorded path, calls: %v", fx.Calls)
	}
}

func TestLogPathOutsideRootsIsDenied(t *testing.T) {
	s, fx := newTestService(t)
	s.Cfg.LogRoots = []string{"/scratch/{user}/"}
	_, err := s.JobLogTail(context.Background(), "236", "stdout", 10, false)
	if !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("want denied, got %v", err)
	}
	for _, c := range fx.Calls {
		if strings.HasPrefix(c, "tail") {
			t.Fatalf("tail ran despite the denial: %v", c)
		}
	}
	if err := s.checkLogPath("/home/alice_ucr_edu/../bob/x.log", "alice_ucr_edu"); err == nil {
		t.Error("dot-dot path accepted")
	}
	if err := s.checkLogPath("/home/alice_ucr_eduX/x.log", "alice_ucr_edu"); err == nil {
		t.Error("prefix-sibling path accepted")
	}
}

func TestLogLinesCapped(t *testing.T) {
	s, fx := newTestService(t)
	if _, err := s.JobLogTail(context.Background(), "236", "stdout", 5000, false); err != nil {
		t.Fatal(err)
	}
	last := fx.Calls[len(fx.Calls)-1]
	if !strings.HasPrefix(last, "tail -n 200 ") {
		t.Errorf("lines not capped: %s", last)
	}
}

func TestClusterStatus(t *testing.T) {
	s, _ := newTestService(t)
	cs, err := s.ClusterStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cs.JobsRunning != 1 || cs.NodesUp != 1 || cs.BurnPerHour == nil || *cs.BurnPerHour != 1.87 {
		t.Fatalf("status: running %d up %d burn %v", cs.JobsRunning, cs.NodesUp, cs.BurnPerHour)
	}
	if cs.SlurmVersion != "25.11.4" {
		t.Error(cs.SlurmVersion)
	}
}

func TestShowCostFalseHidesDollars(t *testing.T) {
	s, _ := newTestService(t)
	s.Cfg.ShowCost = false
	cs, _ := s.ClusterStatus(context.Background())
	if cs.BurnPerHour != nil {
		t.Error("burn shown with show_cost false")
	}
	u, _ := s.Usage(context.Background(), UsageInput{})
	if u.Total.CostUSD != 0 {
		t.Error("usage cost shown with show_cost false")
	}
	js, _ := s.JobsList(context.Background(), JobsListInput{})
	for _, j := range js {
		if j.CostUSD != 0 {
			t.Fatalf("job %s cost shown", j.JobID)
		}
	}
}

func TestUsage(t *testing.T) {
	s, _ := newTestService(t)
	u, err := s.Usage(context.Background(), UsageInput{GroupBy: "partition"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Total.Jobs != 12 || u.Total.NodeHours <= 0 || u.Total.CostUSD <= 0 {
		t.Fatalf("total: %+v", u.Total)
	}
	var sum int
	var gpu float64
	for _, r := range u.Rows {
		sum += r.Jobs
		if r.Key == "gpul4" {
			gpu = r.GPUHours
		}
	}
	if sum != u.Total.Jobs {
		t.Errorf("rows sum %d != total %d", sum, u.Total.Jobs)
	}
	if gpu <= 0 {
		t.Error("gpul4 jobs should count GPU-hours (whole-node GPU)")
	}
	if _, err := s.Usage(context.Background(), UsageInput{GroupBy: "lab"}); err == nil {
		t.Error("unknown group_by accepted")
	}
}

func TestScriptCheck(t *testing.T) {
	s, _ := newTestService(t)
	good := "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -N 2\n#SBATCH --ntasks-per-node=22\n#SBATCH -t 04:00:00\nmodule load openmpi gromacs/2026.1\nsrun gmx_mpi mdrun -deffnm md\n"
	sc, err := s.ScriptCheck(context.Background(), good)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.OK {
		t.Fatalf("good script flagged: %+v", sc.Issues)
	}
	if sc.EstCostUSD == nil || *sc.EstCostUSD != 14.96 { // 1.87 x 2 nodes x 4 h
		t.Errorf("cost: %v", sc.EstCostUSD)
	}

	bad := "#SBATCH -p gpu\nmodule load gromacs/2026.1\npython x.py\n"
	sc, _ = s.ScriptCheck(context.Background(), bad)
	if sc.OK {
		t.Fatal("bad script passed")
	}
	msgs := issues(sc)
	for _, want := range []string{"shebang", `partition "gpu" does not exist`, "no --time"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("missing %q in:\n%s", want, msgs)
		}
	}

	mpi := "#!/bin/bash\n#SBATCH -p standard\n#SBATCH -t 10\nmodule load gromacs/2026.1\n"
	sc, _ = s.ScriptCheck(context.Background(), mpi)
	if !strings.Contains(issues(sc), "needs `module load openmpi` first") {
		t.Errorf("MPI prerequisite not caught: %s", issues(sc))
	}

	tooBig := "#!/bin/bash\n#SBATCH -p gpul4\n#SBATCH -c 16\n#SBATCH --mem=100G\n#SBATCH -t 1:00:00\n#SBATCH --gres=gpu:2\n"
	sc, _ = s.ScriptCheck(context.Background(), tooBig)
	m := issues(sc)
	for _, want := range []string{"16 CPUs per node", "--mem 100G", "2 GPUs per node"} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %q in:\n%s", want, m)
		}
	}

	typo := "#!/bin/bash\n#SBATCH -t 5\nmodule load gromac\n"
	sc, _ = s.ScriptCheck(context.Background(), typo)
	if !strings.Contains(issues(sc), "closest: gromacs") {
		t.Errorf("no suggestion: %s", issues(sc))
	}

	spot := "#!/bin/bash\n#SBATCH -p spot\n#SBATCH -t 1-00:00\nmodule load python-sci\npython x.py\n"
	sc, _ = s.ScriptCheck(context.Background(), spot)
	if !strings.Contains(issues(sc), "--requeue") {
		t.Error("spot requeue warning missing")
	}
	if sc.EstCostUSD == nil || *sc.EstCostUSD != 17.76 { // 0.74 x 24 h
		t.Errorf("spot cost %v", sc.EstCostUSD)
	}
}

func issues(sc *ScriptCheck) string {
	var b strings.Builder
	for _, i := range sc.Issues {
		b.WriteString(i.Severity + ": " + i.Message + "\n")
	}
	return b.String()
}

func TestSlurmMinutes(t *testing.T) {
	cases := map[string]int{"30": 30, "1:00:00": 60, "04:30:00": 270, "1-00:00": 1440, "2-12": 3600, "1-01:30:00": 1530, "45:00": 45}
	for in, want := range cases {
		if got, ok := slurmMinutes(in); !ok || got != want {
			t.Errorf("%s: %d %v (want %d)", in, got, ok, want)
		}
	}
}

func TestCallAuditsAndDenies(t *testing.T) {
	s, _ := newTestService(t, "R1")
	_, err := Call(context.Background(), s, "test", "jobs_list_all", "R2", map[string]any{"user": "bob"}, true,
		func(ctx context.Context) (int, error) { t.Fatal("ran despite denial"); return 0, nil })
	if !errors.Is(err, policy.ErrDenied) {
		t.Fatalf("want denied: %v", err)
	}
	r, err := Call(context.Background(), s, "test", "cluster_status", "R1", nil, true, s.ClusterStatus)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Source.Commands) == 0 || r.AsOf == "" || !strings.HasPrefix(r.Source.Backend, "fixture") {
		t.Errorf("envelope: %+v", r.Source)
	}
	b, _ := os.ReadFile(s.Audit.Path())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines: %d", len(lines))
	}
	var rec policy.Record
	_ = json.Unmarshal([]byte(lines[0]), &rec)
	if rec.Decision != "denied" || rec.Tool != "jobs_list_all" {
		t.Errorf("first record: %+v", rec)
	}
	_ = json.Unmarshal([]byte(lines[1]), &rec)
	if rec.Decision != "allowed" || len(rec.Commands) == 0 {
		t.Errorf("second record: %+v", rec)
	}
}

func TestCacheAvoidsRepeatCalls(t *testing.T) {
	s, fx := newTestService(t)
	ctx := context.Background()
	_, _ = s.ClusterStatus(ctx)
	n := len(fx.Calls)
	_, _ = s.ClusterStatus(ctx)
	if len(fx.Calls) != n {
		t.Errorf("second status call hit the backend: %v", fx.Calls[n:])
	}
	s.Now = func() time.Time { return time.Unix(1790875401+3600, 0) }
	_, _ = s.ClusterStatus(ctx)
	if len(fx.Calls) == n {
		t.Error("cache never expired")
	}
}

func TestModules(t *testing.T) {
	s, _ := newTestService(t)
	cat, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := cat.SearchModules("gromacs")
	if len(h) != 1 || h[0].Requires != "module load openmpi" || !h[0].GPU {
		t.Fatalf("%+v", h)
	}
	if ok, _ := cat.ModuleExists("python-sci"); !ok {
		t.Error("python-sci missing")
	}
	if ok, needs := cat.ModuleExists("lammps/20250722.4-cuda"); !ok || needs == "" {
		t.Error("lammps cuda")
	}
	if ok, _ := cat.ModuleExists("numpy"); ok {
		t.Error("numpy is not a module")
	}
	if len(cat.SearchRecipes("pytorch")) == 0 {
		t.Error("no pytorch recipe")
	}
}
