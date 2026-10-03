// Package rules is the deterministic diagnosis engine behind job_explain.
//
// Each rule looks at facts from the accounting record and, optionally, the log
// tail, and returns a finding with the evidence that fired it. No model runs here.
// The log rules are seeded from deep-research's FAILURE_CLASSES (lab.py) and real
// failed jobs on Ursa Major; every rule has a test in rules_test.go.
package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Facts is what the rules look at. Built by core from sacct (+ squeue) and the log.
type Facts struct {
	State         string // COMPLETED, FAILED, TIMEOUT, OUT_OF_MEMORY, CANCELLED, NODE_FAIL, PENDING, RUNNING, PREEMPTED
	Reason        string // squeue/sacct state reason
	ExitCode      string // "1", "signal 9 (KILL)"
	Partition     string
	ElapsedSec    int64
	LimitMin      int64 // 0 = none/infinite
	CPUsAlloc     int64
	CPUSeconds    float64 // total user+system CPU time
	MemReqMB      int64   // requested/allocated memory (MB)
	MaxRSSMB      int64   // peak RSS over steps (MB)
	GPUsAlloc     int64
	RestartCnt    int64
	KilledBy      string // kill_request_user
	Log           string // log tail (may be empty)
	LogReadable   bool
	SpotPartition bool
}

// Severity of a finding.
const (
	Error   = "error"
	Warning = "warning"
	Info    = "info"
)

// Finding is one diagnosis.
type Finding struct {
	Rule       string   `json:"rule"`
	Severity   string   `json:"severity"`
	Title      string   `json:"title"`
	Evidence   []string `json:"evidence"`
	Suggestion string   `json:"suggestion"`
}

type logRule struct {
	id, title, suggestion string
	severity              string
	re                    *regexp.Regexp
}

