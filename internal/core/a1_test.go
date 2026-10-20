package core

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
)

const goodScript = "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -N 1\n#SBATCH -t 01:00:00\n#SBATCH -J hello\nmodule load python-sci\npython -c 'print(42)' > answer.txt\n"

func a1Service(t *testing.T) (*Service, *backend.Fixture) {
	t.Helper()
	s, fx := newTestService(t, "R1", "R2", "A1")
	s.Cfg.StatePath = filepath.Join(t.TempDir(), "a1.json")
	s.Cfg.ResultsDir = filepath.Join(t.TempDir(), "results")
	return s, fx
}

func TestSubmitTwoStep(t *testing.T) {
	s, fx := a1Service(t)
	ctx := context.Background()
	plan, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
	if err != nil {
		t.Fatal(err)
	}
	if plan.WorstCaseUSD != 1.87 || plan.Partition != "computehigh" || plan.Nodes != 1 || plan.TimeLimit != "1 h" {
		t.Errorf("plan: %+v", plan)
	}
	if !strings.HasPrefix(plan.Token, "bf1-") || len(plan.Token) < 30 {
		t.Errorf("token: %q", plan.Token)
	}
	if !strings.Contains(plan.Scheduler, "would start at") {
		t.Errorf("scheduler: %q", plan.Scheduler)
	}
	// nothing submitted at prepare time
	for _, c := range fx.Calls {
		if strings.HasPrefix(c, "bash -c") || strings.HasPrefix(c, "sbatch --parsable") {
			t.Fatalf("prepare submitted something: %s", c)
		}
	}
	// test-only carries the enforced flags and the script on stdin
	var testOnly string
	for i, c := range fx.Calls {
		if strings.HasPrefix(c, "sbatch --test-only") {
			testOnly = c
			if string(fx.Stdin[i]) != goodScript {
				t.Error("test-only did not get the script on stdin")
			}
		}
	}
	for _, want := range []string{"--partition=computehigh", "--nodes=1", "--time=60", "--comment=bifrost:"} {
		if !strings.Contains(testOnly, want) {
			t.Errorf("test-only lacks %s: %s", want, testOnly)
		}
	}

	res, err := s.ConfirmSubmit(ctx, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.JobID != "9001" || !strings.HasPrefix(res.RemoteDir, "~/bifrost-jobs/") {
		t.Errorf("confirmed: %+v", res)
	}
	last := fx.Calls[len(fx.Calls)-1]
	if !strings.HasPrefix(last, "bash -c ") || !strings.Contains(last, "--partition=computehigh") || string(fx.Stdin[len(fx.Stdin)-1]) != goodScript {
		t.Errorf("submit call: %s", last)
	}
	// single use
	if _, err := s.ConfirmSubmit(ctx, plan.Token); err == nil {
		t.Fatal("token reused")
	}
	usd, n, _ := s.SpendToday()
	if usd != 1.87 || n != 1 {
		t.Errorf("ledger: %v %d", usd, n)
	}
}

func TestSubmitCaps(t *testing.T) {
	s, fx := a1Service(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   SubmitInput
		want string
	}{
		{"nodes", SubmitInput{Script: goodScript, Nodes: 5}, "5 nodes requested, cap is 4"},
		{"hours", SubmitInput{Script: goodScript, Time: "1-01:00:00"}, "over the 24 h cap"},
		{"cost", SubmitInput{Script: goodScript, Partition: "highmem", Nodes: 2, Time: "4:00:00"}, "per-job cap"}, // 4.19 x 2 x 4 = 33.52
		{"no time", SubmitInput{Script: "#!/bin/bash\n#SBATCH -p standard\necho hi\n"}, "time limit is required"},
		{"bad module", SubmitInput{Script: "#!/bin/bash\n#SBATCH -t 10\nmodule load gromac\n"}, "script_check found errors"},
		{"bad partition", SubmitInput{Script: goodScript, Partition: "gpu"}, `partition "gpu" does not exist`},
		{"empty", SubmitInput{Script: "  "}, "empty script"},
	}
	for _, c := range cases {
		_, err := s.PrepareSubmit(ctx, c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	for _, c := range fx.Calls {
		if strings.HasPrefix(c, "sbatch") || strings.HasPrefix(c, "bash -c") {
			t.Fatalf("a refused plan reached the scheduler: %s", c)
		}
	}
	_, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript, Nodes: 5})
	if !errors.Is(err, ErrCapExceeded) {
		t.Error("cap errors must wrap ErrCapExceeded")
	}
}

func TestDayCap(t *testing.T) {
	s, _ := a1Service(t)
	ctx := context.Background()
	big := SubmitInput{Script: goodScript, Nodes: 4, Time: "3:00:00"} // 1.87 x 4 x 3 = 22.44
	for i := 0; i < 2; i++ {
		p, err := s.PrepareSubmit(ctx, big)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ConfirmSubmit(ctx, p.Token); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.PrepareSubmit(ctx, big) // 44.88 + 22.44 > 50
	if err == nil || !strings.Contains(err.Error(), "day cap") {
		t.Fatalf("day cap not enforced: %v", err)
	}
	// a plan prepared before the cap filled is re-checked at confirm time
	s2, _ := a1Service(t)
	p1, _ := s2.PrepareSubmit(ctx, big)
	p2, _ := s2.PrepareSubmit(ctx, big)
	p3, _ := s2.PrepareSubmit(ctx, big)
	if _, err := s2.ConfirmSubmit(ctx, p1.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ConfirmSubmit(ctx, p2.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ConfirmSubmit(ctx, p3.Token); err == nil || !strings.Contains(err.Error(), "day cap") {
		t.Fatalf("confirm-time day cap: %v", err)
	}
}

func TestTokenExpiryAndTampering(t *testing.T) {
	s, fx := a1Service(t)
	ctx := context.Background()
	p, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
	if err != nil {
		t.Fatal(err)
	}
	base := s.Now()
	s.Now = func() time.Time { return base.Add(11 * time.Minute) }
	if _, err := s.ConfirmSubmit(ctx, p.Token); err == nil || !strings.Contains(err.Error(), "expired") && !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expired token accepted: %v", err)
	}
	s.Now = func() time.Time { return base }
	if _, err := s.ConfirmSubmit(ctx, "bf1-000000000000000000000000000000000000"); err == nil {
		t.Fatal("made-up token accepted")
	}
	// tamper with the stored script: the hash check must refuse
	p, _ = s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
	b, _ := os.ReadFile(s.statePath())
	b2 := strings.Replace(string(b), "print(42)", "print(666)", 1)
	if b2 == string(b) {
		t.Fatal("test setup: script not found in state")
	}
	_ = os.WriteFile(s.statePath(), []byte(b2), 0o600)
	n := len(fx.Calls)
	if _, err := s.ConfirmSubmit(ctx, p.Token); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered plan submitted: %v", err)
	}
	for _, c := range fx.Calls[n:] {
		if strings.HasPrefix(c, "bash -c") {
			t.Fatal("tampered plan reached sbatch")
		}
	}
	st, _ := os.Stat(s.statePath())
	if st.Mode().Perm() != 0o600 {
		t.Errorf("state file mode %v", st.Mode().Perm())
	}
}

func TestTokenKindMismatch(t *testing.T) {
	s, _ := a1Service(t)
	ctx := context.Background()
	p, _ := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
	if _, err := s.ConfirmAction(ctx, "cancel", p.Token); err == nil || !strings.Contains(err.Error(), "for submit") {
		t.Fatalf("submit token used for cancel: %v", err)
	}
}

func TestSchedulerRejection(t *testing.T) {
	s, fx := a1Service(t)
	fx.TestOnly = "ERROR:allocation failure: Requested node configuration is not available"
	_, err := s.PrepareSubmit(context.Background(), SubmitInput{Script: goodScript})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("scheduler rejection not surfaced: %v", err)
	}
	b, _ := os.ReadFile(s.statePath())
	if strings.Contains(string(b), "print(42)") {
		t.Error("a rejected plan was stored")
	}
}

func TestOverridesReported(t *testing.T) {
	s, _ := a1Service(t)
	p, err := s.PrepareSubmit(context.Background(), SubmitInput{Script: goodScript, Partition: "standard", Time: "30"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(p.Overrides, "|")
	if !strings.Contains(joined, "partition computehigh replaced by standard") || !strings.Contains(joined, "time 01:00:00 replaced by 30 min") {
		t.Errorf("overrides: %v", p.Overrides)
	}
	if p.WorstCaseUSD != 0.73 { // 1.45 x 0.5
		t.Errorf("cost %v", p.WorstCaseUSD)
	}
}

func TestCancelHoldRelease(t *testing.T) {
	s, fx := a1Service(t)
	ctx := context.Background()
	// 260 is RUNNING in the fixtures
	p, err := s.PrepareAction(ctx, "cancel", "260")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Calls {
		if strings.HasPrefix(c, "scancel") {
			t.Fatal("prepare cancelled")
		}
	}
	r, err := s.ConfirmAction(ctx, "cancel", p.Token)
	if err != nil || r.JobID != "260" {
		t.Fatalf("%v %+v", err, r)
	}
	if fx.Calls[len(fx.Calls)-1] != "scancel 260" {
		t.Errorf("last call %s", fx.Calls[len(fx.Calls)-1])
	}
	if _, err := s.ConfirmAction(ctx, "cancel", p.Token); err == nil {
		t.Error("cancel token reused")
	}
	// finished job: nothing to cancel; running job cannot be held
	if _, err := s.PrepareAction(ctx, "cancel", "236"); err == nil || !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("cancel of a finished job: %v", err)
	}
	if _, err := s.PrepareAction(ctx, "hold", "260"); err == nil || !strings.Contains(err.Error(), "only pending") {
		t.Errorf("hold of a running job: %v", err)
	}
	// other users' jobs are refused even with A1 + R2
	s.user, s.Cfg.ClusterUser = "bob_ucr_edu", "bob_ucr_edu"
	if _, err := s.PrepareAction(ctx, "cancel", "260"); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("cancel of another user's job: %v", err)
	}
}

func TestA1NeedsTier(t *testing.T) {
	s, _ := newTestService(t, "R1", "R2")
	s.Cfg.StatePath = filepath.Join(t.TempDir(), "a1.json")
	_, err := Call(context.Background(), s, "test", "job_submit", "A1", nil, true,
		func(ctx context.Context) (*SubmitPlan, error) {
			return s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
		})
	if err == nil || !strings.Contains(err.Error(), "needs tier A1") {
		t.Fatalf("A1 without the tier: %v", err)
	}
}

func resultsFixture(s *Service, fx *backend.Fixture, dir string) {
	fx.Files = map[string]map[string]string{dir: {
		"answer.txt": "42\n", "job.sbatch": goodScript, "out/data.csv": "a,b\n1,2\n",
		"log.txt": "API_TOKEN=supersecret123 done\n",
	}}
}

func TestJobResults(t *testing.T) {
	s, fx := a1Service(t)
	ctx := context.Background()
	// job 236's working dir in the fixtures
	dir := "/home/alice_ucr_edu/deep-research-lab/run_76"
	resultsFixture(s, fx, dir)
	r, err := s.JobResults(ctx, ResultsInput{JobID: "236"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Files) != 4 || r.Folder != dir {
		t.Fatalf("files: %+v", r.Files)
	}
	r, err = s.JobResults(ctx, ResultsInput{JobID: "236", Read: "log.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Preview == nil || strings.Contains(r.Preview.Text, "supersecret123") {
		t.Errorf("preview not redacted: %+v", r.Preview)
	}
	if _, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "../../.ssh/id_rsa"}); err == nil {
		t.Error("read outside the job folder")
	}
	if _, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "missing.txt"}); err == nil {
		t.Error("read of a file not in the listing")
	}
	r, err = s.JobResults(ctx, ResultsInput{JobID: "236", Download: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Saved) != 4 {
		t.Fatalf("saved: %v", r.Saved)
	}
	b, _ := os.ReadFile(filepath.Join(r.Downloaded, "out", "data.csv"))
	if string(b) != "a,b\n1,2\n" {
		t.Errorf("content: %q", b)
	}
	// a second download never overwrites
	r, _ = s.JobResults(ctx, ResultsInput{JobID: "236", Download: true, Files: []string{"answer.txt"}})
	if len(r.Saved) != 1 || r.Saved[0] != "answer.txt.1" {
		t.Errorf("second download: %v", r.Saved)
	}
}

