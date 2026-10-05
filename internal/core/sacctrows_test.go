package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/slurm"
)

// SPEC section 23: the plain-text accounting rows must give the list readers
// exactly what --json gave them for the same jobs. The fixture prints both from
// one recording, so any field the parser drops or misreads shows up here.
func TestSacctRowsMatchJSON(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	jc, _ := backend.SacctUser("alice_ucr_edu", "now-7days", "")
	b, err := s.Backend.Run(ctx, jc)
	if err != nil {
		t.Fatal(err)
	}
	var want slurm.AcctResponse
	if err := slurm.Decode(b, &want); err != nil {
		t.Fatal(err)
	}
	rc, _ := backend.SacctSummaryUser("alice_ucr_edu", "now-7days", "")
	got, err := s.acctRows(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want.Jobs) || len(got) == 0 {
		t.Fatalf("got %d jobs, want %d", len(got), len(want.Jobs))
	}
	for i, w := range want.Jobs {
		g := got[i]
		type view struct {
			ID, Nodes, Elapsed, Start, End, Submit, Restarts int64
			Name, User, Partition, State, Reason, Exit, Node string
			CPU                                              float64
			Cores, Mem, Peak                                 int64
		}
		v := func(j slurm.AcctJob) view {
			return view{j.JobID, j.AllocationNodes, j.Time.Elapsed, j.Time.Start, j.Time.End, j.Time.Submission, j.RestartCnt,
				j.Name, j.User, j.Partition, j.StateName(), j.State.Reason, j.ExitCode.String(), j.Nodes,
				round(j.Time.Total.Float(), 2), j.TRES.Allocated.Get("cpu"), j.TRES.Allocated.Get("mem"), j.MaxRSSBytes()}
		}
		gv, wv := v(g), v(w)
		// TotalCPU is printed to the millisecond, and to the second once it
		// passes an hour (sacct's own format); --json carries microseconds
		tol := 0.011
		if wv.CPU >= 3600 {
			tol = 1
		}
		if d := gv.CPU - wv.CPU; d > tol || d < -tol {
			t.Errorf("job %d cpu %.3f, want %.3f", w.JobID, gv.CPU, wv.CPU)
		}
		gv.CPU, wv.CPU = 0, 0
		if !reflect.DeepEqual(gv, wv) {
			gj, _ := json.Marshal(gv)
			wj, _ := json.Marshal(wv)
			t.Errorf("job %d\n got  %s\n want %s", w.JobID, gj, wj)
		}
	}
}

func TestSacctSummaryFields(t *testing.T) {
	if backend.SacctFieldsForTest() != slurm.SacctFields {
		t.Fatalf("backend prints %q, parser expects %q", backend.SacctFieldsForTest(), slurm.SacctFields)
	}
}

// List-type reads use the plain-text summary; one-job and job_ids reads keep --json.
func TestListReadsUseTheSummary(t *testing.T) {
	s, fx := newTestService(t, "R1", "R2")
	ctx := context.Background()
	calls := func(f func()) (summary, jsonAcct int) {
		fx.Calls = nil
		s.mu.Lock()
		s.cache = map[string]cacheEntry{}
		s.mu.Unlock()
		f()
		for _, c := range fx.Calls {
			switch {
			case strings.Contains(c, "sacct -n -P --noconvert"):
				summary++
			case strings.HasPrefix(c, "sacct --json"):
				jsonAcct++
			}
		}
		return
	}
	for name, f := range map[string]func(){
		"jobs_list":     func() { _, _ = s.JobsList(ctx, JobsListInput{}) },
		"jobs_list all": func() { _, _ = s.JobsList(ctx, JobsListInput{All: true}) },
		"my_usage":      func() { _, _ = s.Usage(ctx, UsageInput{}) },
		"usage_report":  func() { _, _ = s.Usage(ctx, UsageInput{All: true}) },
		"waste_report":  func() { _, _ = s.Waste(ctx, WasteInput{}) },
		"waste_all":     func() { _, _ = s.Waste(ctx, WasteInput{All: true}) },
		"health":        func() { _, _ = s.Health(ctx) },
	} {
		if sum, js := calls(f); sum != 1 || js != 0 {
			t.Errorf("%s: %d summary reads, %d --json accounting reads; want 1 and 0", name, sum, js)
		}
	}
	for name, f := range map[string]func(){
		"job_show": func() { _, _ = s.JobShow(ctx, JobShowInput{JobID: "236"}) },
		"job_ids":  func() { _, _ = s.JobsList(ctx, JobsListInput{JobIDs: []string{"236"}}) },
	} {
		if sum, js := calls(f); sum != 0 || js != 1 {
			t.Errorf("%s: %d summary reads, %d --json reads; want 0 and 1", name, sum, js)
		}
	}
}

// include_script on a job whose script Slurm did not store says so.
func TestIncludeScriptMissingSaysWhy(t *testing.T) {
	s, _ := newTestService(t)
	d, err := s.JobShow(context.Background(), JobShowInput{JobID: "261", IncludeScript: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Script != nil || !strings.Contains(d.ScriptNote, "did not store") {
		t.Errorf("script %v note %q", d.Script, d.ScriptNote)
	}
	d, _ = s.JobShow(context.Background(), JobShowInput{JobID: "261"})
	if d.ScriptNote != "" {
		t.Error("script note without include_script")
	}
	d, _ = s.JobShow(context.Background(), JobShowInput{JobID: "236", IncludeScript: true})
	if d.Script == nil || d.ScriptNote != "" {
		t.Errorf("stored script: %v note %q", d.Script, d.ScriptNote)
	}
}
