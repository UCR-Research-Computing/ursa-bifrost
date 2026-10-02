package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shared partitions (v0.8.0): Slurm reports OverSubscribe per partition; on a shared
// partition a script must ask for cores and is priced by its share of the node.

const sharedNow = "computehigh|NO\ngpul4|EXCLUSIVE\nhighmem|EXCLUSIVE\nnvmescratch|NO\nspot|FORCE:4\nstandard*|NO\n"

func sharedService(t *testing.T) *Service {
	t.Helper()
	s, fx := a1Service(t)
	fx.Sharing = sharedNow
	return s
}

func script(lines ...string) string {
	return "#!/bin/bash\n" + strings.Join(lines, "\n") + "\nmodule load python-sci\npython3 x.py\n"
}

func TestPartitionSharingParsesSinfo(t *testing.T) {
	s := sharedService(t)
	sh := s.partitionSharing(context.Background())
	want := map[string]bool{"computehigh": true, "gpul4": false, "highmem": false, "nvmescratch": true, "spot": true, "standard": true}
	for k, v := range want {
		if sh[k] != v {
			t.Errorf("%s shared = %v, want %v (all: %v)", k, sh[k], v, sh)
		}
	}
	// today's cluster (fixture default): every partition EXCLUSIVE
	s2, _ := a1Service(t)
	for k, v := range s2.partitionSharing(context.Background()) {
		if v {
			t.Errorf("%s reported shared on an exclusive cluster", k)
		}
	}
}

func TestSharingReadFailureMeansExclusive(t *testing.T) {
	s, fx := a1Service(t)
	fx.Fail = map[string]string{"sinfo": "slurm_load_partitions: Unable to contact slurm controller"}
	if s.partitionShared(context.Background(), "computehigh") {
		t.Fatal("a failed read must not count as shared")
	}
}