func TestDownloadRefusesHostileArchive(t *testing.T) {
	s, fx := a1Service(t)
	dir := "/home/alice_ucr_edu/deep-research-lab/run_76"
	resultsFixture(s, fx, dir)
	for _, h := range []tar.Header{
		{Name: "../../evil.sh", Mode: 0o755, Size: 1, Typeflag: tar.TypeReg},
		{Name: "/etc/evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg},
	} {
		backend.FakeTarExtra = []tar.Header{h}
		_, err := s.JobResults(context.Background(), ResultsInput{JobID: "236", Download: true})
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: %v", h.Name, err)
		}
	}
	backend.FakeTarExtra = []tar.Header{{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}}
	r, err := s.JobResults(context.Background(), ResultsInput{JobID: "236", Download: true})
	backend.FakeTarExtra = nil
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(r.Downloaded, "link")); err == nil {
		t.Error("symlink was extracted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.Cfg.ResultsDir), "evil.sh")); err == nil {
		t.Error("path traversal wrote outside the results folder")
	}
}

func TestResultsRefusesHomeAndOtherUsers(t *testing.T) {
	s, _ := a1Service(t)
	// job 260 (the warm worker) ran with its working directory = $HOME
	_, err := s.JobResults(context.Background(), ResultsInput{JobID: "260"})
	if err == nil || !strings.Contains(err.Error(), "home folder itself") {
		t.Errorf("home folder listing: %v", err)
	}
	s.user, s.Cfg.ClusterUser = "bob_ucr_edu", "bob_ucr_edu"
	if _, err := s.JobResults(context.Background(), ResultsInput{JobID: "236"}); err == nil {
		t.Error("another user's results")
	}
}

