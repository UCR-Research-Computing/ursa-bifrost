package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/core"
)

func tw(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func money(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("$%.2f", *p)
}

// elapsedText shows "-" only for jobs that never ran; a job that finished in
// under a second shows 0m00s.
func elapsedText(state string, sec int64) string {
	s := strings.Fields(state)
	if sec <= 0 && (len(s) == 0 || s[0] == "PENDING" || s[0] == "CANCELLED") {
		return "-"
	}
	return dur(sec)
}

func dur(sec int64) string {
	if sec < 0 {
		return "-"
	}
	d := time.Duration(sec) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h >= 24 {
		return fmt.Sprintf("%dd%02dh", h/24, h%24)
	}
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm%02ds", m, int(d.Seconds())%60)
}

func printStatus(w io.Writer, s *core.ClusterStatus) {
	fmt.Fprintf(w, "%s (Slurm %s): %d running, %d pending, %d node(s) powered up, now %s/hour\n\n",
		s.Cluster, s.SlurmVersion, s.JobsRunning, s.JobsPending, s.NodesUp, money(s.BurnPerHour))
	t := tw(w)
	fmt.Fprintln(t, "PARTITION\tNODES\tUP\tALLOC\tIDLE-UP\tBOOT\tDOWN\tRUN\tPEND\t$/NODE-H\t$/H NOW")
	for _, p := range s.Partitions {
		name := p.Name
		if p.Default {
			name += "*"
		}
		fmt.Fprintf(t, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", name, p.NodesTotal, p.PoweredUp, p.Allocated,
			p.IdleUp, p.Booting, p.Down, p.Running, p.Pending, money(p.USDPerHour), money(p.BurnPerHour))
	}
	t.Flush()
	if len(s.IdleBilling) > 0 {
		fmt.Fprintf(w, "\nIdle but powered up (billing): %s\n", strings.Join(s.IdleBilling, ", "))
	}
	for _, p := range s.Partitions {
		for _, pr := range p.Problems {
			fmt.Fprintln(w, "problem:", pr)
		}
	}
	for k, v := range s.PendingByWhy {
		fmt.Fprintf(w, "pending (%s): %d\n", k, v)
	}
	fmt.Fprintln(w, "\n* default partition. Powered-down cloud nodes cost nothing.")
}

func printPartitions(w io.Writer, ps []core.PartitionInfo) {
	t := tw(w)
	fmt.Fprintln(t, "PARTITION\tNODES\tCORES\tMEM GB\tGPU\t$/NODE-H\tUSE FOR")
	for _, p := range ps {
		name := p.Name
		if p.Default {
			name += "*"
		}
		gpu := "-"
		if p.GPUsPerNode > 0 && p.GPUType != nil {
			gpu = fmt.Sprintf("%dx %s", p.GPUsPerNode, *p.GPUType)
		}
		use := p.UseFor
		if len(use) > 70 {
			use = use[:70] + "..."
		}
		fmt.Fprintf(t, "%s\t%d\t%d\t%.0f\t%s\t%s\t%s\n", name, p.MaxNodes, p.CPUsPerNode, p.MemGBPerNode, gpu, money(p.USD), use)
	}
	t.Flush()
}

func printJobs(w io.Writer, js []core.JobSummary, showUser bool) {
	if len(js) == 0 {
		fmt.Fprintln(w, "no jobs")
		return
	}
	t := tw(w)
	hdr := "JOBID\tSTATE\tPARTITION\tNODES\tELAPSED\tEXIT\tEST $\tNAME"
	if showUser {
		hdr = "JOBID\tUSER\tSTATE\tPARTITION\tNODES\tELAPSED\tEXIT\tEST $\tNAME"
	}
	fmt.Fprintln(t, hdr)
	for _, j := range js {
		state := j.State
		if j.Reason != "" {
			state += " (" + j.Reason + ")"
		}
		cost := "-"
		if j.CostUSD > 0 {
			cost = fmt.Sprintf("%.2f", j.CostUSD)
		}
		name := j.Name
		if len(name) > 40 {
			name = name[:40] + "..."
		}
		if showUser {
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", j.JobID, j.User, state, j.Partition, j.NodeCount, elapsedText(j.State, j.ElapsedS), j.ExitCode, cost, name)
		} else {
			fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", j.JobID, state, j.Partition, j.NodeCount, elapsedText(j.State, j.ElapsedS), j.ExitCode, cost, name)
		}
	}
	t.Flush()
}

func printJob(w io.Writer, d *core.JobDetail) {
	fmt.Fprintf(w, "Job %s  %s  user %s  partition %s\n", d.JobID, d.State, d.User, d.Partition)
	fmt.Fprintf(w, "name: %s\n", d.Name)
	if d.Reason != "" {
		fmt.Fprintf(w, "reason: %s\n", d.Reason)
	}
	if d.PendingReason != nil {
		fmt.Fprintf(w, "waiting: %s\n  -> %s\n", d.PendingReason.Meaning, d.PendingReason.Advice)
	}
	fmt.Fprintf(w, "submitted %s  started %s  ended %s  elapsed %s  limit %dm\n", d.Submitted, d.Started, d.Ended, elapsedText(d.State, d.ElapsedS), d.TimeLimitMin)
	fmt.Fprintf(w, "nodes %d (%s)  cpus %d  exit %s\n", d.NodeCount, d.Nodes, d.CPUs, d.ExitCode)
	if e := d.Efficiency; e != nil {
		fmt.Fprintf(w, "CPU efficiency %.1f%%  memory peak %d MB of %d MB (%.1f%%)\n", e.CPUPercent, e.MemPeakMB, e.MemAllocMB, e.MemPercent)
	}
	if d.CostUSD > 0 {
		fmt.Fprintf(w, "estimated cost $%.2f\n", d.CostUSD)
	}
	fmt.Fprintf(w, "stdout %s\n", d.Stdout)
	if d.Stderr != "" && d.Stderr != d.Stdout {
		fmt.Fprintf(w, "stderr %s\n", d.Stderr)
	}
	for _, s := range d.Steps {
		fmt.Fprintf(w, "  step %-12s %-10s exit %-6s %s  maxrss %d MB\n", s.ID, s.State, s.ExitCode, dur(s.ElapsedS), s.MaxRSSMB)
	}
	if d.Script != nil {
		fmt.Fprintf(w, "\n--- batch script (untrusted, redacted) ---\n%s\n", d.Script.Text)
	}
}

func printExplain(w io.Writer, e *core.Explanation) {
	fmt.Fprintf(w, "Job %s  %s  partition %s  exit %s  elapsed %s\n", e.Job.JobID, e.Job.State, e.Job.Partition, e.Job.ExitCode, elapsedText(e.Job.State, e.Job.ElapsedS))
	if len(e.Findings) == 0 {
		fmt.Fprintln(w, "No findings: nothing looks wrong.")
	}
	for _, f := range e.Findings {
		fmt.Fprintf(w, "\n[%s] %s (%s)\n", strings.ToUpper(f.Severity), f.Title, f.Rule)
		for _, ev := range f.Evidence {
			fmt.Fprintf(w, "  evidence: %s\n", ev)
		}
		fmt.Fprintf(w, "  fix: %s\n", f.Suggestion)
	}
	if e.LogError != "" {
		fmt.Fprintf(w, "\nlog: %s\n", e.LogError)
	} else if e.LogPath != "" {
		fmt.Fprintf(w, "\nlog: %s (bifrost job log %s)\n", e.LogPath, e.Job.JobID)
	}
}

func printModules(w io.Writer, r moduleResult) {
	hs := r.Matches
	for _, c := range r.Containers {
		fmt.Fprintf(w, "container: %s (%.1f GB)  ->  apptainer exec [--nv] %s <cmd>\n", c.Path, c.SizeGB, c.Path)
	}
	if len(hs) == 0 {
		if len(r.Containers) == 0 {
			fmt.Fprintln(w, "no matching modules or containers")
		}
		return
	}
	t := tw(w)
	fmt.Fprintln(t, "MODULE\tVERSIONS\tNEEDS FIRST\tGPU")
	for _, h := range hs {
		g := ""
		if h.GPU {
			g = "yes"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", h.Name, strings.Join(h.Versions, " "), h.Requires, g)
	}
	t.Flush()
}

func printRecipes(w io.Writer, rs []core.Recipe) {
	if len(rs) == 0 {
		fmt.Fprintln(w, "no matching recipes")
	}
	for _, r := range rs {
		fmt.Fprintf(w, "%s  [%s, partition %s]\n  module load %s\n  %s\n", r.Name, r.Field, r.Partition, strings.Join(r.Load, " "), r.Run)
		if r.Notes != "" {
			fmt.Fprintf(w, "  note: %s\n", r.Notes)
		}
		fmt.Fprintln(w)
	}
}

func printCheck(w io.Writer, c *core.ScriptCheck) {
	verdict := "OK"
	if !c.OK {
		verdict = "PROBLEMS FOUND"
	}
	fmt.Fprintf(w, "%s  partition %s", verdict, c.Partition)
	if c.EstCostUSD != nil {
		fmt.Fprintf(w, "  worst-case cost $%.2f", *c.EstCostUSD)
	}
	fmt.Fprintln(w)
	for _, is := range c.Issues {
		loc := ""
		if is.Line > 0 {
			loc = fmt.Sprintf("line %d: ", is.Line)
		}
		fmt.Fprintf(w, "  %-7s %s%s\n", is.Severity, loc, is.Message)
	}
}

func printUsage(w io.Writer, u *core.Usage) {
	fmt.Fprintf(w, "Usage since %s by %s\n\n", u.Since, u.GroupBy)
	t := tw(w)
	fmt.Fprintln(t, strings.ToUpper(u.GroupBy)+"\tJOBS\tFAILED\tNODE-H\tCORE-H\tGPU-H\tCPU EFF\tEST $")
	row := func(r core.UsageRow) {
		fmt.Fprintf(t, "%s\t%d\t%d\t%.2f\t%.1f\t%.2f\t%.1f%%\t%.2f\n", r.Key, r.Jobs, r.Failed, r.NodeHours, r.CoreHours, r.GPUHours, r.CPUPercent, r.CostUSD)
	}
	for _, r := range u.Rows {
		row(r)
	}
	row(u.Total)
	t.Flush()
	for _, n := range u.Notes {
		fmt.Fprintln(w, "\nnote:", n)
	}
}

func printWaste(w io.Writer, r *core.WasteReport) {
	fmt.Fprintf(w, "Waste since %s (%s): %d item(s), %.2f wasted node-hours", r.Since, r.Scope, len(r.Items), r.TotalWasteH)
	if r.TotalWasteUS > 0 {
		fmt.Fprintf(w, ", about $%.2f", r.TotalWasteUS)
	}
	fmt.Fprintf(w, " (%d jobs scanned)\n", r.JobsScanned)
	if len(r.Items) == 0 {
		fmt.Fprintln(w, "\nNothing above the thresholds.")
		return
	}
	for _, it := range r.Items {
		who := it.JobID
		if who == "" {
			who = it.Node
		}
		if len(who) > 40 {
			who = who[:40] + "..."
		}
		cost := ""
		if it.CostUSD > 0 {
			cost = fmt.Sprintf(" ~$%.2f", it.CostUSD)
		}
		user := ""
		if it.User != "" && r.Scope == "all users" {
			user = " " + it.User
		}
		fmt.Fprintf(w, "\n%-19s %s%s [%s] %.2f wasted node-h%s\n  %s\n  -> %s\n", it.Kind, who, user, it.Partition, it.WasteHours, cost, it.Detail, it.Suggestion)
	}
	fmt.Fprintln(w, "\nnote:", r.Notes[0])
}

func printHealth(w io.Writer, h *core.Health) {
	status := "OK"
	if !h.OK {
		status = "PROBLEMS"
	}
	fmt.Fprintf(w, "Health: %s, %d issue(s)", status, len(h.Issues))
	if h.FailureRate != nil {
		fmt.Fprintf(w, "; last 24 h: %d jobs ended, %.0f%% failed", h.Jobs24h, *h.FailureRate)
	}
	fmt.Fprintln(w)
	for _, i := range h.Issues {
		since := ""
		if i.Since != "" {
			since = " since " + i.Since
		}
		fmt.Fprintf(w, "\n[%s] %s: %s%s\n  %s\n  -> %s\n", strings.ToUpper(i.Severity), i.Kind, i.Subject, since, i.Detail, i.Advice)
	}
}

func printTicket(w io.Writer, t *core.TicketDraft) {
	fmt.Fprintf(w, "Ticket draft for job %s (user %s), confidence %s\n%s\n\n", t.JobID, t.User, t.Confidence, t.Summary)
	fmt.Fprintln(w, "What happened:")
	for _, s := range t.WhatHappened {
		fmt.Fprintln(w, "  "+s)
	}
	fmt.Fprintln(w, "Evidence:")
	for _, s := range t.Evidence {
		fmt.Fprintln(w, "  "+s)
	}
	fmt.Fprintf(w, "\n----- reply draft (edit before sending) -----\n%s---------------------------------------------\n", t.Reply)
	fmt.Fprintln(w, t.InternalNote)
}

func printSubmitPlan(w io.Writer, p *core.SubmitPlan) {
	fmt.Fprintf(w, "Plan (nothing submitted yet):\n")
	fmt.Fprintf(w, "  job        %s\n  partition  %s, %d node(s), time limit %s\n", p.JobName, p.Partition, p.Nodes, p.TimeLimit)
	fmt.Fprintf(w, "  folder     %s (on the cluster)\n  script     %d bytes, sha256 %s\n", p.RemoteDir, p.ScriptBytes, p.ScriptSHA256)
	fmt.Fprintf(w, "  scheduler  %s\n", p.Scheduler)
	fmt.Fprintf(w, "  worst case $%.2f  (today so far $%.2f of $%.2f day cap)\n", p.WorstCaseUSD, p.SpentTodayUS, p.DayCapUSD)
	for _, o := range p.Overrides {
		fmt.Fprintln(w, "  override  ", o)
	}
	for _, x := range p.Warnings {
		fmt.Fprintln(w, "  warning   ", x)
	}
	fmt.Fprintf(w, "  token      %s (expires %s)\n", p.Token, p.ExpiresAt)
}

func printResults(w io.Writer, r *core.Results) {
	fmt.Fprintf(w, "Job %s (%s): %s, %d file(s), %.1f MB\n", r.JobID, r.State, r.Folder, len(r.Files), float64(r.TotalBytes)/(1<<20))
	for _, f := range r.Files {
		fmt.Fprintf(w, "  %10d  %s  %s\n", f.Bytes, f.Modified[:16], f.Path)
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, "note:", n)
	}
	if r.Preview != nil {
		fmt.Fprintf(w, "\n--- %s (untrusted, redacted) ---\n%s\n", r.PreviewOf, r.Preview.Text)
	}
	if r.Downloaded != "" {
		fmt.Fprintf(w, "\nDownloaded %d file(s) to %s\n", len(r.Saved), r.Downloaded)
	}
}

// doctor checks config, gcloud/ssh, the cluster user and the catalog.
func doctor(ctx context.Context, svc *core.Service, w io.Writer, g globals) int {
	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	var cs []check
	add := func(name string, ok bool, detail string) { cs = append(cs, check{name, ok, detail}) }
	add("config", true, svc.Cfg.Path)
	add("backend", true, svc.Backend.Name())
	if svc.Cfg.SSH.GCloud != nil {
		_, err := exec.LookPath("gcloud")
		add("gcloud on PATH", err == nil, errText(err))
	}
	t0 := time.Now()
	u, err := svc.User(ctx)
	add("cluster login", err == nil, firstOf(err, fmt.Sprintf("user %s (%s)", u, time.Since(t0).Round(time.Millisecond))))
	if err == nil {
		cat, err := svc.Catalog(ctx)
		detail := errText(err)
		if err == nil {
			detail = fmt.Sprintf("%s, generated %s, %d partitions, %d core modules", cat.Schema, cat.Generated, len(cat.Partitions), len(cat.Modules.Core))
		}
		add("cluster catalog", err == nil, detail)
		b, err := svc.Backend.Run(ctx, backend.Sinfo())
		add("slurm --json", err == nil && len(b) > 0, firstOf(err, fmt.Sprintf("sinfo --json returned %d bytes", len(b))))
	}
	add("audit log", svc.Audit != nil, svc.Audit.Path())
	add("tiers", true, strings.Join(svc.Cfg.Tiers, ","))
	ok := true
	for _, c := range cs {
		ok = ok && c.OK
	}
	code := 0
	if !ok {
		code = 1
	}
	if g.json {
		emit(w, g, map[string]any{"ok": ok, "checks": cs}, nil)
		return code
	}
	for _, c := range cs {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "%s  %-16s %s\n", mark, c.Name, c.Detail)
	}
	return code
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstOf(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}
