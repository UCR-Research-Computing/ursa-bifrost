// Package slurm parses the JSON that Slurm 25.11 prints with --json
// (data_parser v0.0.44): squeue, sacct, sinfo and scontrol show nodes.
//
// Only the fields ursa-bifrost uses are declared; unknown fields are ignored, so
// newer Slurm versions keep parsing as long as these names stay.
package slurm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Num is Slurm's optional number: {"set":true,"infinite":false,"number":5}.
// Some fields are plain numbers instead; both forms are accepted.
type Num struct {
	Set      bool
	Infinite bool
	Number   float64
}

// UnmarshalJSON accepts the wrapped object or a bare number.
func (n *Num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*n = Num{}
		return nil
	}
	if b[0] == '{' {
		var w struct {
			Set      bool    `json:"set"`
			Infinite bool    `json:"infinite"`
			Number   float64 `json:"number"`
		}
		if err := json.Unmarshal(b, &w); err != nil {
			return err
		}
		*n = Num{Set: w.Set, Infinite: w.Infinite, Number: w.Number}
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("slurm number: %w", err)
	}
	*n = Num{Set: true, Number: f}
	return nil
}

// Int returns the value, or 0 when unset or infinite.
func (n Num) Int() int64 {
	if !n.Set || n.Infinite {
		return 0
	}
	return int64(n.Number)
}

// Valid reports a set, finite value.
func (n Num) Valid() bool { return n.Set && !n.Infinite }

// ExitCode is Slurm's structured exit code.
type ExitCode struct {
	Status     []string `json:"status"`
	ReturnCode Num      `json:"return_code"`
	Signal     struct {
		ID   Num    `json:"id"`
		Name string `json:"name"`
	} `json:"signal"`
}

// String renders "1", "signal 9 (KILL)" or "".
func (e ExitCode) String() string {
	if e.Signal.ID.Valid() {
		s := fmt.Sprintf("signal %d", e.Signal.ID.Int())
		if e.Signal.Name != "" {
			s += " (" + e.Signal.Name + ")"
		}
		return s
	}
	if e.ReturnCode.Valid() {
		return fmt.Sprintf("%d", e.ReturnCode.Int())
	}
	return ""
}

// Message is an entry of the errors/warnings arrays.
type Message struct {
	Description string `json:"description"`
	Source      string `json:"source"`
	Error       string `json:"error"`
}

// Meta is the meta block every --json response carries.
type Meta struct {
	Plugin struct {
		DataParser string `json:"data_parser"`
	} `json:"plugin"`
	Slurm struct {
		Release string `json:"release"`
		Cluster string `json:"cluster"`
	} `json:"slurm"`
}

// QueueJob is one job from `squeue --json` (or `scontrol show job --json`).
type QueueJob struct {
	JobID          int64    `json:"job_id"`
	ArrayJobID     Num      `json:"array_job_id"`
	ArrayTaskID    Num      `json:"array_task_id"`
	Name           string   `json:"name"`
	UserName       string   `json:"user_name"`
	Account        string   `json:"account"`
	Partition      string   `json:"partition"`
	JobState       []string `json:"job_state"`
	StateReason    string   `json:"state_reason"`
	Nodes          string   `json:"nodes"`
	NodeCount      Num      `json:"node_count"`
	CPUs           Num      `json:"cpus"`
	MemoryPerCPU   Num      `json:"memory_per_cpu"`
	MemoryPerNode  Num      `json:"memory_per_node"`
	TimeLimit      Num      `json:"time_limit"` // minutes
	SubmitTime     Num      `json:"submit_time"`
	StartTime      Num      `json:"start_time"`
	EndTime        Num      `json:"end_time"`
	StdoutExpanded string   `json:"stdout_expanded"`
	StderrExpanded string   `json:"stderr_expanded"`
	TresReqStr     string   `json:"tres_req_str"`
	TresAllocStr   string   `json:"tres_alloc_str"`
	TresPerNode    string   `json:"tres_per_node"`
	SubmitLine     string   `json:"submit_line"`
	Comment        string   `json:"comment"`
	RestartCnt     Num      `json:"restart_cnt"`
	Flags          []string `json:"flags"`
	ExitCode       ExitCode `json:"exit_code"`
	Command        string   `json:"command"`
	WorkDir        string   `json:"current_working_directory"`
}