func TestSubmitWarnsOnStockout(t *testing.T) {
	dir := mutatedFixtures(t, "nodes.json", func(doc map[string]any) {
		for _, x := range doc["nodes"].([]any) {
			n := x.(map[string]any)
			if n["name"] == "ucrslurmcl-c3nodeset-1" {
				n["state"] = []string{"DOWN", "CLOUD", "NOT_RESPONDING", "POWERING_UP"}
				n["reason"] = "GCP Error: ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS"
			}
		}
	})
	s := serviceAt(t, dir, "R1", "A1")
	s.Cfg.StatePath = filepath.Join(t.TempDir(), "a1.json")
	p, err := s.PrepareSubmit(context.Background(), SubmitInput{Script: goodScript})
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(p.Warnings, "|")
	if !strings.Contains(w, "no capacity for computehigh") || !strings.Contains(w, "consider partition") {
		t.Errorf("warnings: %v", p.Warnings)
	}
	// the cheap default (standard, e2 in any zone) is the first alternative offered (v0.9.8)
	if !strings.Contains(w, "consider partition standard") {
		t.Errorf("alternative should be standard first: %v", p.Warnings)
	}
	// and when standard itself is short, spot (same e2 nodes) comes before computehigh
	dir2 := mutatedFixtures(t, "nodes.json", func(doc map[string]any) {
		for _, x := range doc["nodes"].([]any) {
			n := x.(map[string]any)
			if n["name"] == "ucrslurmcl-c3nodeset-1" {
				n["name"] = "ucrslurmcl-stdnodeset-1"
				n["partitions"] = []string{"standard"}
				n["state"] = []string{"DOWN", "CLOUD", "NOT_RESPONDING", "POWERING_UP"}
				n["reason"] = "GCP Error: ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS"
			}
		}
	})
	s3 := serviceAt(t, dir2, "R1", "A1")
	s3.Cfg.StatePath = filepath.Join(t.TempDir(), "a1.json")
	std := strings.Replace(goodScript, "-p computehigh", "-p standard", 1)
	p3, err := s3.PrepareSubmit(context.Background(), SubmitInput{Script: std})
	if err != nil {
		t.Fatal(err)
	}
	if w3 := strings.Join(p3.Warnings, "|"); !strings.Contains(w3, "no capacity for standard") ||
		!strings.Contains(w3, "consider partition spot") {
		t.Errorf("standard short: alternative should be spot: %v", p3.Warnings)
	}
	// a healthy partition gets no such warning
	s2, _ := a1Service(t)
	p, _ = s2.PrepareSubmit(context.Background(), SubmitInput{Script: goodScript})
	for _, x := range p.Warnings {
		if strings.Contains(x, "capacity") || strings.Contains(x, "failed to start") {
			t.Errorf("false stockout warning: %s", x)
		}
	}
}

