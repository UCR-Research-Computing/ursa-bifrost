package slurm

// Plain-text accounting rows (SPEC section 23). `sacct --json` spends nearly all
// of its time serializing: a week of one person's jobs took 19.6 s as JSON and
// 0.2 s as `sacct -n -P --noconvert -o <SacctFields>`. List-type reads (job
// lists, usage, waste, the failure rate) use these rows; per-job reads keep
// --json for the submit line, paths and full steps.

import (
	"fmt"
	"strconv"
	"strings"
)

// SacctFields is the -o list the rows are printed with. JobName is last so a
// "|" inside a name stays part of the name.
const SacctFields = "JobIDRaw,User,Partition,State,Reason,ExitCode,NodeList,NNodes,Submit,Start,End,ElapsedRaw,TotalCPU,AllocTRES,MaxRSS,Restarts,JobName"

const sacctFieldCount = 17

// ParseSacctRows turns `sacct -n -P --noconvert -o SacctFields` output, printed
// with SLURM_TIME_FORMAT=%s, into the AcctJob values `sacct --json` gives for the
// same jobs. Job rows carry the totals; each step row (<id>.batch, <id>.0)
// becomes one step holding only its peak memory, which is all the list readers
// take from steps (AcctJob.MaxRSSBytes).
func ParseSacctRows(b []byte) ([]AcctJob, error) {
	var jobs []AcctJob
	index := map[string]int{}
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "|", sacctFieldCount)
		if len(f) != sacctFieldCount {
			return nil, fmt.Errorf("sacct row %d: %d fields, want %d", n+1, len(f), sacctFieldCount)
		}
		id := f[0]
		if parent, _, isStep := strings.Cut(id, "."); isStep {
			i, ok := index[parent]
			if !ok {
				continue // a step whose job row fell outside the window
			}
			var st Step
			st.Step.ID = id
			st.Step.Name = f[16]
			if rss := memBytes(f[14]); rss > 0 {
				st.TRES.Requested.Max = TRESList{{Type: "mem", Count: rss}}
			}
			jobs[i].Steps = append(jobs[i].Steps, st)
			continue
		}
		jid, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("sacct row %d: job id %q", n+1, id)
		}
		var j AcctJob
		j.JobID = jid
		j.User, j.Partition, j.Nodes, j.Name = f[1], f[2], f[6], f[16]
		state, _, _ := strings.Cut(f[3], " ") // "CANCELLED by 50001"
		j.State.Current = []string{state}
		j.State.Reason = f[4]
		j.ExitCode = exitCodeOf(f[5])
		j.AllocationNodes = atoi(f[7])
		if j.Nodes == "None assigned" {
			j.AllocationNodes = 0 // never started: --json says 0 nodes
		}
		j.Time.Submission, j.Time.Start, j.Time.End = atoi(f[8]), atoi(f[9]), atoi(f[10])
		j.Time.Elapsed = atoi(f[11])
		j.Time.Total = cpuTimeOf(f[12])
		j.TRES.Allocated = tresOf(f[13])
		j.RestartCnt = atoi(f[15])
		index[id] = len(jobs)
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// atoi reads a plain integer; "None", "Unknown" and "" are 0 (unset).
func atoi(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// exitCodeOf turns "1:0" (return code : signal) into the structured exit code.
func exitCodeOf(s string) ExitCode {
	var e ExitCode
	rc, sig, ok := strings.Cut(s, ":")
	if !ok {
		return e
	}
	if n := atoi(sig); n != 0 {
		e.Status = []string{"SIGNALED"}
		e.Signal.ID = Num{Set: true, Number: float64(n)}
		e.Signal.Name = signalName(n)
		return e
	}
	n := atoi(rc)
	e.ReturnCode = Num{Set: true, Number: float64(n)}
	e.Status = []string{"SUCCESS"}
	if n != 0 {
		e.Status = []string{"ERROR"}
	}
	return e
}

// signalName is the name --json gives a signal: the Linux name for 1-31, else
// the number (Slurm reports 53 and 125 that way).
func signalName(n int64) string {
	names := []string{"", "HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "BUS", "FPE", "KILL", "USR1",
		"SEGV", "USR2", "PIPE", "ALRM", "TERM", "STKFLT", "CHLD", "CONT", "STOP", "TSTP", "TTIN",
		"TTOU", "URG", "XCPU", "XFSZ", "VTALRM", "PROF", "WINCH", "IO", "PWR", "SYS"}
	if n > 0 && int(n) < len(names) {
		return names[n]
	}
	return strconv.FormatInt(n, 10)
}

// cpuTimeOf reads sacct's TotalCPU: [D-][HH:]MM:SS[.mmm].
func cpuTimeOf(s string) CPUTime {
	s = strings.TrimSpace(s)
	if s == "" {
		return CPUTime{}
	}
	var days int64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, s = atoi(d), rest
	}
	parts := strings.Split(s, ":")
	var secs float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return CPUTime{}
		}
		secs = secs*60 + v
	}
	whole := int64(secs)
	return CPUTime{Seconds: days*86400 + whole, Microseconds: int64((secs-float64(whole))*1e6 + 0.5)}
}

// tresOf reads AllocTRES ("billing=16,cpu=16,mem=126832M,node=1,gres/gpu=1");
// mem is in MB, as in --json.
func tresOf(s string) TRESList {
	var l TRESList
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		t := TRES{Type: k}
		if typ, name, ok := strings.Cut(k, "/"); ok {
			t.Type, t.Name = typ, name
		}
		if t.Type == "mem" {
			t.Count = memBytes(v) / (1024 * 1024)
		} else {
			t.Count = atoi(v)
		}
		l = append(l, t)
	}
	return l
}

// memBytes reads a size: plain bytes (--noconvert) or a K/M/G/T suffix.
func memBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mul := map[byte]float64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}
	if m, ok := mul[s[len(s)-1]]; ok {
		v, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0
		}
		return int64(v * m)
	}
	return atoi(s)
}