// State is the primary job state (first entry).
func (j QueueJob) State() string { return first(j.JobState) }

// QueueResponse is `squeue --json`.
type QueueResponse struct {
	Jobs     []QueueJob `json:"jobs"`
	Meta     Meta       `json:"meta"`
	Errors   []Message  `json:"errors"`
	Warnings []Message  `json:"warnings"`
}

// TRES is one trackable resource (cpu, mem, node, gres/gpu, ...).
type TRES struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// Key is "cpu", "mem" or "gres/gpu".
func (t TRES) Key() string {
	if t.Name != "" {
		return t.Type + "/" + t.Name
	}
	return t.Type
}

// TRESList is a list of TRES with lookups.
type TRESList []TRES

// Get returns the count for a key ("cpu", "mem", "node", "gres/gpu") or 0.
func (l TRESList) Get(key string) int64 {
	for _, t := range l {
		if t.Key() == key {
			return t.Count
		}
	}
	return 0
}

// CPUTime is the {seconds, microseconds} pair sacct uses for CPU times.
type CPUTime struct {
	Seconds      int64 `json:"seconds"`
	Microseconds int64 `json:"microseconds"`
}

// Float returns seconds as a float.
func (c CPUTime) Float() float64 { return float64(c.Seconds) + float64(c.Microseconds)/1e6 }