func TestBootingNodesAreNotStockouts(t *testing.T) {
	dir := mutatedFixtures(t, "nodes.json", func(doc map[string]any) {
		for _, x := range doc["nodes"].([]any) {
			n := x.(map[string]any)
			if n["name"] == "ucrslurmcl-c3nodeset-1" {
				n["state"] = []string{"ALLOCATED", "CLOUD", "NOT_RESPONDING", "POWERING_UP", "POWER_DOWN"}
				n["reason"] = ""
			}
		}
	})
	s := serviceAt(t, dir, "R1", "R2", "A1")
	s.Cfg.StatePath = filepath.Join(t.TempDir(), "a1.json")
	p, err := s.PrepareSubmit(context.Background(), SubmitInput{Script: goodScript})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range p.Warnings {
		if strings.Contains(w, "failed to start") || strings.Contains(w, "capacity") {
			t.Errorf("booting node reported as a failure: %s", w)
		}
	}
	h, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range h.Issues {
		if i.Kind == "node-down" && i.Subject == "ucrslurmcl-c3nodeset-1" {
			t.Errorf("health calls a booting node down: %+v", i)
		}
	}
}

func TestCancelBeforeStartReleasesDayCap(t *testing.T) {
	// a copy of the running job 260, made PENDING and never started, as job 9001
	dir := mutatedFixtures(t, "squeue.json", func(doc map[string]any) {
		jobs := doc["jobs"].([]any)
		b, _ := json.Marshal(jobs[0])
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		p["job_id"] = 9001
		p["job_state"] = []string{"PENDING"}
		p["state_reason"] = "BeginTime"
		p["start_time"] = map[string]any{"set": true, "infinite": false, "number": 0}
		doc["jobs"] = append(jobs, p)
	})
	s := serviceAt(t, dir, "R1", "A1")
	s.Cfg.StatePath = filepath.Join(t.TempDir(), "a1.json")
	ctx := context.Background()
	p, _ := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript, Nodes: 4, Time: "3:00:00"}) // $22.44
	c, err := s.ConfirmSubmit(ctx, p.Token)
	if err != nil || c.JobID != "9001" {
		t.Fatalf("%v %+v", err, c)
	}
	if usd, _, _ := s.SpendToday(); usd != 22.44 {
		t.Fatalf("committed $%v", usd)
	}
	a, err := s.PrepareAction(ctx, "cancel", "9001")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ConfirmAction(ctx, "cancel", a.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Message, "no longer counts") {
		t.Errorf("message: %s", r.Message)
	}
	usd, n, _ := s.SpendToday()
	if usd != 0 || n != 1 {
		t.Errorf("after release: $%v, %d submits (want $0, 1)", usd, n)
	}
	// a running job that is cancelled keeps counting (it already spent)
	st, _ := s.loadState()
	st.Ledger = append(st.Ledger, ledgerEntry{Time: s.Now(), JobID: "260", WorstUSD: 5})
	_ = s.saveState(st)
	a, _ = s.PrepareAction(ctx, "cancel", "260")
	if _, err := s.ConfirmAction(ctx, "cancel", a.Token); err != nil {
		t.Fatal(err)
	}
	if usd, _, _ := s.SpendToday(); usd != 5 {
		t.Errorf("running job's worst case released: $%v", usd)
	}
}