// Log rules: order is priority (first, most specific).
var logRules = []logRule{
	{
		id: "install-ladder", severity: Error, title: "No install method produced a working environment",
		re:         regexp.MustCompile(`\[ERROR\] no install method produced a working environment`),
		suggestion: "The software setup failed, not the analysis: try another package source, an existing module, or a container.",
	},
	{
		id: "container", severity: Error, title: "Container image could not be pulled or opened",
		re:         regexp.MustCompile(`FATAL:\s+While (?:making image|pulling)|unable to parse image name`),
		suggestion: "Local .sif files are run directly (`apptainer exec /apps/containers/x.sif ...`), not pulled; for registry images use docker://name:tag. List /apps/containers for prebuilt images.",
	},
	{
		id: "tool-crash", severity: Error, title: "A program crashed (assertion or segfault)",
		re:         regexp.MustCompile(`Assertion '.*' failed|Segmentation fault|core dumped|signal 11|\*\*\* Process received signal`),
		suggestion: "Usually an input it did not expect (name or format), or code built for AVX-512 on the AVX2-only standard/spot nodes (e2). Check inputs; try computehigh (Intel, AVX-512) to rule out the CPU.",
	},
	{
		id: "glibc", severity: Error, title: "Binary needs a newer glibc than the nodes have",
		re:         regexp.MustCompile("version `GLIBC_2\\.\\d+' not found"),
		suggestion: "Nodes run Rocky 8 (glibc 2.28). Use the module, a container, or an older build (conda-forge keeps old versions).",
	},
	{
		id: "module-missing", severity: Error, title: "A module could not be loaded",
		re:         regexp.MustCompile(`Unable to locate a modulefile|The following module\(s\) are unknown|module: command not found|Lmod has detected the following error`),
		suggestion: "Check the exact name with modules_search; MPI-built packages appear only after `module load openmpi`. In a batch script use `#!/bin/bash -l` or source the job header so `module` exists.",
	},
	{
		id: "python-import", severity: Error, title: "Python package not installed in the environment",
		re:         regexp.MustCompile(`ModuleNotFoundError: No module named '([^']+)'|ImportError: No module named ([\w.]+)|python[\d.]*: No module named ([\w.]+)`),
		suggestion: "Load python-sci (CPU) or python-ml (PyTorch/GPU), or create a uv/Pixi environment in the job. Check that the environment is activated in the same shell that runs python.",
	},
	{
		id: "tls", severity: Error, title: "HTTPS certificate verification failed",
		re:         regexp.MustCompile(`CERTIFICATE_VERIFY_FAILED|unable to get local issuer certificate|SSL certificate problem`),
		suggestion: "Export SSL_CERT_FILE (and REQUESTS_CA_BUNDLE) to certifi's bundle before downloading: export SSL_CERT_FILE=$(python -c 'import certifi; print(certifi.where())'). Never disable certificate checks.",
	},
	{
		id: "download", severity: Error, title: "A download failed",
		re:         regexp.MustCompile(`HTTP Error 40[34]|HTTP Error 429|returned error: 40[34]|urlopen error|Connection (?:refused|reset|timed out)|Read timed out|Temporary failure in name resolution`),
		suggestion: "Check the URL still exists (404) or the site blocks scripts (403/429): send a User-Agent, retry with backoff, or fetch elsewhere and stage the data in $SCRATCH.",
	},
	{
		id: "bad-arguments", severity: Error, title: "A program rejected its command-line arguments",
		re:         regexp.MustCompile(`error: unrecognized arguments?:|unrecognized option|invalid option --|Unknown option`),
		suggestion: "The installed version does not know this flag (often a newer option). Check `<program> --help` on the cluster or pin the version that has it.",
	},
	{
		id: "api-change", severity: Error, title: "A library's API differs from what the code expects",
		re:         regexp.MustCompile(`AttributeError: '\w+' object has no attribute|unexpected keyword argument|(?:astropy|pandas|table)[^\n]*\n(?:[^\n]*\n){0,12}KeyError: '`),
		suggestion: "Inspect what the installed version provides (dir(obj), table.colnames) and use that, or pin the version the code was written for.",
	},
	{
		id: "syntax", severity: Error, title: "Syntax error in the script",
		re:         regexp.MustCompile(`SyntaxError: |unterminated string literal|IndentationError: |syntax error near unexpected token`),
		suggestion: "Fix the syntax error at the line shown; generated code often loses backslashes or braces in f-strings.",
	},
	{
		id: "numerical", severity: Error, title: "The computation blew up numerically",
		re:         regexp.MustCompile(`ZeroDivisionError|FloatingPointError|nan detected|diverg|RuntimeWarning: (?:overflow|invalid value)`),
		suggestion: "Check stability limits (time step, CFL, relaxation) and guard divisions; do not hide it with try/except.",
	},
	{
		id: "missing-feature", severity: Error, title: "The installed build lacks a needed feature",
		re:         regexp.MustCompile(`lacks? (?:the )?\w+ (?:support|package)|Unrecognized (?:pair|fix|atom) style|Package \w+ is not installed|not compiled with`),
		suggestion: "Use a build that has it: another module variant (e.g. -cuda), conda-forge, or a container.",
	},
	{
		id: "cuda", severity: Error, title: "GPU/CUDA problem",
		re:         regexp.MustCompile(`CUDA error|no CUDA-capable device|CUDA out of memory|torch\.cuda\.OutOfMemoryError|NVIDIA-SMI has failed`),
		suggestion: "GPU memory is 24 GB per L4: reduce batch size or model size. If no device is found, request --gres=gpu:1 on gpul4 and use `apptainer exec --nv` for containers.",
	},
	{
		id: "disk-full", severity: Error, title: "Out of disk space or quota",
		re:         regexp.MustCompile(`No space left on device|Disk quota exceeded`),
		suggestion: "Write large outputs to $SCRATCH (/scratch/$USER) and temp files to $TMPDIR; clean up old runs.",
	},
	{
		id: "permission", severity: Error, title: "Permission denied",
		re:         regexp.MustCompile(`Permission denied`),
		suggestion: "Check file permissions and that paths are under your home or scratch; scripts need chmod +x if executed directly.",
	},
	{
		id: "command-not-found", severity: Error, title: "A command was not found",
		re:         regexp.MustCompile(`(?mi)(\S+): command not found\s*$`),
		suggestion: "Load the module that provides it (modules_search) or fix PATH in the job script.",
	},
	{
		id: "python-error", severity: Error, title: "Python error in the script's own code",
		re:         regexp.MustCompile(`(?m)^(NameError|TypeError|ValueError|IndexError|KeyError|UnboundLocalError|FileNotFoundError|AssertionError|RuntimeError): .+$`),
		suggestion: "A bug in the script (not the cluster): read the traceback just above this line for the file and line number, fix it there, and rerun. If earlier stages finished, rerun only the failed step instead of the whole job.",
	},
}