func TestScriptWithoutCoresIsRefusedOnSharedPartition(t *testing.T) {
	s := sharedService(t)
	ctx := context.Background()
	sc, err := s.ScriptCheck(ctx, script("#SBATCH -p computehigh", "#SBATCH -t 30"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.OK || !strings.Contains(issues(sc), "computehigh shares nodes between jobs: this script asks for no cores, so Slurm gives it 1 core and about 4 GB") {
		t.Fatalf("no-core script on a shared partition must be an error that reads as a sentence: %s", issues(sc))
	}
	// what the job will get, as numbers: 1 core, not "the node"
	if sc.Cores == nil || sc.Cores.CoresPerNod != 1 || sc.Cores.NodeShare >= 0.1 {
		t.Fatalf("no-core request should be 1 core, a small share: %+v", sc.Cores)
	}
	// job_submit refuses it and submits nothing
	if _, err := s.PrepareSubmit(ctx, SubmitInput{Script: script("#SBATCH -p computehigh", "#SBATCH -t 30")}); err == nil || !strings.Contains(err.Error(), "shares nodes") {
		t.Fatalf("job_submit must refuse a no-core script on a shared partition: %v", err)
	}
	// every way of asking for cores is accepted
	for _, ask := range []string{"#SBATCH --cpus-per-task=4", "#SBATCH -c 4", "#SBATCH --ntasks-per-node=8", "#SBATCH -n 2", "#SBATCH --exclusive"} {
		sc, _ := s.ScriptCheck(ctx, script("#SBATCH -p computehigh", "#SBATCH -t 30", ask))
		if strings.Contains(issues(sc), "shares nodes") {
			t.Errorf("%q should satisfy the core rule: %s", ask, issues(sc))
		}
	}
}

func TestWholeNodePartitionsNeedNoCoreRequest(t *testing.T) {
	s := sharedService(t)
	for _, p := range []string{"highmem", "gpul4"} {
		sc, _ := s.ScriptCheck(context.Background(), script("#SBATCH -p "+p, "#SBATCH -t 30", "#SBATCH --gres=gpu:1"))
		if strings.Contains(issues(sc), "shares nodes") {
			t.Errorf("%s gives whole nodes; no core request needed: %s", p, issues(sc))
		}
		if sc.Cores == nil || sc.Cores.NodeShare != 1 {
			t.Errorf("%s must be priced as a whole node: %+v", p, sc.Cores)
		}
	}
}

func TestNoCoreRuleOnTodaysExclusiveCluster(t *testing.T) {
	s, _ := a1Service(t)
	sc, _ := s.ScriptCheck(context.Background(), script("#SBATCH -p computehigh", "#SBATCH -t 30"))
	if strings.Contains(issues(sc), "shares nodes") {
		t.Fatalf("exclusive partitions must not demand a core request: %s", issues(sc))
	}
	if sc.EstCostUSD == nil || *sc.EstCostUSD != 0.94 { // 1.87 x 0.5 h, whole node
		t.Fatalf("exclusive cost must stay whole-node: %v", sc.EstCostUSD)
	}
}

func TestSharedCostIsTheShareOfTheNode(t *testing.T) {
	s := sharedService(t)
	ctx := context.Background()
	cost := func(lines ...string) float64 {
		t.Helper()
		sc, err := s.ScriptCheck(ctx, script(append([]string{"#SBATCH -p computehigh", "#SBATCH -t 60"}, lines...)...))
		if err != nil || sc.EstCostUSD == nil {
			t.Fatalf("%v %v", err, sc)
		}
		return *sc.EstCostUSD
	}
	near := func(got, want float64, what string) {
		t.Helper()
		if math.Abs(got-want) > 0.011 {
			t.Errorf("%s: $%.2f, want $%.2f", what, got, want)
		}
	}
	near(cost("#SBATCH --cpus-per-task=11"), 1.87/2, "11 of 22 cores")
	near(cost("#SBATCH -c 2"), 1.87*2/22, "2 cores")
	near(cost("#SBATCH --ntasks-per-node=4", "#SBATCH --cpus-per-task=5"), 1.87*20/22, "4 ranks x 5 threads")
	near(cost("#SBATCH -c 2", "#SBATCH --mem=42G"), 1.87*42*1024/(85*1024), "memory share wins")
	near(cost("#SBATCH -c 2", "#SBATCH --mem-per-cpu=21G"), 1.87*42/85, "mem-per-cpu x cores")
	near(cost("#SBATCH --exclusive"), 1.87, "--exclusive pays the node")
	near(cost("#SBATCH -c 64"), 1.87, "more cores than the node: capped at one node")
	// the reported core count is capped at the node too (64 asked, 22 exist)
	sc, _ := s.ScriptCheck(ctx, script("#SBATCH -p computehigh", "#SBATCH -t 60", "#SBATCH -c 64"))
	if sc.Cores == nil || sc.Cores.CoresPerNod != 22 {
		t.Errorf("cores per node must be capped at 22: %+v", sc.Cores)
	}
	near(cost("#SBATCH -c 2", "#SBATCH --mem=999G"), 1.87, "more memory than the node: capped at one node")
}

func TestSharedWorstCaseDrivesCaps(t *testing.T) {
	s := sharedService(t)
	ctx := context.Background()
	// 4 nodes x 24 h whole-node computehigh = $179.52, far over the $25 job cap;
	// at 2 of 22 cores per node it is $16.32 and passes
	small := script("#SBATCH -p computehigh", "#SBATCH -N 4", "#SBATCH -t 24:00:00", "#SBATCH -c 2")
	p, err := s.PrepareSubmit(ctx, SubmitInput{Script: small})
	if err != nil {
		t.Fatalf("a 2-core job must be priced by its share: %v", err)
	}
	if math.Abs(p.WorstCaseUSD-16.32) > 0.011 {
		t.Errorf("worst case $%.2f, want $16.32", p.WorstCaseUSD)
	}
	big := script("#SBATCH -p computehigh", "#SBATCH -N 4", "#SBATCH -t 24:00:00", "#SBATCH --exclusive")
	if _, err := s.PrepareSubmit(ctx, SubmitInput{Script: big}); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("--exclusive must be priced as whole nodes and hit the cap: %v", err)
	}
}

func TestOverrideOntoSharedPartitionIsChecked(t *testing.T) {
	s := sharedService(t)
	ctx := context.Background()
	// written for highmem (whole node, no core request needed), sent to standard
	in := SubmitInput{Script: script("#SBATCH -p highmem", "#SBATCH -t 30"), Partition: "standard"}
	if _, err := s.PrepareSubmit(ctx, in); err == nil || !strings.Contains(err.Error(), "standard shares nodes") {
		t.Fatalf("override onto a shared partition must apply the core rule: %v", err)
	}
	// and the other way: a shared-partition script without cores sent to highmem is fine
	in = SubmitInput{Script: script("#SBATCH -p standard", "#SBATCH -t 30"), Partition: "highmem"}
	if _, err := s.PrepareSubmit(ctx, in); err != nil && strings.Contains(err.Error(), "shares nodes") {
		t.Fatalf("highmem gives whole nodes; the core rule must not fire: %v", err)
	}
}

func TestAllCoreIdiomsWarnOnSharedPartition(t *testing.T) {
	s := sharedService(t)
	sc, _ := s.ScriptCheck(context.Background(), "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -t 30\n#SBATCH -c 4\nmodule load python-sci\npython3 -c 'import os; os.cpu_count()'\n")
	if !strings.Contains(issues(sc), "counts every core on the node (os.cpu_count())") || !strings.Contains(issues(sc), "holds 4") {
		t.Errorf("os.cpu_count() on a shared partition should warn: %s", issues(sc))
	}
	sc, _ = s.ScriptCheck(context.Background(), "#!/bin/bash\n#SBATCH -p computehigh\n#SBATCH -t 30\n#SBATCH -c 4\nmake -j\"$SLURM_CPUS_ON_NODE\"\n")
	if strings.Contains(issues(sc), "counts every core") {
		t.Errorf("$SLURM_CPUS_ON_NODE is the job's own count: %s", issues(sc))
	}
}

func TestClusterStatusNotesSayWhichPartitionsShare(t *testing.T) {
	s := sharedService(t)
	cs, err := s.ClusterStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Join(cs.Notes, " ")
	if !strings.Contains(n, "Shared partitions (computehigh, nvmescratch, spot, standard)") || !strings.Contains(n, "Whole-node partitions (gpul4, highmem)") {
		t.Errorf("notes: %s", n)
	}
	s2, _ := a1Service(t)
	cs2, _ := s2.ClusterStatus(context.Background())
	if !strings.Contains(strings.Join(cs2.Notes, " "), "Partitions are whole-node (exclusive)") {
		t.Errorf("exclusive notes: %v", cs2.Notes)
	}
}

func TestAllocShare(t *testing.T) {
	s, _ := a1Service(t)
	cat, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	near := func(got, want float64, what string) {
		t.Helper()
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("%s: %v, want %v", what, got, want)
		}
	}
	near(allocShare(cat, "computehigh", 1, 22, 87054), 1, "whole node (exclusive allocation)")
	// 2 x DefMemPerCPU (3957 MB) is a hair over 2/22 of the catalog's 85 GB node
	near(allocShare(cat, "computehigh", 1, 2, 7914), 7914.0/(85*1024), "2 cores, default memory")
	near(allocShare(cat, "computehigh", 1, 2, 4000), 2.0/22, "2 cores, little memory: core share")
	near(allocShare(cat, "computehigh", 1, 2, 43520), 43520.0/(85*1024), "memory share wins")
	near(allocShare(cat, "computehigh", 2, 32, 0), 16.0/22, "per node over 2 nodes")
	near(allocShare(cat, "computehigh", 1, 0, 0), 1, "nothing known: whole node")
	near(allocShare(cat, "nosuch", 1, 2, 0), 1, "unknown partition: whole node")
	near(allocShare(nil, "computehigh", 1, 2, 0), 1, "no catalog: whole node")
	near(allocShare(cat, "computehigh", 1, 99, 0), 1, "more than a node: capped")
}

