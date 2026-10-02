package core

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/slurm"
)

// ---- cluster status -------------------------------------------------------------

// PartitionStatus is one partition's live state.
type PartitionStatus struct {
	Name        string   `json:"name"`
	Default     bool     `json:"default,omitempty"`
	NodesTotal  int      `json:"nodes_total"`
	PoweredDown int      `json:"powered_down"` // cloud nodes with no VM (cost nothing)
	PoweredUp   int      `json:"powered_up"`   // VMs that exist and bill
	Allocated   int      `json:"allocated"`
	IdleUp      int      `json:"idle_powered_up"` // billing but no job
	Booting     int      `json:"booting"`
	Down        int      `json:"down_or_drained"`
	Running     int      `json:"jobs_running"`
	Pending     int      `json:"jobs_pending"`
	USDPerHour  *float64 `json:"usd_per_node_hour,omitempty"`
	BurnPerHour *float64 `json:"current_usd_per_hour,omitempty"`
	Problems    []string `json:"problems,omitempty"`
}

// ClusterStatus is the cluster_status answer.
type ClusterStatus struct {
	Cluster       string            `json:"cluster"`
	SlurmVersion  string            `json:"slurm_version"`
	Partitions    []PartitionStatus `json:"partitions"`
	JobsRunning   int               `json:"jobs_running"`
	JobsPending   int               `json:"jobs_pending"`
	NodesUp       int               `json:"nodes_powered_up"`
	BurnPerHour   *float64          `json:"current_usd_per_hour,omitempty"`
	IdleBilling   []string          `json:"idle_billing_nodes,omitempty"`
	Notes         []string          `json:"notes"`
	PendingByWhy  map[string]int    `json:"pending_by_reason,omitempty"`
	ProblemsTotal int               `json:"problem_nodes"`
}

// ClusterStatus reads node and queue state.
func (s *Service) ClusterStatus(ctx context.Context) (*ClusterStatus, error) {
	var nr slurm.NodesResponse
	b, err := s.run(ctx, backend.Nodes())
	if err != nil {
		return nil, err
	}
	if err := slurm.Decode(b, &nr); err != nil {
		return nil, err
	}
	var q slurm.QueueResponse
	b, err = s.run(ctx, backend.SqueueAll())
	if err != nil {
		return nil, err
	}
	if err := slurm.Decode(b, &q); err != nil {
		return nil, err
	}
	cat, _ := s.Catalog(ctx)
	byName := map[string]*PartitionStatus{}
	get := func(name string) *PartitionStatus {
		p := byName[name]
		if p == nil {
			p = &PartitionStatus{Name: name}
			byName[name] = p
		}
		return p
	}
	cs := &ClusterStatus{Cluster: s.Cfg.Cluster, SlurmVersion: nr.Meta.Slurm.Release, PendingByWhy: map[string]int{}}
	for _, n := range nr.Nodes {
		for _, pn := range n.Partitions {
			p := get(pn)
			p.NodesTotal++
			switch {
			case n.Broken() || n.HasState("DRAIN"):
				p.Down++
				msg := n.Name + ": " + strings.Join(n.State, "+")
				if n.Reason != "" {
					msg += " (" + n.Reason + ")"
				}
				p.Problems = append(p.Problems, msg)
			}
			if !n.PoweredUp() {
				p.PoweredDown++
				continue
			}
			p.PoweredUp++
			switch {
			case n.HasState("POWERING_UP"):
				p.Booting++
			case n.HasState("ALLOCATED") || n.HasState("MIXED") || n.HasState("COMPLETING"):
				p.Allocated++
			case n.HasState("IDLE"):
				p.IdleUp++
				cs.IdleBilling = append(cs.IdleBilling, n.Name+" ("+pn+")")
			}
		}
	}
	for _, j := range q.Jobs {
		p := get(j.Partition)
		switch j.State() {
		case "RUNNING", "COMPLETING", "CONFIGURING":
			p.Running++
			cs.JobsRunning++
		case "PENDING":
			p.Pending++
			cs.JobsPending++
			cs.PendingByWhy[j.StateReason]++
		}
	}
	var burn float64
	priced := false
	for _, p := range byName {
		if cat != nil {
			if cp, ok := cat.Partition(p.Name); ok {
				p.Default = cp.Default
			}
		}
		if v, ok := s.price(cat, p.Name); ok && s.Cfg.ShowCost {
			v := v
			p.USDPerHour = &v
			b := round(v*float64(p.PoweredUp), 2)
			p.BurnPerHour = &b
			burn += b
			priced = true
		}
		cs.NodesUp += p.PoweredUp
		cs.ProblemsTotal += p.Down
		cs.Partitions = append(cs.Partitions, *p)
	}
	sort.Slice(cs.Partitions, func(i, j int) bool { return cs.Partitions[i].Name < cs.Partitions[j].Name })
	if priced {
		b := round(burn, 2)
		cs.BurnPerHour = &b
	}
	if len(cs.PendingByWhy) == 0 {
		cs.PendingByWhy = nil
	}
	cs.Notes = []string{
		"Cloud nodes (slurm-gcp): powered-down nodes have no VM and cost nothing; powered-up nodes bill per hour whether or not a job runs.",
		s.sharedSummary(ctx),
	}
	if len(cs.IdleBilling) > 0 {
		cs.Notes = append(cs.Notes, fmt.Sprintf("%d node(s) are powered up with no job (billing while idle). A warm worker kept on purpose looks like this too.", len(cs.IdleBilling)))
	}
	return cs, nil
}

