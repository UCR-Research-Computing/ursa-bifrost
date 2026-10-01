package slurm

import (
	"encoding/json"
	"os"
	"testing"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := Decode(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func TestNumForms(t *testing.T) {
	var v struct {
		A Num `json:"a"`
		B Num `json:"b"`
		C Num `json:"c"`
		D Num `json:"d"`
	}
	in := `{"a":{"set":true,"infinite":false,"number":5},"b":{"set":false,"infinite":false,"number":0},"c":{"set":true,"infinite":true,"number":0},"d":7}`
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	if v.A.Int() != 5 || !v.A.Valid() {
		t.Errorf("a: %+v", v.A)
	}
	if v.B.Valid() || v.B.Int() != 0 {
		t.Errorf("b: %+v", v.B)
	}
	if v.C.Valid() || !v.C.Infinite {
		t.Errorf("c: %+v", v.C)
	}
	if v.D.Int() != 7 {
		t.Errorf("d: %+v", v.D)
	}
}

func TestSqueueFixture(t *testing.T) {
	var q QueueResponse
	load(t, "squeue.json", &q)
	if len(q.Jobs) != 1 {
		t.Fatalf("jobs: %d", len(q.Jobs))
	}
	j := q.Jobs[0]
	if j.JobID != 260 || j.State() != "RUNNING" || j.Partition != "computehigh" || j.CPUs.Int() != 22 {
		t.Errorf("unexpected job: %+v", j)
	}
	if j.TimeLimit.Int() != 1440 || !j.StartTime.Valid() {
		t.Errorf("time fields: limit %v start %v", j.TimeLimit, j.StartTime)
	}
	if q.Meta.Slurm.Release != "25.11.4" {
		t.Errorf("release %q", q.Meta.Slurm.Release)
	}
}

func TestSacctFixture(t *testing.T) {
	var a AcctResponse
	load(t, "sacct_jobs.json", &a)
	byID := map[int64]AcctJob{}
	for _, j := range a.Jobs {
		byID[j.JobID] = j
	}
	j := byID[236]
	if j.StateName() != "FAILED" || j.ExitCode.String() != "1" || j.Partition != "gpul4" {
		t.Errorf("236: state %s exit %s part %s", j.StateName(), j.ExitCode.String(), j.Partition)
	}
	if j.TRES.Allocated.Get("cpu") != 8 || j.TRES.Allocated.Get("mem") != 63216 {
		t.Errorf("236 tres: %+v", j.TRES.Allocated)
	}
	if j.MaxRSSBytes() == 0 {
		t.Error("236: expected a recorded peak memory")
	}
	if to := byID[253]; to.StateName() != "TIMEOUT" || to.Time.Limit.Int() != 90 {
		t.Errorf("253: %s limit %d", to.StateName(), to.Time.Limit.Int())
	}
	c := byID[93]
	if c.StateName() != "CANCELLED" {
		t.Errorf("93: %s", c.StateName())
	}
	if s := c.Steps[0].ExitCode.String(); s != "signal 15 (TERM)" {
		t.Errorf("93 batch step exit: %q", s)
	}
}

func TestSinfoAndNodes(t *testing.T) {
	var s SinfoResponse
	load(t, "sinfo.json", &s)
	if len(s.Sinfo) == 0 || s.Sinfo[0].Partition.Name == "" {
		t.Fatal("sinfo empty")
	}
	var n NodesResponse
	load(t, "nodes.json", &n)
	up := 0
	for _, x := range n.Nodes {
		if x.PoweredUp() {
			up++
		}
	}
	if up != 1 {
		t.Errorf("powered-up nodes = %d, want 1 (the warm worker)", up)
	}
	if !n.Nodes[0].HasState("ALLOCATED") || !n.Nodes[0].PoweredUp() {
		t.Errorf("node 0: %v", n.Nodes[0].State)
	}
}

func TestDecodeReportsSlurmErrors(t *testing.T) {
	var q QueueResponse
	err := Decode([]byte(`{"jobs":[],"errors":[{"description":"Invalid job id specified","error":"","source":""}]}`), &q)
	if err == nil || err.Error() != "slurm: Invalid job id specified" {
		t.Fatalf("got %v", err)
	}
}