// Step is one job step from sacct.
type Step struct {
	Step struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"step"`
	State    []string `json:"state"`
	ExitCode ExitCode `json:"exit_code"`
	Time     struct {
		Elapsed int64   `json:"elapsed"`
		Total   CPUTime `json:"total"`
	} `json:"time"`
	Nodes struct {
		Count int64 `json:"count"`
	} `json:"nodes"`
	TRES struct {
		Requested struct {
			Max TRESList `json:"max"`
		} `json:"requested"`
	} `json:"tres"`
}

// MaxRSSBytes is the step's peak memory (bytes), as gathered by jobacct_gather.
func (s Step) MaxRSSBytes() int64 { return s.TRES.Requested.Max.Get("mem") }

// AcctJob is one job from `sacct --json`.
type AcctJob struct {
	JobID     int64  `json:"job_id"`
	Name      string `json:"name"`
	User      string `json:"user"`
	Account   string `json:"account"`
	Partition string `json:"partition"`
	State     struct {
		Current []string `json:"current"`
		Reason  string   `json:"reason"`
	} `json:"state"`
	ExitCode        ExitCode `json:"exit_code"`
	DerivedExitCode ExitCode `json:"derived_exit_code"`
	Nodes           string   `json:"nodes"`
	AllocationNodes int64    `json:"allocation_nodes"`
	FailedNode      string   `json:"failed_node"`
	KillRequestUser string   `json:"kill_request_user"`
	RestartCnt      int64    `json:"restart_cnt"`
	Time            struct {
		Elapsed    int64   `json:"elapsed"`
		Start      int64   `json:"start"`
		End        int64   `json:"end"`
		Submission int64   `json:"submission"`
		Limit      Num     `json:"limit"` // minutes
		Total      CPUTime `json:"total"`
	} `json:"time"`
	TRES struct {
		Allocated TRESList `json:"allocated"`
		Requested TRESList `json:"requested"`
	} `json:"tres"`
	Required struct {
		CPUs          int64 `json:"CPUs"`
		MemoryPerCPU  Num   `json:"memory_per_cpu"`
		MemoryPerNode Num   `json:"memory_per_node"`
	} `json:"required"`
	Steps          []Step `json:"steps"`
	StdoutExpanded string `json:"stdout_expanded"`
	StderrExpanded string `json:"stderr_expanded"`
	WorkDir        string `json:"working_directory"`
	SubmitLine     string `json:"submit_line"`
	Script         string `json:"script"`
	Comment        struct {
		Job string `json:"job"`
	} `json:"comment"`
}

// StateName is the primary state (first entry).
func (j AcctJob) StateName() string { return first(j.State.Current) }

// MaxRSSBytes is the largest peak memory over all steps.
func (j AcctJob) MaxRSSBytes() int64 {
	var m int64
	for _, s := range j.Steps {
		if v := s.MaxRSSBytes(); v > m {
			m = v
		}
	}
	return m
}

// AcctResponse is `sacct --json`.
type AcctResponse struct {
	Jobs     []AcctJob `json:"jobs"`
	Meta     Meta      `json:"meta"`
	Errors   []Message `json:"errors"`
	Warnings []Message `json:"warnings"`
}

// SinfoEntry is one row of `sinfo --json` (a partition x node-state group).
type SinfoEntry struct {
	Partition struct {
		Name string `json:"name"`
	} `json:"partition"`
	Node struct {
		State []string `json:"state"`
	} `json:"node"`
	Nodes struct {
		Allocated int64    `json:"allocated"`
		Idle      int64    `json:"idle"`
		Other     int64    `json:"other"`
		Total     int64    `json:"total"`
		Names     []string `json:"nodes"`
	} `json:"nodes"`
	CPUs struct {
		Total int64 `json:"total"`
	} `json:"cpus"`
	Memory struct {
		Minimum int64 `json:"minimum"`
	} `json:"memory"`
	Gres struct {
		Total string `json:"total"`
	} `json:"gres"`
	Reason struct {
		Description string `json:"description"`
	} `json:"reason"`
}

// SinfoResponse is `sinfo --json`.
type SinfoResponse struct {
	Sinfo    []SinfoEntry `json:"sinfo"`
	Meta     Meta         `json:"meta"`
	Errors   []Message    `json:"errors"`
	Warnings []Message    `json:"warnings"`
}

// Node is one node from `scontrol show nodes --json`.
type Node struct {
	Name         string   `json:"name"`
	State        []string `json:"state"`
	CPULoad      int64    `json:"cpu_load"` // load average x 100
	CPUs         int64    `json:"cpus"`
	AllocCPUs    int64    `json:"alloc_cpus"`
	RealMemory   int64    `json:"real_memory"`
	AllocMemory  int64    `json:"alloc_memory"`
	Partitions   []string `json:"partitions"`
	Reason       string   `json:"reason"`
	Gres         string   `json:"gres"`
	LastBusy     Num      `json:"last_busy"`
	BootTime     Num      `json:"boot_time"`
	ReasonSetBy  string   `json:"reason_set_by_user"`
	ReasonAt     Num      `json:"reason_changed_at"`
	InstanceType string   `json:"instance_type"`
}

// HasState reports whether the node carries a state flag (IDLE, POWERED_DOWN, ...).
func (n Node) HasState(s string) bool { return contains(n.State, s) }

// PoweredUp reports a cloud node whose VM exists (and bills), including one that
// is booting. slurm-gcp marks powered-off nodes POWERED_DOWN; the POWER_DOWN flag
// alone only means "power off once free".
func (n Node) PoweredUp() bool { return !n.HasState("POWERED_DOWN") }

// Booting is a cloud node still starting. slurm-gcp nodes report
// NOT_RESPONDING (and sometimes POWER_DOWN as the next step) while POWERING_UP;
// that is normal for the first minutes and is not a failure.
func (n Node) Booting() bool { return n.HasState("POWERING_UP") }

// Broken is a node that failed: DOWN, FAIL or DRAIN, or NOT_RESPONDING when it
// is not merely still booting.
func (n Node) Broken() bool {
	if n.HasState("DOWN") || n.HasState("FAIL") {
		return true
	}
	return n.HasState("NOT_RESPONDING") && !n.Booting()
}

// NodesResponse is `scontrol show nodes --json`.
type NodesResponse struct {
	Nodes    []Node    `json:"nodes"`
	Meta     Meta      `json:"meta"`
	Errors   []Message `json:"errors"`
	Warnings []Message `json:"warnings"`
}

// Decode parses data into v and returns Slurm's own error messages as an error.
func Decode(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parsing Slurm JSON: %w", err)
	}
	var env struct {
		Errors []Message `json:"errors"`
	}
	_ = json.Unmarshal(data, &env)
	if len(env.Errors) > 0 {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, strings.TrimSpace(e.Description+" "+e.Error))
		}
		return fmt.Errorf("slurm: %s", strings.Join(msgs, "; "))
	}
	return nil
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
