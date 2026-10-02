package rules

import (
	"os"
	"strings"
	"testing"
)

func logFixture(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/logs/job_" + id + ".log")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rulesOf(fs []Finding) []string {
	var r []string
	for _, f := range fs {
		r = append(r, f.Rule)
	}
	return r
}

func want(t *testing.T, fs []Finding, rule string) Finding {
	t.Helper()
	for _, f := range fs {
		if f.Rule == rule {
			if len(f.Evidence) == 0 || f.Suggestion == "" || f.Title == "" {
				t.Errorf("finding %s is missing evidence/suggestion/title: %+v", rule, f)
			}
			return f
		}
	}
	t.Fatalf("want rule %q, got %v", rule, rulesOf(fs))
	return Finding{}
}

// Real failed jobs from Ursa Major (anonymized fixtures).
func TestRealFailures(t *testing.T) {
	cases := []struct {
		job, state, exit, rule, evidence string
	}{
		{"236", "FAILED", "1", "python-import", "pandas"},
		{"203", "FAILED", "1", "python-import", "matplotlib"},
		{"203", "FAILED", "1", "download", "404"},
		{"229", "FAILED", "255", "container", "cuda-12.4-devel.sif"},
		{"195", "FAILED", "1", "syntax", "f-string"},
		{"45", "FAILED", "1", "bad-arguments", "--kv-transfer-config"},
		{"41", "FAILED", "1", "python-import", "No module named pip"},
		{"212", "FAILED", "139", "exit-signal", "SIGSEGV"},
	}
	for _, c := range cases {
		t.Run(c.job+"-"+c.rule, func(t *testing.T) {
			fs := Explain(Facts{State: c.state, ExitCode: c.exit, Log: logFixture(t, c.job), LogReadable: true})
			f := want(t, fs, c.rule)
			if !strings.Contains(strings.Join(f.Evidence, " "), c.evidence) {
				t.Errorf("evidence %v lacks %q", f.Evidence, c.evidence)
			}
		})
	}
}

func TestPipMissingGetsSpecificAdvice(t *testing.T) {
	fs := Explain(Facts{State: "FAILED", ExitCode: "1", Log: logFixture(t, "41"), LogReadable: true})
	f := want(t, fs, "python-import")
	if !strings.Contains(f.Suggestion, "pixi add pip") {
		t.Errorf("suggestion: %s", f.Suggestion)
	}
}

func TestTimeoutFromStateAndLog(t *testing.T) {
	fs := Explain(Facts{State: "TIMEOUT", LimitMin: 90, ElapsedSec: 5400, Log: logFixture(t, "253"), LogReadable: true})
	f := want(t, fs, "timeout")
	if !strings.Contains(strings.Join(f.Evidence, " "), "1h30m") {
		t.Errorf("evidence: %v", f.Evidence)
	}
	// FAILED state but the log shows Slurm's time-limit message
	fs = Explain(Facts{State: "FAILED", Log: "slurmstepd: error: *** JOB 9 ON n1 CANCELLED AT 2026 DUE TO TIME LIMIT ***\n"})
	want(t, fs, "timeout")
}

func TestOOM(t *testing.T) {
	fs := Explain(Facts{State: "OUT_OF_MEMORY", MemReqMB: 4000, MaxRSSMB: 3990, Partition: "standard"})
	f := want(t, fs, "oom")
	if !strings.Contains(f.Suggestion, "highmem") {
		t.Errorf("suggestion: %s", f.Suggestion)
	}
	// cgroup kill reported as FAILED with "Killed" in the log
	fs = Explain(Facts{State: "FAILED", ExitCode: "137", Log: "step 1\nKilled\n", MemReqMB: 1000, MaxRSSMB: 990})
	want(t, fs, "oom-log")
	want(t, fs, "memory-near-limit")
	for _, f := range fs {
		if f.Rule == "exit-signal" {
			t.Error("exit-signal should defer to the OOM findings")
		}
	}
	// already on highmem: different advice
	fs = Explain(Facts{State: "OUT_OF_MEMORY", Partition: "highmem"})
	if !strings.Contains(want(t, fs, "oom").Suggestion, "Already on highmem") {
		t.Error("highmem advice")
	}
}