func TestNoValTimestamps(t *testing.T) {
	if got := ts(4294967294); got != "" {
		t.Errorf("Slurm NO_VAL time shown as %q", got)
	}
	if got := ts(4294967295); got != "" {
		t.Errorf("Slurm INFINITE time shown as %q", got)
	}
	if ts(1790875000) == "" {
		t.Error("real time hidden")
	}
}

// TestStateLockIsHonored proves the A1 state file lock works across processes
// deterministically: another holder of the flock (as a second bifrost process
// would be) blocks a confirm until it lets go. The parallel-process race test
// in cmd/bifrost only catches a missing lock some of the time.
func TestStateLockIsHonored(t *testing.T) {
	s, _ := a1Service(t)
	ctx := context.Background()
	p, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.statePath()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.ConfirmSubmit(ctx, p.Token); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("confirm ran while another process held the state lock (err=%v)", err)
	case <-time.After(400 * time.Millisecond):
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("confirm after unlock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("confirm still blocked after the lock was released")
	}
}

// TestAlternativeOrder: standard (cheap e2, any zone) is offered first, then spot (same
// nodes), then the specialist partitions (v0.9.8).
func TestAlternativeOrder(t *testing.T) {
	want := []string{"standard", "spot", "computehigh", "nvmescratch", "highmem"}
	if strings.Join(altPartitions, ",") != strings.Join(want, ",") {
		t.Errorf("altPartitions = %v, want %v", altPartitions, want)
	}
	if strings.Join(altColdPartitions, ",") != "standard,spot,computehigh" {
		t.Errorf("altColdPartitions = %v", altColdPartitions)
	}
}