// ---- partitions -------------------------------------------------------------------

// PartitionInfo merges the catalog with live node counts.
type PartitionInfo struct {
	CatalogPartition
	USD *float64 `json:"usd_per_node_hour,omitempty"`
}

// Partitions lists partitions from the catalog with configured prices.
func (s *Service) Partitions(ctx context.Context) ([]PartitionInfo, error) {
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out []PartitionInfo
	for _, p := range cat.Partitions {
		pi := PartitionInfo{CatalogPartition: p}
		pi.CatalogPartition.USDPerNodeHr = nil
		if v, ok := s.price(cat, p.Name); ok && s.Cfg.ShowCost {
			v := v
			pi.USD = &v
		}
		out = append(out, pi)
	}
	return out, nil
}

// ---- usage -------------------------------------------------------------------------

// UsageRow is usage for one group key.
type UsageRow struct {
	Key        string  `json:"key"`
	Jobs       int     `json:"jobs"`
	Failed     int     `json:"failed"`
	NodeHours  float64 `json:"node_hours"`
	CoreHours  float64 `json:"core_hours"`
	GPUHours   float64 `json:"gpu_hours,omitempty"`
	CPUPercent float64 `json:"cpu_efficiency_percent"`
	CostUSD    float64 `json:"est_cost_usd,omitempty"`
}

// Usage is the my_usage / usage_report answer.
type Usage struct {
	Since   string     `json:"since"`
	Until   string     `json:"until,omitempty"`
	GroupBy string     `json:"group_by"`
	Rows    []UsageRow `json:"rows"`
	Total   UsageRow   `json:"total"`
	Notes   []string   `json:"notes"`
}

// UsageInput selects a usage report.
type UsageInput struct {
	User    string // "" = caller (ignored when All)
	All     bool
	Since   string
	Until   string
	GroupBy string // partition | user | state
}