func TestLowEfficiency(t *testing.T) {
	fs := Explain(Facts{State: "COMPLETED", CPUsAlloc: 22, ElapsedSec: 3600, CPUSeconds: 3600})
	want(t, fs, "low-cpu-efficiency")
	// short jobs are not judged
	fs = Explain(Facts{State: "COMPLETED", CPUsAlloc: 22, ElapsedSec: 60, CPUSeconds: 1})
	if len(fs) != 0 {
		t.Errorf("short job findings: %v", rulesOf(fs))
	}
}

func TestPendingAndCancelled(t *testing.T) {
	fs := Explain(Facts{State: "PENDING", Reason: "Resources"})
	f := want(t, fs, "pending")
	if !strings.Contains(f.Suggestion, "boot") {
		t.Errorf("Resources advice: %s", f.Suggestion)
	}
	m, _ := PendingReason("DependencyNeverSatisfied")
	if !strings.Contains(m, "never start") {
		t.Error(m)
	}
	m, _ = PendingReason("SomethingNew")
	if !strings.Contains(m, "SomethingNew") {
		t.Error("unknown reasons should be passed through")
	}
	fs = Explain(Facts{State: "CANCELLED", KilledBy: "bob"})
	if f := want(t, fs, "cancelled"); f.Severity != Info {
		t.Error("cancel is info")
	}
}

func TestNodeFailAndPreempt(t *testing.T) {
	want(t, Explain(Facts{State: "NODE_FAIL", RestartCnt: 1}), "node-fail")
	want(t, Explain(Facts{State: "PREEMPTED", Partition: "spot"}), "preempted")
}

func TestFallbackScriptError(t *testing.T) {
	fs := Explain(Facts{State: "FAILED", ExitCode: "2", Log: "all fine\nbye\n", LogReadable: true})
	want(t, fs, "script-error")
}

func TestCompletedHealthyHasNoFindings(t *testing.T) {
	if fs := Explain(Facts{State: "COMPLETED", ExitCode: "0", CPUsAlloc: 16, ElapsedSec: 3600, CPUSeconds: 16 * 3600 * 0.9}); len(fs) != 0 {
		t.Errorf("unexpected: %v", rulesOf(fs))
	}
}

func TestEachLogRuleFires(t *testing.T) {
	samples := map[string]string{
		"install-ladder":    "[ERROR] no install method produced a working environment",
		"container":         "FATAL:   While pulling image",
		"tool-crash":        "Segmentation fault (core dumped)",
		"glibc":             "/lib64/libc.so.6: version `GLIBC_2.34' not found",
		"module-missing":    "Lmod has detected the following error: The following module(s) are unknown: \"foo\"",
		"python-import":     "ModuleNotFoundError: No module named 'scipy'",
		"tls":               "ssl.SSLCertVerificationError: CERTIFICATE_VERIFY_FAILED",
		"download":          "urllib.error.HTTPError: HTTP Error 429: Too Many Requests",
		"bad-arguments":     "prog: error: unrecognized arguments: --fast",
		"api-change":        "AttributeError: 'Table' object has no attribute 'colname'",
		"syntax":            "  File \"x.py\", line 3\nSyntaxError: invalid syntax",
		"numerical":         "ZeroDivisionError: float division by zero",
		"missing-feature":   "ERROR: Unrecognized pair style 'reaxff' is part of the REAXFF package which is not enabled",
		"cuda":              "torch.cuda.OutOfMemoryError: CUDA out of memory.",
		"disk-full":         "OSError: [Errno 28] No space left on device",
		"permission":        "bash: ./run.sh: Permission denied",
		"command-not-found": "line 4: gmx_mpi: command not found",
		"python-error":      "Traceback (most recent call last):\n  File \"r.py\", line 222\nNameError: name 'd3_fp32_mlups' is not defined. Did you mean: 'fp32_mlups'?",
	}
	for _, r := range logRules {
		s, ok := samples[r.id]
		if !ok {
			t.Errorf("rule %s has no test sample", r.id)
			continue
		}
		fs := Explain(Facts{State: "FAILED", ExitCode: "1", Log: s + "\n", LogReadable: true})
		want(t, fs, r.id)
	}
}