// Explain runs every rule and returns the findings, most important first.
func Explain(f Facts) []Finding {
	var out []Finding
	add := func(x Finding) { out = append(out, x) }

	st := f.State
	switch st {
	case "OUT_OF_MEMORY":
		add(Finding{Rule: "oom", Severity: Error, Title: "Job ran out of memory",
			Evidence:   []string{"state OUT_OF_MEMORY", memEvidence(f)},
			Suggestion: memSuggestion(f)})
	case "TIMEOUT":
		ev := []string{"state TIMEOUT"}
		if f.LimitMin > 0 {
			ev = append(ev, fmt.Sprintf("time limit %s, ran %s", minutes(f.LimitMin), dur(f.ElapsedSec)))
		}
		add(Finding{Rule: "timeout", Severity: Error, Title: "Job hit its time limit", Evidence: ev,
			Suggestion: "Raise --time (partitions have no maximum here), or checkpoint and resubmit. If it should have finished sooner, check it was not stuck waiting (I/O, a download, a lock)."})
	case "NODE_FAIL":
		add(Finding{Rule: "node-fail", Severity: Error, Title: "The node failed under the job",
			Evidence:   []string{"state NODE_FAIL", fmt.Sprintf("restart count %d", f.RestartCnt)},
			Suggestion: "Usually not the user's fault (cloud node lost). Resubmit; add --requeue for long jobs."})
	case "PREEMPTED":
		add(Finding{Rule: "preempted", Severity: Warning, Title: "Job was preempted",
			Evidence:   []string{"state PREEMPTED", "partition " + f.Partition},
			Suggestion: "Spot nodes can be reclaimed with ~30 s notice. Add --requeue and checkpointing, or use standard for work that cannot restart."})
	case "CANCELLED":
		ev := []string{"state CANCELLED"}
		if f.KilledBy != "" {
			ev = append(ev, "cancelled by "+f.KilledBy)
		}
		add(Finding{Rule: "cancelled", Severity: Info, Title: "Job was cancelled", Evidence: ev,
			Suggestion: "Cancelled by a person or a tool (scancel), not a failure of the job itself."})
	}

	// Memory near the limit even without OOM state (cgroup kill shows as FAILED).
	if st != "OUT_OF_MEMORY" && f.MemReqMB > 0 && f.MaxRSSMB > 0 && float64(f.MaxRSSMB) >= 0.95*float64(f.MemReqMB) {
		sev := Warning
		if st == "FAILED" {
			sev = Error
		}
		add(Finding{Rule: "memory-near-limit", Severity: sev, Title: "Peak memory reached the allocation",
			Evidence: []string{memEvidence(f)}, Suggestion: memSuggestion(f)})
	}

	if f.Log != "" {
		tail := f.Log
		if len(tail) > 40000 {
			tail = tail[len(tail)-40000:]
		}
		if m := regexp.MustCompile(`oom-kill|Out Of Memory|out of memory|MemoryError|(?m)Killed\s*$`).FindString(tail); m != "" && st != "OUT_OF_MEMORY" {
			add(Finding{Rule: "oom-log", Severity: Error, Title: "Out-of-memory kill in the log",
				Evidence: []string{"log: " + clip(lineOf(tail, m), 200), memEvidence(f)}, Suggestion: memSuggestion(f)})
		}
		if st != "TIMEOUT" && regexp.MustCompile(`DUE TO TIME LIMIT`).MatchString(tail) {
			add(Finding{Rule: "timeout", Severity: Error, Title: "Job hit its time limit",
				Evidence:   []string{"log: " + clip(lineOf(tail, "DUE TO TIME LIMIT"), 200)},
				Suggestion: "Raise --time or checkpoint and resubmit."})
		}
		matched := 0
		for _, r := range logRules {
			locs := r.re.FindAllStringIndex(tail, -1)
			if len(locs) == 0 {
				continue
			}
			last := locs[len(locs)-1]
			ev := []string{"log: " + clip(lineAt(tail, last[0]), 240)}
			if len(locs) > 1 {
				ev = append(ev, fmt.Sprintf("%d matching lines", len(locs)))
			}
			sug := r.suggestion
			if r.id == "command-not-found" {
				if m := r.re.FindStringSubmatch(tail[last[0]:]); len(m) > 1 {
					sug = commandNotFoundAdvice(strings.TrimSuffix(m[1], ":"))
				}
			}
			if r.id == "python-import" {
				if m := r.re.FindStringSubmatch(tail[last[0]:]); len(m) > 1 {
					pkg := firstNonEmpty(m[1:]...)
					sug = fmt.Sprintf("Package %q is missing. ", pkg) + sug
					if pkg == "pip" {
						sug = "The environment has no pip (Pixi/conda envs do not include it by default): add pip to the environment (pixi add pip) or install with uv/pixi instead of python -m pip."
					}
				}
			}
			add(Finding{Rule: r.id, Severity: r.severity, Title: r.title, Evidence: ev, Suggestion: sug})
			matched++
			if matched >= 4 {
				break
			}
		}
	}

	// Exit codes that mean a signal killed the program (128+N), when no log rule
	// already explained it.
	if st == "FAILED" || st == "OUT_OF_MEMORY" {
		if sig, ok := signalExits[f.ExitCode]; ok && !hasRule(out, "tool-crash", "oom", "oom-log", "memory-near-limit") {
			add(Finding{Rule: "exit-signal", Severity: Error, Title: sig.title,
				Evidence: []string{"exit code " + f.ExitCode + " = " + sig.meaning}, Suggestion: sig.fix})
		}
	}

	// Efficiency (only meaningful once the job ran a while).
	if f.CPUsAlloc >= 4 && f.ElapsedSec >= 600 && f.CPUSeconds > 0 {
		eff := f.CPUSeconds / (float64(f.CPUsAlloc) * float64(f.ElapsedSec))
		if eff < 0.2 {
			add(Finding{Rule: "low-cpu-efficiency", Severity: Warning, Title: "Most allocated cores sat idle",
				Evidence:   []string{fmt.Sprintf("CPU efficiency %.0f%% (%d cores x %s)", eff*100, f.CPUsAlloc, dur(f.ElapsedSec))},
				Suggestion: "The program used few cores: enable its threading/MPI options (-ntomp, OMP_NUM_THREADS, srun) or ask for fewer cores (on a shared partition only the cores held bill; on a whole-node partition idle cores still bill)."})
		}
	}

	// Exit 127 is the shell's "command not found", even when the log is gone.
	if st == "FAILED" && f.ExitCode == "127" && !hasRule(out, "command-not-found") {
		add(Finding{Rule: "command-not-found", Severity: Error, Title: "A command was not found",
			Evidence:   []string{"exit code 127 = the shell could not find a command"},
			Suggestion: "Check the first \"command not found\" line in the log (job_log_tail). Load the module that provides the program, or use its full path. There is no bare `python` on the nodes: use python3, or load python-sci / python-ml."})
	}

	if st == "FAILED" && len(out) == 0 {
		ev := []string{"state FAILED", "exit code " + f.ExitCode}
		if !f.LogReadable {
			ev = append(ev, "log not readable")
		}
		add(Finding{Rule: "script-error", Severity: Error, Title: "The job script exited with an error",
			Evidence:   ev,
			Suggestion: "No known pattern matched. Read the end of the log (job_log_tail) for the first error message."})
	}

	if st == "PENDING" {
		add(pendingFinding(f.Reason))
	}

	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].Severity) < rank(out[j].Severity) })
	return dedupe(out)
}