// Usage sums node-hours, core-hours and estimated cost from accounting.
func (s *Service) Usage(ctx context.Context, in UsageInput) (*Usage, error) {
	if in.Since == "" {
		in.Since = "now-30days"
	}
	if in.GroupBy == "" {
		in.GroupBy = "partition"
	}
	if in.GroupBy != "partition" && in.GroupBy != "user" && in.GroupBy != "state" {
		return nil, fmt.Errorf("group_by must be partition, user or state")
	}
	var c backend.Command
	var err error
	if in.All {
		c, err = backend.SacctAll(in.Since, in.Until)
	} else {
		me, uerr := s.User(ctx)
		if uerr != nil {
			return nil, uerr
		}
		c, err = backend.SacctUser(firstNonEmpty(in.User, me), in.Since, in.Until)
	}
	if err != nil {
		return nil, err
	}
	b, err := s.run(ctx, c)
	if err != nil {
		return nil, err
	}
	var a slurm.AcctResponse
	if err := slurm.Decode(b, &a); err != nil {
		return nil, err
	}
	cat, _ := s.Catalog(ctx)
	type acc struct {
		row             UsageRow
		cpuSec, coreSec float64
	}
	rows := map[string]*acc{}
	tot := &acc{row: UsageRow{Key: "total"}}
	missingPrice := map[string]bool{}
	for _, j := range a.Jobs {
		key := j.Partition
		switch in.GroupBy {
		case "user":
			key = j.User
		case "state":
			key = j.StateName()
		}
		r := rows[key]
		if r == nil {
			r = &acc{row: UsageRow{Key: key}}
			rows[key] = r
		}
		hrs := float64(j.Time.Elapsed) / 3600
		nodes := float64(max(j.AllocationNodes, 1))
		cores := float64(j.TRES.Allocated.Get("cpu"))
		gpus := float64(j.TRES.Allocated.Get("gres/gpu"))
		if gpus == 0 && cat != nil { // GPUs are not accounted as TRES here; nodes are whole
			if cp, ok := cat.Partition(j.Partition); ok {
				gpus = float64(cp.GPUsPerNode) * nodes
			}
		}
		price, priced := s.price(cat, j.Partition)
		if !priced && j.Time.Elapsed > 0 {
			missingPrice[j.Partition] = true
		}
		failed := false
		switch j.StateName() {
		case "FAILED", "OUT_OF_MEMORY", "TIMEOUT", "NODE_FAIL":
			failed = true
		}
		for _, x := range []*acc{r, tot} {
			x.row.Jobs++
			if failed {
				x.row.Failed++
			}
			x.row.NodeHours += nodes * hrs
			x.row.CoreHours += cores * hrs
			x.row.GPUHours += gpus * hrs
			if priced {
				// shared partitions bill the share of the node the job held (v0.8.0);
				// on exclusive partitions Slurm allocates whole nodes, so share is 1
				x.row.CostUSD += price * nodes * hrs * allocShare(cat, j.Partition, int64(nodes), int64(cores), j.TRES.Allocated.Get("mem"))
			}
			x.cpuSec += j.Time.Total.Float()
			x.coreSec += cores * float64(j.Time.Elapsed)
		}
	}
	u := &Usage{Since: in.Since, Until: in.Until, GroupBy: in.GroupBy}
	finish := func(x *acc) UsageRow {
		if x.coreSec > 0 {
			x.row.CPUPercent = round(100*x.cpuSec/x.coreSec, 1)
		}
		fin(&x.row, s.Cfg.ShowCost)
		return x.row
	}
	for _, r := range rows {
		u.Rows = append(u.Rows, finish(r))
	}
	sort.Slice(u.Rows, func(i, j int) bool { return u.Rows[i].NodeHours > u.Rows[j].NodeHours })
	u.Total = finish(tot)
	if u.Rows == nil {
		u.Rows = []UsageRow{}
	}
	u.Notes = []string{"Estimated cost = partition list price x nodes x elapsed hours x the share of each node the job held (1 on whole-node partitions; cores or memory share, whichever is larger, on shared ones); it excludes boot/idle time and credits. Treat as an estimate."}
	if len(missingPrice) > 0 {
		var m []string
		for k := range missingPrice {
			m = append(m, k)
		}
		sort.Strings(m)
		u.Notes = append(u.Notes, "No price for partition(s): "+strings.Join(m, ", "))
	}
	if !s.Cfg.ShowCost {
		u.Notes = append(u.Notes, "Dollar figures hidden (show_cost: false).")
	}
	return u, nil
}

func fin(r *UsageRow, showCost bool) {
	r.NodeHours = round(r.NodeHours, 2)
	r.CoreHours = round(r.CoreHours, 1)
	r.GPUHours = round(r.GPUHours, 2)
	if showCost {
		r.CostUSD = round(r.CostUSD, 2)
	} else {
		r.CostUSD = 0
	}
}

// ---- script check ------------------------------------------------------------------