// A partition list goes to sbatch as written (Slurm picks whichever starts first) while the
// cost cap uses the dearest member.
func TestSubmitPartitionList(t *testing.T) {
	s, fx := a1Service(t)
	ctx := context.Background()
	script := "#!/bin/bash\n#SBATCH -p spot,standard\n#SBATCH --exclusive\n#SBATCH --requeue\n#SBATCH -t 60\necho hi\n"
	plan, err := s.PrepareSubmit(ctx, SubmitInput{Script: script})
	if err != nil {
		t.Fatal(err)
	}
	if plan.WorstCaseUSD != 1.45 || plan.Partition != "spot,standard" {
		t.Errorf("plan: worst %v partition %q, want 1.45 and the list", plan.WorstCaseUSD, plan.Partition)
	}
	found := false
	for _, c := range fx.Calls {
		if strings.HasPrefix(c, "sbatch --test-only") && strings.Contains(c, "--partition=spot,standard") {
			found = true
		}
	}
	if !found {
		t.Errorf("test-only did not carry --partition=spot,standard: %v", fx.Calls)
	}
	if _, err := s.PrepareSubmit(ctx, SubmitInput{Script: strings.Replace(script, "spot,standard", "spot,nosuch", 1)}); err == nil || !strings.Contains(err.Error(), `"nosuch" does not exist`) {
		t.Errorf("unknown list member not refused: %v", err)
	}
}

// v0.9.16: an override partition gets the full static check against itself.
func TestSubmitOverrideRechecksLimits(t *testing.T) {
	s, fx := a1Service(t)
	ctx := context.Background()
	// 20 cores fit computehigh (22) but not standard (16)
	script := "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH --exclusive\n#SBATCH -c 20\n#SBATCH -t 60\necho hi\n"
	if _, err := s.PrepareSubmit(ctx, SubmitInput{Script: script}); err != nil {
		t.Fatalf("baseline plan on computehigh: %v", err)
	}
	_, err := s.PrepareSubmit(ctx, SubmitInput{Script: script, Partition: "standard"})
	if err == nil || !strings.Contains(err.Error(), "20 CPUs per node requested; standard") {
		t.Errorf("override onto a smaller partition not re-checked: %v", err)
	}
	// GPUs requested, override onto a partition with none
	gpu := "#!/bin/bash\n#SBATCH -p gpul4\n#SBATCH --gres=gpu:1\n#SBATCH -t 60\necho hi\n"
	if _, err := s.PrepareSubmit(ctx, SubmitInput{Script: gpu, Partition: "highmem"}); err == nil || !strings.Contains(err.Error(), "GPUs requested on highmem") {
		t.Errorf("GPU script moved to highmem not refused: %v", err)
	}
	// override onto a shared partition without a core request still refused, once
	sh := sharedService(t)
	plain := "#!/bin/bash\n#SBATCH -p highmem\n#SBATCH -t 60\necho hi\n"
	_, err = sh.PrepareSubmit(ctx, SubmitInput{Script: plain, Partition: "standard"})
	if err == nil || strings.Count(err.Error(), "shares nodes between jobs") != 1 {
		t.Errorf("shared-partition override: want one shares-nodes error, got %v", err)
	}
	for _, c := range fx.Calls {
		if strings.HasPrefix(c, "sbatch --parsable") {
			t.Fatalf("a refused plan reached sbatch: %s", c)
		}
	}
}