// PendingReason decodes a Slurm pending reason into plain words.
func PendingReason(reason string) (meaning, advice string) {
	switch reason {
	case "Priority":
		return "Other jobs with higher priority are ahead of it.", "Wait, or ask for fewer nodes/less time so backfill can start it earlier."
	case "Resources":
		return "It is next in line, waiting for nodes to free up or (on cloud partitions) to boot.", "Cold partitions take ~90 s-5 min to boot a node; GCP stockouts can delay longer. Try another partition if it waits more than 15 minutes."
	case "BeginTime":
		return "It was submitted with a start time in the future (--begin).", "Nothing to do; it will start at the requested time."
	case "Dependency":
		return "It waits for another job (--dependency).", "Check the job it depends on."
	case "DependencyNeverSatisfied":
		return "The job it depends on failed, so this one can never start.", "Cancel it and resubmit without the dependency, or fix the earlier job."
	case "JobHeldUser":
		return "Held by its owner (scontrol hold).", "Release it with `scontrol release <id>`."
	case "JobHeldAdmin":
		return "Held by an administrator.", "Ask Research Computing."
	case "ReqNodeNotAvail", "ReqNodeNotAvail, UnavailableNodes":
		return "A node it needs is down, drained or reserved.", "Remove --nodelist/--exclude constraints or pick another partition."
	case "PartitionDown", "PartitionInactive":
		return "The partition is not accepting jobs.", "Use another partition and tell Research Computing."
	case "PartitionTimeLimit":
		return "The requested time is over the partition's limit.", "Lower --time."
	case "PartitionNodeLimit":
		return "More nodes requested than the partition allows.", "Lower -N."
	case "QOSMaxJobsPerUserLimit", "AssocMaxJobsLimit", "QOSMaxSubmitJobPerUserLimit":
		return "You are at the per-user job limit.", "It starts when one of your running jobs ends."
	case "QOSMaxCpuPerUserLimit", "AssocGrpCpuLimit", "QOSGrpCpuLimit":
		return "You (or your group) are at the CPU limit.", "It starts when running jobs free cores."
	case "launch failed requeued held":
		return "The node failed to launch the job and Slurm held it.", "Release it (`scontrol release <id>`); if it repeats, report the node to Research Computing."
	case "BadConstraints":
		return "The request cannot be met by any node (features, memory or CPUs per node).", "Check the request against the partition sizes (partitions tool)."
	case "None", "":
		return "No reason recorded yet (it was just submitted).", "Check again in a minute."
	}
	return "Slurm reason: " + reason, "Look up the reason in the Slurm squeue documentation or ask Research Computing."
}