// ScriptIssue is one problem found in a batch script.
type ScriptIssue struct {
	Severity string `json:"severity"` // error | warning | info
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// ScriptCheck is the static check result.
type ScriptCheck struct {
	OK         bool              `json:"ok"`
	Partition  string            `json:"partition"`
	Request    map[string]string `json:"request"`
	Modules    []string          `json:"modules"`
	Issues     []ScriptIssue     `json:"issues"`
	Cores      *CoreRequest      `json:"cores,omitempty"` // what the job holds per node (v0.8.0)
	EstCostUSD *float64          `json:"est_max_cost_usd,omitempty"`
	Note       string            `json:"note"`
}

var (
	// idioms that count the whole node's cores rather than the job's
	reAllCores = regexp.MustCompile(`os\.cpu_count\(\)|multiprocessing\.cpu_count\(\)|nproc\s+--all|/proc/cpuinfo`)
	reSbatch   = regexp.MustCompile(`^#SBATCH\s+(.*)$`)
	reModLoad  = regexp.MustCompile(`\bmodule\s+(?:load|add)\s+([^;&|#\n]+)`)
	reMemValue = regexp.MustCompile(`^(\d+)([KMGT]?)B?$`)
)

// ScriptCheck checks a batch script against the cluster's partitions and modules.
// Nothing is submitted; the script is never sent to the cluster.
func (s *Service) ScriptCheck(ctx context.Context, script string) (*ScriptCheck, error) {
	if len(script) > s.Cfg.Limits.ScriptBytes {
		return nil, fmt.Errorf("script is %d bytes; limit %d", len(script), s.Cfg.Limits.ScriptBytes)
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	sc := &ScriptCheck{Request: map[string]string{}, Issues: []ScriptIssue{}, Modules: []string{},
		Note: "Static check only; nothing was submitted."}
	add := func(sev string, line int, msg string, a ...any) {
		sc.Issues = append(sc.Issues, ScriptIssue{Severity: sev, Line: line, Message: fmt.Sprintf(msg, a...)})
	}
	lines := strings.Split(script, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "#!") {
		add("error", 1, "first line must be a shebang such as #!/bin/bash (sbatch rejects scripts without one)")
	}
	bodyStarted := false
	for i, raw := range lines {
		ln := strings.TrimSpace(raw)
		if m := reSbatch.FindStringSubmatch(ln); m != nil {
			if bodyStarted {
				add("warning", i+1, "#SBATCH after the first command is ignored by sbatch: %s", ln)
				continue
			}
			parseSbatch(m[1], sc.Request)
			continue
		}
		if ln != "" && !strings.HasPrefix(ln, "#") {
			bodyStarted = true
		}
		if strings.HasPrefix(ln, "#") {
			continue // a comment that mentions `module load x` loads nothing
		}
		for _, m := range reModLoad.FindAllStringSubmatch(raw, -1) {
			for _, mod := range strings.Fields(m[1]) {
				if strings.HasPrefix(mod, "-") || strings.Contains(mod, "$") {
					continue
				}
				sc.Modules = append(sc.Modules, mod)
				ok, _ := cat.ModuleExists(mod)
				needs, satisfied := mpiSatisfied(lines[:i+1], cat.ModuleRequires(mod))
				switch {
				case !ok:
					msg := "module %q not found in the cluster catalog"
					if c := cat.Closest(mod, 3); len(c) > 0 {
						msg += "; closest: " + strings.Join(c, ", ")
					}
					add("error", i+1, msg, mod)
				case !satisfied:
					add("error", i+1, "module %q is MPI-built and needs `%s` first", mod, needs)
				}
			}
		}
		if strings.Contains(raw, "--partition=") || strings.Contains(raw, "sbatch ") && i > 0 && !strings.HasPrefix(ln, "#") {
			if strings.Contains(raw, "sbatch ") {
				add("info", i+1, "the script calls sbatch itself (nested submission)")
			}
		}
	}
	mins, hasTime := slurmMinutes(sc.Request["time"])
	if !hasTime {
		add("warning", 0, "no --time limit: the job can run (and bill) until it ends on its own; set a limit")
	}
	part := firstNonEmpty(sc.Request["partition"], cat.DefaultPartition())
	sc.Partition = part
	p, ok := cat.Partition(part)
	if !ok {
		var names []string
		for _, x := range cat.Partitions {
			names = append(names, x.Name)
		}
		add("error", 0, "partition %q does not exist; partitions: %s", part, strings.Join(names, ", "))
	} else {
		if sc.Request["partition"] == "" {
			add("info", 0, "no partition given; Slurm will use the default (%s)", part)
		}
		nodes := atoiDefault(sc.Request["nodes"], 1)
		if nodes > p.MaxNodes {
			add("error", 0, "%d nodes requested; %s has %d", nodes, part, p.MaxNodes)
		}
		if c := cpusPerNode(sc.Request); c > p.CPUsPerNode {
			add("error", 0, "%d CPUs per node requested; %s nodes have %d physical cores (no hyperthreads)", c, part, p.CPUsPerNode)
		}
		if mem, ok := memMB(sc.Request["mem"]); ok && float64(mem) > p.MemGBPerNode*1024 {
			add("error", 0, "--mem %s is more than a %s node has (%.0f GB)", sc.Request["mem"], part, p.MemGBPerNode)
		}
		g := gpusRequested(sc.Request)
		if g > 0 && p.GPUsPerNode == 0 {
			add("error", 0, "GPUs requested on %s, which has no GPUs; use gpul4", part)
		}
		if g > p.GPUsPerNode && p.GPUsPerNode > 0 {
			add("error", 0, "%d GPUs per node requested; %s has %d", g, part, p.GPUsPerNode)
		}
		if g == 0 && p.GPUsPerNode > 0 {
			add("warning", 0, "running on %s without --gres=gpu:1; the GPU will not be visible to the job", part)
		}
		if p.Spot && !strings.Contains(script, "--requeue") {
			add("warning", 0, "spot nodes can be reclaimed with ~30 s notice; add #SBATCH --requeue and make the job restartable")
		}
		shared := s.partitionShared(ctx, part)
		cr := coreRequest(sc.Request, p, shared)
		sc.Cores = &cr
		if shared && !cr.CoresAsked {
			// a job with no core request gets 1 core on a shared node and is held
			// there; refuse rather than let it run 20x slower than its author meant
			add("error", 0, "%s shares nodes between jobs: this script %s. Ask for the cores it needs (#SBATCH --cpus-per-task=N, or --ntasks-per-node=N for MPI ranks), or #SBATCH --exclusive for the whole node", part, cr.DefaultNote)
		}
		if shared && !cr.Exclusive {
			if m := reAllCores.FindString(script); m != "" {
				add("warning", 0, "the script counts every core on the node (%s), but on shared %s the job holds %d; use $SLURM_CPUS_PER_TASK or $SLURM_CPUS_ON_NODE", m, part, cr.CoresPerNod)
			}
		}
		if price, ok := s.price(cat, part); ok && hasTime && s.Cfg.ShowCost {
			c := round(price*float64(nodes)*cr.NodeShare*float64(mins)/60, 2)
			sc.EstCostUSD = &c
		}
		if shared {
			sc.Note += fmt.Sprintf(" %s shares nodes: cost is the share of the node held (%.0f%%, by %s).", part, 100*cr.NodeShare, firstNonEmpty(cr.Basis, "whole node"))
		} else {
			sc.Note += fmt.Sprintf(" %s gives whole nodes, so cost is per node regardless of cores used.", part)
		}
	}
	// the run-time checks below read code only: a comment (# ...) runs nothing
	code := codeOnly(script)
	if strings.Contains(code, "srun") && sc.Request["ntasks"] == "" && sc.Request["ntasks-per-node"] == "" && atoiDefault(sc.Request["nodes"], 1) > 1 {
		add("warning", 0, "multi-node srun without --ntasks/--ntasks-per-node runs one task per node")
	}
	if regexp.MustCompile(`(?m)^\s*(pip|pip3)\s+install\b`).MatchString(code) && !strings.Contains(code, "venv") && !strings.Contains(code, "uv ") && !strings.Contains(code, "pixi") && !strings.Contains(code, "--user") {
		add("warning", 0, "bare `pip install` without a virtual environment; use `uv venv $TMPDIR/v` or a Pixi/conda env")
	}
	if strings.Contains(code, "python") && !regexp.MustCompile(`python-(sci|ml)|venv|conda|pixi|uv |apptainer|miniforge`).MatchString(code) {
		add("warning", 0, "calls python without loading python-sci/python-ml or an environment (system Python 3.6 on Rocky 8)")
	}
	if strings.Contains(code, "apptainer") && strings.Contains(code, "--nv") && part != "gpul4" {
		add("warning", 0, "apptainer --nv on a partition without GPUs")
	}
	sc.OK = true
	for _, is := range sc.Issues {
		if is.Severity == "error" {
			sc.OK = false
		}
	}
	return sc, nil
}

// codeOnly drops comment lines and trailing comments (" # ..."), keeping the
// shebang out too, so checks don't fire on a commented-out alternative.
func codeOnly(script string) string {
	var b strings.Builder
	for _, l := range strings.Split(script, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if i := strings.Index(l, " #"); i >= 0 && !strings.ContainsAny(l[:i], "'\"") {
			l = l[:i]
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

func parseSbatch(opts string, req map[string]string) {
	f := strings.Fields(opts)
	for i := 0; i < len(f); i++ {
		o := f[i]
		if strings.HasPrefix(o, "#") {
			break
		}
		key, val, hasEq := strings.Cut(strings.TrimLeft(o, "-"), "=")
		short := map[string]string{"p": "partition", "N": "nodes", "n": "ntasks", "c": "cpus-per-task", "t": "time", "J": "job-name", "G": "gpus", "o": "output", "e": "error", "A": "account"}
		if !strings.HasPrefix(o, "--") && strings.HasPrefix(o, "-") && len(o) >= 2 {
			k := o[1:2]
			if long, ok := short[k]; ok {
				key = long
				val = o[2:]
				if val == "" && i+1 < len(f) {
					i++
					val = f[i]
				}
				req[key] = val
				continue
			}
		}
		if !hasEq && i+1 < len(f) && !strings.HasPrefix(f[i+1], "-") {
			switch key {
			case "partition", "nodes", "ntasks", "ntasks-per-node", "cpus-per-task", "time", "mem", "mem-per-cpu", "gres", "gpus", "gpus-per-node", "job-name", "output", "error", "account", "array":
				i++
				val = f[i]
			}
		}
		if val == "" && !hasEq {
			val = "true"
		}
		req[key] = val
	}
}

func atoiDefault(s string, d int) int {
	s, _, _ = strings.Cut(s, "-") // "2-4" node ranges: take the minimum
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return d
}

func cpusPerNode(req map[string]string) int {
	cpt := atoiDefault(req["cpus-per-task"], 1)
	if t := atoiDefault(req["ntasks-per-node"], 0); t > 0 {
		return t * cpt
	}
	nodes := atoiDefault(req["nodes"], 1)
	if t := atoiDefault(req["ntasks"], 0); t > 0 {
		per := (t + nodes - 1) / nodes
		return per * cpt
	}
	return cpt
}

func gpusRequested(req map[string]string) int {
	for _, k := range []string{"gpus-per-node", "gpus"} {
		if v := req[k]; v != "" {
			parts := strings.Split(v, ":")
			return atoiDefault(parts[len(parts)-1], 1)
		}
	}
	if g := req["gres"]; strings.HasPrefix(g, "gpu") {
		parts := strings.Split(g, ":")
		return atoiDefault(parts[len(parts)-1], 1)
	}
	return 0
}

func memMB(s string) (int64, bool) {
	m := reMemValue.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, false
	}
	v, _ := strconv.ParseInt(m[1], 10, 64)
	switch m[2] {
	case "K":
		v /= 1024
	case "G":
		v *= 1024
	case "T":
		v *= 1024 * 1024
	}
	return v, true
}

// slurmMinutes parses Slurm time formats: M, M:S, H:M:S, D-H, D-H:M, D-H:M:S.
func slurmMinutes(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	if s == "infinite" || s == "UNLIMITED" {
		return 0, false
	}
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days = atoiDefault(d, 0)
		s = rest
		p := strings.Split(s, ":")
		h := atoiDefault(p[0], 0)
		m := 0
		if len(p) > 1 {
			m = atoiDefault(p[1], 0)
		}
		return days*1440 + h*60 + m, true
	}
	p := strings.Split(s, ":")
	switch len(p) {
	case 1:
		return atoiDefault(p[0], 0), true
	case 2:
		return atoiDefault(p[0], 0), true
	case 3:
		return atoiDefault(p[0], 0)*60 + atoiDefault(p[1], 0), true
	}
	return 0, false
}

// mpiSatisfied reports whether any of the MPI prerequisites in reqs is loaded in
// lines; with none loaded it returns a readable "need" text naming all choices.
func mpiSatisfied(lines []string, reqs []string) (string, bool) {
	if len(reqs) == 0 {
		return "", true
	}
	names := make([]string, 0, len(reqs))
	for _, r := range reqs {
		mpi := strings.TrimPrefix(r, "module load ")
		if loadedBefore(lines, mpi) {
			return "", true
		}
		names = append(names, mpi)
	}
	if len(names) == 1 {
		return "module load " + names[0], false
	}
	return "module load " + strings.Join(names, "` or `module load "), false
}

func loadedBefore(lines []string, mpi string) bool {
	for _, l := range lines {
		for _, m := range reModLoad.FindAllStringSubmatch(l, -1) {
			for _, mod := range strings.Fields(m[1]) {
				if mod == mpi || strings.HasPrefix(mod, mpi+"/") {
					return true
				}
			}
		}
	}
	return false
}

// ---- modules ---------------------------------------------------------------------

// ModuleShowResult is what module_show returns.
type ModuleShowResult struct {
	Module string `json:"module"`
	// LoadedFirst is the MPI module loaded before `module show` (MPI-built
	// packages are only visible under an MPI); empty for core modules.
	LoadedFirst string `json:"loaded_first,omitempty"`
	// BuiltFor lists every `module load ...` under which the package exists.
	BuiltFor []string `json:"built_for,omitempty"`
	Show     string   `json:"show"`
	Notes    []string `json:"notes,omitempty"`
}

// ModuleShow returns what a module sets (Lmod output, trusted site content). On this
// cluster's hierarchical Lmod tree, packages built with MPI (hdf5, fftw, petsc...) are
// invisible until an MPI is loaded, so ModuleShow loads one first: the requested mpi
// when given (it must be one the package is built for), else openmpi when the package
// is built for it, else the first choice.
func (s *Service) ModuleShow(ctx context.Context, name, mpi string) (*ModuleShowResult, error) {
	res := &ModuleShowResult{Module: name}
	cat, cerr := s.Catalog(ctx) // optional for a plain show
	if cerr != nil && mpi != "" {
		// cannot check the choice without the catalog; never guess
		return nil, fmt.Errorf("cannot check which MPI builds %s has: %w", name, cerr)
	}
	var choices []string
	if cat != nil {
		for _, r := range cat.ModuleRequires(name) {
			choices = append(choices, strings.TrimSpace(strings.TrimPrefix(r, "module load ")))
		}
	}
	for _, c := range choices {
		res.BuiltFor = append(res.BuiltFor, "module load "+c)
	}
	switch {
	case mpi != "":
		ok := false
		for _, c := range choices {
			if c == mpi || strings.HasPrefix(c, mpi+"/") || strings.HasPrefix(mpi, c+"/") {
				ok = true
			}
		}
		if !ok {
			if len(choices) == 0 {
				return nil, fmt.Errorf("%s is not built against an MPI; call module_show without mpi", name)
			}
			return nil, fmt.Errorf("%s is not built for %s; choices: %s", name, mpi, strings.Join(choices, ", "))
		}
		res.LoadedFirst = mpi
	case len(choices) > 0:
		res.LoadedFirst = choices[0]
		for _, c := range choices {
			if c == "openmpi" || strings.HasPrefix(c, "openmpi/") {
				res.LoadedFirst = c
			}
		}
	}
	c, err := backend.ModuleShowUnder(name, res.LoadedFirst)
	if err != nil {
		return nil, err
	}
	b, err := s.run(ctx, c)
	if err != nil {
		return nil, err
	}
	out := string(b)
	if strings.Contains(out, "Failed to find the following module") {
		return nil, fmt.Errorf("module %s was not found; modules_search lists what exists (MPI-built packages need their MPI, which module_show loads for known ones)", name)
	}
	if res.LoadedFirst != "" {
		res.Notes = append(res.Notes, fmt.Sprintf("%s is built with MPI, so it was shown after `module load %s`; in a job script load that MPI before it.", name, res.LoadedFirst))
		if len(choices) > 1 {
			res.Notes = append(res.Notes, "Also built for: "+strings.Join(choices, ", ")+" (pass mpi to see another build).")
		}
	}
	if len(out) > 8000 {
		out = out[:8000]
		markTruncated(ctx)
	}
	res.Show = out
	return res, nil
}