func TestWithPartition(t *testing.T) {
	cases := map[string]string{
		"#!/bin/bash\n#SBATCH -p computehigh\necho":         "#SBATCH -p standard",
		"#!/bin/bash\n#SBATCH --partition=gpul4 -t 5\necho": "#SBATCH --partition=standard -t 5",
		"#!/bin/bash\n#SBATCH --partition gpul4\necho":      "#SBATCH --partition standard",
		"#!/bin/bash\n#SBATCH -pgpul4\necho":                "#SBATCH -pstandard",
		"#!/bin/bash\n#SBATCH -t 5\necho":                   "#SBATCH --partition=standard",
	}
	for in, want := range cases {
		if got := withPartition(in, "standard"); !strings.Contains(got, want) || !strings.HasPrefix(got, "#!/bin/bash\n") {
			t.Errorf("withPartition(%q) = %q, want it to contain %q", in, got, want)
		}
	}
}

// v0.9.16: plans prepared together cannot all be confirmed past max_submits_per_day.
func TestSubmitCountRecheckedAtConfirm(t *testing.T) {
	s, _ := a1Service(t)
	s.Cfg.Caps.MaxSubmitsPerDay = 2
	ctx := context.Background()
	var toks []string
	for i := 0; i < 3; i++ {
		p, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, p.Token)
	}
	for i, tok := range toks {
		_, err := s.ConfirmSubmit(ctx, tok)
		if i < 2 && err != nil {
			t.Fatalf("confirm %d: %v", i, err)
		}
		if i == 2 && (err == nil || !errors.Is(err, ErrCapExceeded) || !strings.Contains(err.Error(), "submissions today")) {
			t.Fatalf("third confirm past a cap of 2: %v", err)
		}
	}
}

type testSealer struct{ seals int }

func (ts *testSealer) Seal(label string, v any) ([]byte, error) {
	ts.seals++
	b, err := json.Marshal(v)
	return append([]byte("SEALED:"+label+":"), b...), err
}
func (ts *testSealer) Open(label string, b []byte, v any) error {
	p := "SEALED:" + label + ":"
	if !strings.HasPrefix(string(b), p) {
		return errors.New("bad seal")
	}
	return json.Unmarshal(b[len(p):], v)
}

// v0.9.16: with a sealer the A1 state is never written as plaintext, and an
// older plaintext file is still read (then sealed on the next save).
func TestA1StateSealed(t *testing.T) {
	s, _ := a1Service(t)
	ctx := context.Background()
	// an old plaintext ledger with one entry
	old := `{"pending":{},"ledger":[{"time":"` + s.Now().Format(time.RFC3339) + `","job_id":"1","worst_usd":5}]}`
	if err := os.WriteFile(s.statePath(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := &testSealer{}
	s.StateSealer = ts
	p, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.statePath())
	if !strings.HasPrefix(string(b), "SEALED:a1-state:") || strings.Contains(string(b), "\n \"pending\"") {
		t.Fatalf("state not sealed after save: %.60q", b)
	}
	if _, err := s.ConfirmSubmit(ctx, p.Token); err != nil {
		t.Fatalf("confirm via sealed state: %v", err)
	}
	st, err := s.loadState()
	if err != nil || len(st.Ledger) != 2 {
		t.Fatalf("sealed ledger: %v entries=%d (want the old one + this job)", err, len(st.Ledger))
	}
}