func TestTodaysUsageCostsAreUnchanged(t *testing.T) {
	// every recorded job held whole nodes (exclusive cluster), so the share is 1 and
	// usage costs equal list price x nodes x hours exactly as before v0.8.0
	s, _ := a1Service(t)
	u, err := s.Usage(context.Background(), UsageInput{Since: "now-30days", GroupBy: "partition"})
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := s.Catalog(context.Background())
	var want float64
	for _, r := range u.Rows {
		p, ok := s.price(cat, r.Key)
		if ok {
			want += p * r.NodeHours
		}
	}
	if math.Abs(u.Total.CostUSD-round(want, 2)) > 0.02 {
		t.Errorf("usage cost %v, whole-node formula %v", u.Total.CostUSD, round(want, 2))
	}
}

// sharedFixtureDir copies testdata and rewrites job 93 (computehigh, 22 cores) to a
// shared-partition allocation of 2 cores and 4000 MB, as Slurm records it then.
func sharedFixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ents, err := os.ReadDir("../../testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join("../../testdata", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "sacct_jobs.json")
	var doc map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range doc["jobs"].([]any) {
		jm := j.(map[string]any)
		if jm["job_id"].(float64) != 93 {
			continue
		}
		for _, x := range jm["tres"].(map[string]any)["allocated"].([]any) {
			xm := x.(map[string]any)
			switch xm["type"] {
			case "cpu":
				xm["count"] = 2
			case "mem":
				xm["count"] = 4000
			}
		}
		found = true
	}
	if !found {
		t.Fatal("job 93 not in fixture")
	}
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSpentCostsUseTheShareHeld(t *testing.T) {
	ctx := context.Background()
	usage := func(dir string) (*Usage, []JobSummary) {
		t.Helper()
		s, fx := a1Service(t)
		fx.Dir = dir
		u, err := s.Usage(ctx, UsageInput{Since: "now-30days", GroupBy: "partition"})
		if err != nil {
			t.Fatal(err)
		}
		js, err := s.JobsList(ctx, JobsListInput{Since: "now-30days", Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		return u, js
	}
	whole, wholeJobs := usage("../../testdata")
	shared, sharedJobs := usage(sharedFixtureDir(t))
	row := func(u *Usage, k string) UsageRow {
		for _, r := range u.Rows {
			if r.Key == k {
				return r
			}
		}
		t.Fatalf("no %s row", k)
		return UsageRow{}
	}
	w, sh := row(whole, "computehigh"), row(shared, "computehigh")
	if sh.CostUSD >= w.CostUSD {
		t.Fatalf("a 2-core allocation must cost less than the whole node: %v vs %v", sh.CostUSD, w.CostUSD)
	}
	cost := func(js []JobSummary) float64 {
		for _, j := range js {
			if j.JobID == "93" {
				return j.CostUSD
			}
		}
		t.Fatal("job 93 missing from jobs_list")
		return 0
	}
	if c, cw := cost(sharedJobs), cost(wholeJobs); cw <= 0 || math.Abs(c-round(cw*2/22, 2)) > 0.011 {
		t.Errorf("job 93 cost %v, want 2/22 of %v", c, cw)
	}
}

func TestWasteUsesTheShareHeld(t *testing.T) {
	ctx := context.Background()
	waste := func(dir string) *WasteItem {
		t.Helper()
		s, fx := a1Service(t)
		fx.Dir = dir
		// job 93 used 50762 CPU-s over 4970 s: about 10 cores busy, so it counts as
		// low-CPU only against 22 cores; at 2 allocated cores it is fully busy. To
		// look at billed node-hours on the same item, use a threshold no job passes.
		rep, err := s.Waste(ctx, WasteInput{All: true, Since: "now-30days", CPUThreshold: 100000, MinNodeHours: 0.0001})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rep.Items {
			if rep.Items[i].JobID == "93" {
				return &rep.Items[i]
			}
		}
		t.Fatal("job 93 not in waste report")
		return nil
	}
	w, sh := waste("../../testdata"), waste(sharedFixtureDir(t))
	if w.NodeHours <= 0 || math.Abs(sh.NodeHours-round(w.NodeHours*2/22, 2)) > 0.011 {
		t.Errorf("job 93 node-hours: shared %v, whole %v (want 2/22)", sh.NodeHours, w.NodeHours)
	}
}

func TestCommentedModuleLoadIsNotChecked(t *testing.T) {
	s := sharedService(t)
	sc, err := s.ScriptCheck(context.Background(), script("#SBATCH -p computehigh", "#SBATCH -t 30", "#SBATCH -n 22",
		"module load openmpi gromacs",
		"srun gmx_mpi mdrun -deffnm md",
		"# GPU build: module load gromacs/<version>-cuda on the gpul4 partition",
		"   # module load nosuchmodule"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(issues(sc), "not found") {
		t.Fatalf("commented module loads must be ignored: %s", issues(sc))
	}
	for _, m := range sc.Modules {
		if strings.Contains(m, "cuda") || m == "on" || m == "nosuchmodule" {
			t.Fatalf("modules = %v: a commented load was counted", sc.Modules)
		}
	}
}

func TestCommentsDoNotTriggerRunTimeChecks(t *testing.T) {
	s := sharedService(t)
	ctx := context.Background()
	// a commented-out GPU alternative and a "python" mention in a comment
	sc, _ := s.ScriptCheck(ctx, script("#SBATCH -p standard", "#SBATCH -t 30", "#SBATCH -c 4",
		"module load apptainer",
		"apptainer exec /apps/containers/r.sif Rscript x.R",
		"# GPU: apptainer exec --nv /apps/containers/pytorch.sif python train.py",
		"Rscript y.R   # not python"))
	if strings.Contains(issues(sc), "--nv") || strings.Contains(issues(sc), "calls python") {
		t.Fatalf("comments must not trigger run-time checks: %s", issues(sc))
	}
	// the same lines as code still warn
	sc, _ = s.ScriptCheck(ctx, script("#SBATCH -p standard", "#SBATCH -t 30", "#SBATCH -c 4",
		"module load apptainer", "apptainer exec --nv /apps/containers/pytorch.sif nvidia-smi"))
	if !strings.Contains(issues(sc), "--nv") {
		t.Fatalf("a real --nv must still warn: %s", issues(sc))
	}
	sc, _ = s.ScriptCheck(ctx, "#!/bin/bash\n#SBATCH -p standard\n#SBATCH -t 30\n#SBATCH -c 4\n# module load python-sci (not yet)\npython3 x.py  # main step\n")
	if !strings.Contains(issues(sc), "calls python") {
		t.Fatalf("a real bare python must still warn: %s", issues(sc))
	}
}