func TestErrorsSortBeforeWarnings(t *testing.T) {
	fs := Explain(Facts{State: "TIMEOUT", CPUsAlloc: 22, ElapsedSec: 5400, CPUSeconds: 10})
	if len(fs) < 2 || fs[0].Severity != Error {
		t.Fatalf("order: %+v", fs)
	}
}

// Job 237 (live, 2026-10-01): the run finished, then the report script hit a
// NameError. Before v0.2.1 it fell through to "no known pattern".
func TestScriptOwnPythonError(t *testing.T) {
	log := "Generated verdict.json: all_pass=True\nTraceback (most recent call last):\n  File \"/x/analyze_and_report.py\", line 222, in <module>\nNameError: name 'd3_fp32_mlups' is not defined. Did you mean: 'fp32_mlups'?\n[STAGE] Failed (exit 1)\n"
	fs := Explain(Facts{State: "FAILED", ExitCode: "1", Log: log, LogReadable: true})
	f := want(t, fs, "python-error")
	if !strings.Contains(strings.Join(f.Evidence, " "), "d3_fp32_mlups") {
		t.Errorf("evidence: %v", f.Evidence)
	}
	for _, x := range fs {
		if x.Rule == "script-error" {
			t.Error("fell back to script-error")
		}
	}
	// a specific rule still wins over the generic one
	fs = Explain(Facts{State: "FAILED", ExitCode: "1", Log: "ModuleNotFoundError: No module named 'x'\n", LogReadable: true})
	if fs[0].Rule != "python-import" {
		t.Errorf("order: %v", rulesOf(fs))
	}
}

// Job 302 (live, 2026-10-01): GADGET-4's Makefile calls `python`, which the
// nodes do not have. make prints "Command not found" with a capital C, so the
// rule missed it and the job fell through to "no known pattern" (exit 5).
func TestMakeCommandNotFoundCapitalC(t *testing.T) {
	log := "[21:31:11] +4s cloned\nFAIL: build\nmake: python: Command not found\nmake: *** [buildsystem/Makefile.config:12: build/gadgetconfig.h] Error 127\ncat: build/gadgetconfig.h: No such file or directory\n"
	fs := Explain(Facts{State: "FAILED", ExitCode: "5", Log: log, LogReadable: true})
	f := want(t, fs, "command-not-found")
	if !strings.Contains(f.Suggestion, "PYTHON=python3") {
		t.Errorf("python advice missing: %q", f.Suggestion)
	}
	if hasRule(fs, "script-error") {
		t.Error("fell through to script-error")
	}
}

func TestCommandNotFoundAdvice(t *testing.T) {
	for cmd, wantSub := range map[string]string{
		"line 4: gmx_mpi": "modules_search gmx_mpi",
		"mpirun":          "module load openmpi",
		"module":          "#!/bin/bash -l",
		"nvcc":            "gpul4",
		"pip":             "python3",
	} {
		if got := commandNotFoundAdvice(cmd); !strings.Contains(got, wantSub) {
			t.Errorf("%s: %q lacks %q", cmd, got, wantSub)
		}
	}
}

func TestExit127WithoutLog(t *testing.T) {
	fs := Explain(Facts{State: "FAILED", ExitCode: "127", LogReadable: false})
	want(t, fs, "command-not-found")
}
