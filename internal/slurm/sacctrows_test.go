package slurm

import (
	"strings"
	"testing"
)

// Rows recorded from Ursa Major on 2026-10-05 (sacct -n -P --noconvert -o
// SacctFields, SLURM_TIME_FORMAT=%s), anonymized: user, uid and job names.
const realRows = `1129|alice_ucr_edu|highmem|FAILED|None|1:0|ucrslurmcl-highmemnodeset-0|1|1791091216|1791091267|1791102485|11218|04:41:44|billing=32,cpu=32,mem=491520M,node=1||0|db-build
1129.batch|||FAILED||1:0|ucrslurmcl-highmemnodeset-0|1|1791091267|1791091267|1791102485|11218|04:41:44|cpu=32,mem=491520M,node=1|515399680000||batch
202|alice_ucr_edu|spot|CANCELLED by 50001|None|0:0|None assigned|1|1790625975|None|1790626008|0|00:00:00|billing=16,cpu=16,mem=126832M,node=1||15|lab-29-planner
1086|alice_ucr_edu|spot|OUT_OF_MEMORY|None|0:125|ucrslurmcl-spotnodeset-1|1|1791080641|1791080707|1791081483|776|1-02:03:04|billing=4,cpu=4,mem=15854M,node=1||0|db-verify
1086.batch|||OUT_OF_MEMORY||0:125|ucrslurmcl-spotnodeset-1|1|1791080707|1791080707|1791081483|776|1-02:03:04|cpu=4,mem=15854M,node=1|16624107520||batch
1086.extern|||COMPLETED||0:0|ucrslurmcl-spotnodeset-1|1|1791080707|1791080707|1791081483|776|00:00.001|billing=4,cpu=4,mem=15854M,node=1|1032K||extern
344|alice_ucr_edu|spot|COMPLETED|None|0:0|ucrslurmcl-spotnodeset-0|1|1790968180|1790968182|1790968182|0|00:00.059|billing=1,cpu=1,mem=7927M,node=1||0|toy|sweep
344.batch|||COMPLETED||0:0|ucrslurmcl-spotnodeset-0|1|1790968182|1790968182|1790968182|0|00:00.059|cpu=1,mem=7927M,node=1|4558848||batch
900|alice_ucr_edu|gpul4|RUNNING|None|0:0|ucrslurmcl-gpunodeset-0|1|1791100000|1791100060|Unknown|500|00:00:00|billing=8,cpu=8,gres/gpu=1,mem=63216M,node=1||2|gpu-run
`

func TestParseSacctRows(t *testing.T) {
	jobs, err := ParseSacctRows([]byte(realRows))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 5 {
		t.Fatalf("got %d jobs, want 5 (step rows are not jobs)", len(jobs))
	}
	by := map[int64]AcctJob{}
	for _, j := range jobs {
		by[j.JobID] = j
	}

	j := by[1129]
	if j.StateName() != "FAILED" || j.ExitCode.String() != "1" || j.Time.Elapsed != 11218 ||
		j.Time.Total.Float() != 4*3600+41*60+44 || j.TRES.Allocated.Get("cpu") != 32 || j.TRES.Allocated.Get("mem") != 491520 {
		t.Errorf("1129: %+v", j)
	}
	if j.MaxRSSBytes() != 515399680000 || len(j.Steps) != 1 {
		t.Errorf("1129 peak memory %d from %d steps", j.MaxRSSBytes(), len(j.Steps))
	}

	// never started: no node, no start time, cancelled by a uid
	j = by[202]
	if j.StateName() != "CANCELLED" || j.AllocationNodes != 0 || j.Time.Start != 0 || j.RestartCnt != 15 || j.Time.End != 1790626008 {
		t.Errorf("202: state %s nodes %d start %d restarts %d", j.StateName(), j.AllocationNodes, j.Time.Start, j.RestartCnt)
	}

	// signal exit, day-long CPU time, peak memory is the largest step (K suffix too)
	j = by[1086]
	if j.ExitCode.String() != "signal 125 (125)" {
		t.Errorf("1086 exit %q", j.ExitCode.String())
	}
	if j.Time.Total.Float() != 86400+2*3600+3*60+4 {
		t.Errorf("1086 cpu %v", j.Time.Total.Float())
	}
	if j.MaxRSSBytes() != 16624107520 || len(j.Steps) != 2 {
		t.Errorf("1086 peak %d steps %d", j.MaxRSSBytes(), len(j.Steps))
	}

	// a "|" in the job name stays in the name; millisecond CPU time
	j = by[344]
	if j.Name != "toy|sweep" || j.Time.Total.Microseconds != 59000 || j.Time.Total.Seconds != 0 {
		t.Errorf("344 name %q cpu %+v", j.Name, j.Time.Total)
	}

	// running: no end time; gres TRES keeps its name
	j = by[900]
	if j.StateName() != "RUNNING" || j.Time.End != 0 || j.TRES.Allocated.Get("gres/gpu") != 1 || j.RestartCnt != 2 {
		t.Errorf("900: %+v", j)
	}
}

func TestParseSacctRowsSignals(t *testing.T) {
	for in, want := range map[string]string{"0:9": "signal 9 (KILL)", "0:11": "signal 11 (SEGV)", "0:15": "signal 15 (TERM)",
		"0:53": "signal 53 (53)", "139:0": "139", "0:0": "0", "": ""} {
		if got := exitCodeOf(in).String(); got != want {
			t.Errorf("exit %q: got %q, want %q", in, got, want)
		}
	}
	if exitCodeOf("1:0").Status[0] != "ERROR" || exitCodeOf("0:0").Status[0] != "SUCCESS" || exitCodeOf("0:9").Status[0] != "SIGNALED" {
		t.Error("exit status names")
	}
}

func TestParseSacctRowsMemoryAndTime(t *testing.T) {
	for in, want := range map[string]int64{"4558848": 4558848, "1032K": 1032 * 1024, "2M": 2 << 20, "1.5G": 3 << 29, "": 0, "x": 0} {
		if got := memBytes(in); got != want {
			t.Errorf("mem %q: got %d, want %d", in, got, want)
		}
	}
	for in, want := range map[string]float64{"00:14.330": 14.33, "04:41:44": 16904, "1-02:03:04": 93784, "": 0, "bad": 0} {
		if got := cpuTimeOf(in).Float(); got < want-0.0005 || got > want+0.0005 {
			t.Errorf("cpu %q: got %v, want %v", in, got, want)
		}
	}
	if l := tresOf("billing=16,cpu=16,mem=126832M,node=1,gres/gpu=2"); l.Get("mem") != 126832 || l.Get("gres/gpu") != 2 || l.Get("cpu") != 16 {
		t.Errorf("tres %+v", l)
	}
}

func TestParseSacctRowsRejectsShortRows(t *testing.T) {
	if _, err := ParseSacctRows([]byte("1|alice|spot\n")); err == nil || !strings.Contains(err.Error(), "fields") {
		t.Errorf("short row accepted: %v", err)
	}
	if _, err := ParseSacctRows([]byte(strings.Repeat("x|", 16) + "name\n")); err == nil {
		t.Error("non-numeric job id accepted")
	}
	// a step whose job row is missing (window edge) is skipped, not an error
	jobs, err := ParseSacctRows([]byte("77.batch|||COMPLETED||0:0|n|1|1|1|1|1|00:01|cpu=1|100||batch\n"))
	if err != nil || len(jobs) != 0 {
		t.Errorf("orphan step: %v %v", jobs, err)
	}
}