type signalExit struct{ title, meaning, fix string }

// signalExits maps shell exit codes 128+N (and Slurm's "signal N") to causes.
var signalExits = map[string]signalExit{
	"139":              {"A program crashed (segmentation fault)", "128+11, SIGSEGV", "Usually an input it did not expect, a bug, or code built for AVX-512 running on the AVX2-only standard/spot nodes (try computehigh). Run it under gdb or with core dumps to find where."},
	"signal 11 (SEGV)": {"A program crashed (segmentation fault)", "SIGSEGV", "Usually an input it did not expect, a bug, or AVX-512 code on the AVX2-only standard/spot nodes (try computehigh)."},
	"137":              {"A program was killed (SIGKILL)", "128+9, SIGKILL", "Most often the out-of-memory killer: check peak memory and request more (--mem) or use highmem."},
	"signal 9 (KILL)":  {"The job was killed (SIGKILL)", "SIGKILL", "Most often out of memory or a time limit; check job_show for peak memory and limits."},
	"134":              {"A program aborted (SIGABRT)", "128+6, SIGABRT", "An assertion or a library detected a fatal error; read the lines just before the end of the log."},
	"signal 6 (ABRT)":  {"A program aborted (SIGABRT)", "SIGABRT", "An assertion or a library detected a fatal error; read the end of the log."},
	"135":              {"A program crashed (bus error)", "128+7, SIGBUS", "Often a file truncated while being read (memory-mapped) or a full disk."},
	"143":              {"The job was terminated (SIGTERM)", "128+15, SIGTERM", "Sent by scancel, a time limit or node shutdown (spot reclaim)."},
}

func hasRule(fs []Finding, ids ...string) bool {
	for _, f := range fs {
		for _, id := range ids {
			if f.Rule == id {
				return true
			}
		}
	}
	return false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func pendingFinding(reason string) Finding {
	m, a := PendingReason(reason)
	return Finding{Rule: "pending", Severity: Info, Title: "Job is waiting to start",
		Evidence: []string{"reason " + reason, m}, Suggestion: a}
}

func memEvidence(f Facts) string {
	if f.MaxRSSMB > 0 && f.MemReqMB > 0 {
		return fmt.Sprintf("peak memory %s of %s allocated (%.0f%%)", mb(f.MaxRSSMB), mb(f.MemReqMB), 100*float64(f.MaxRSSMB)/float64(f.MemReqMB))
	}
	if f.MemReqMB > 0 {
		return fmt.Sprintf("%s allocated; peak not recorded", mb(f.MemReqMB))
	}
	return "memory use not recorded"
}

func memSuggestion(f Facts) string {
	s := "Request more memory (--mem) or use the highmem partition (~497 GB per node)."
	if f.Partition == "highmem" {
		s = "Already on highmem: reduce the working set (chunking, streaming, smaller batches) or split the work across nodes."
	}
	return s
}

func rank(s string) int {
	switch s {
	case Error:
		return 0
	case Warning:
		return 1
	}
	return 2
}

func dedupe(in []Finding) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, f := range in {
		if seen[f.Rule] {
			continue
		}
		seen[f.Rule] = true
		out = append(out, f)
	}
	return out
}

func lineAt(s string, i int) string {
	start := strings.LastIndexByte(s[:i], '\n') + 1
	end := strings.IndexByte(s[i:], '\n')
	if end < 0 {
		return strings.TrimSpace(s[start:])
	}
	return strings.TrimSpace(s[start : i+end])
}

func lineOf(s, sub string) string {
	i := strings.LastIndex(s, sub)
	if i < 0 {
		return sub
	}
	return lineAt(s, i)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func mb(v int64) string {
	if v >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(v)/1024)
	}
	return fmt.Sprintf("%d MB", v)
}

func minutes(m int64) string { return dur(m * 60) }

func dur(sec int64) string {
	h, m, s := sec/3600, (sec%3600)/60, sec%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// commandNotFoundAdvice tailors the fix to the missing command. Ursa Major
// nodes have python3 but no bare python, which breaks many Makefiles and
// build scripts (GADGET-4's build calls `python`).
func commandNotFoundAdvice(cmd string) string {
	base := cmd
	if i := strings.LastIndex(base, ":"); i >= 0 {
		base = strings.TrimSpace(base[i+1:])
	}
	switch base {
	case "python", "pip":
		return fmt.Sprintf("%q does not exist on the nodes (only python3, and pip inside an environment). Load python-sci or python-ml first, call python3, or for a Makefile pass PYTHON=python3 (make PYTHON=python3).", base)
	case "module":
		return "The `module` command is missing in this shell: start the script with #!/bin/bash -l, or source /apps/docs/templates/job-header.sh."
	case "mpirun", "mpiexec", "mpicc", "mpicxx", "mpif90":
		return fmt.Sprintf("%q comes from an MPI module: add `module load openmpi` before it (or use srun to launch).", base)
	case "nvcc", "nvidia-smi":
		return fmt.Sprintf("%q is only on the GPU nodes (partition gpul4, --gres=gpu:1); nvcc also needs `module load cuda`.", base)
	}
	return fmt.Sprintf("%q is not on PATH in the job: load the module that provides it (modules_search %s) or use its full path.", base, base)
}
