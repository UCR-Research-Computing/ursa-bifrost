package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/rules"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/slurm"
)

// ---- waste report ------------------------------------------------------------------

// WasteItem is one source of avoidable spend.
type WasteItem struct {
	Kind       string  `json:"kind"` // idle-node | low-cpu | oversized-memory | failed-fast-repeat | timeout-idle | long-running-idle
	JobID      string  `json:"job_id,omitempty"`
	Node       string  `json:"node,omitempty"`
	User       string  `json:"user,omitempty"`
	Partition  string  `json:"partition"`
	Detail     string  `json:"detail"`
	NodeHours  float64 `json:"node_hours"`
	WasteHours float64 `json:"wasted_node_hours"`
	CostUSD    float64 `json:"est_wasted_usd,omitempty"`
	Suggestion string  `json:"suggestion"`
}

// WasteReport is the waste_report answer.
type WasteReport struct {
	Since        string         `json:"since"`
	Scope        string         `json:"scope"` // user name or "all users"
	Items        []WasteItem    `json:"items"`
	ByKind       map[string]int `json:"count_by_kind"`
	TotalWasteH  float64        `json:"total_wasted_node_hours"`
	TotalWasteUS float64        `json:"total_est_wasted_usd,omitempty"`
	JobsScanned  int            `json:"jobs_scanned"`
	Thresholds   map[string]any `json:"thresholds"`
	Notes        []string       `json:"notes"`
}

// WasteInput selects a waste report.
type WasteInput struct {
	Since        string
	All          bool    // every user (R2)
	User         string  // one user (R2) or "" for the caller
	CPUThreshold float64 // percent; default 25
	MinNodeHours float64 // ignore jobs smaller than this; default 0.25
}

// Waste finds avoidable node-hours: idle powered-up nodes, jobs that left most
// cores idle, jobs that ran far longer than their CPU use, repeated fast
// failures. Partitions bill whole nodes, so wasted node-hours = node-hours x
// (1 - CPU efficiency), a deliberate upper bound.
func (s *Service) Waste(ctx context.Context, in WasteInput) (*WasteReport, error) {
	if in.Since == "" {
		in.Since = "now-7days"
	}
	if in.CPUThreshold <= 0 {
		in.CPUThreshold = 25
	}
	if in.MinNodeHours <= 0 {
		in.MinNodeHours = 0.25
	}
	var c backend.Command
	var err error
	scope := "all users"
	if in.All {
		c, err = backend.SacctAll(in.Since, "")
	} else {
		me, uerr := s.User(ctx)
		if uerr != nil {
			return nil, uerr
		}
		scope = firstNonEmpty(in.User, me)
		c, err = backend.SacctUser(scope, in.Since, "")
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
	rep := &WasteReport{Since: in.Since, Scope: scope, ByKind: map[string]int{}, JobsScanned: len(a.Jobs),
		Thresholds: map[string]any{"cpu_efficiency_percent": in.CPUThreshold, "min_node_hours": in.MinNodeHours}}
	cost := func(part string, h float64) float64 {
		if p, ok := s.price(cat, part); ok && s.Cfg.ShowCost {
			return round(p*h, 2)
		}
		return 0
	}
	add := func(w WasteItem) {
		w.NodeHours, w.WasteHours = round(w.NodeHours, 2), round(w.WasteHours, 2)
		w.CostUSD = cost(w.Partition, w.WasteHours)
		rep.Items = append(rep.Items, w)
	}

	// 1. Jobs: low CPU efficiency (running jobs included: they bill now).
	failsByKey := map[string][]slurm.AcctJob{}
	for _, j := range a.Jobs {
		nodes := float64(max(j.AllocationNodes, 1))
		nh := nodes * float64(j.Time.Elapsed) / 3600
		cores := float64(j.TRES.Allocated.Get("cpu"))
		st := j.StateName()
		if st == "FAILED" && j.Time.Elapsed < 600 {
			key := j.User + "|" + j.Partition + "|" + nameStem(j.Name)
			failsByKey[key] = append(failsByKey[key], j)
		}
		if nh < in.MinNodeHours || cores == 0 {
			continue
		}
		eff := 100 * j.Time.Total.Float() / (cores * float64(j.Time.Elapsed))
		if st == "RUNNING" && j.Time.Total.Float() == 0 {
			// sacct only fills CPU totals when a job ends; judge running jobs elsewhere
			continue
		}
		if eff < in.CPUThreshold {
			kind := "low-cpu"
			sug := "Most cores sat idle. Use the program's threading/MPI options (OMP_NUM_THREADS, -ntomp, srun -n) or a smaller partition (nvmescratch 8 cores, gpul4 8 cores)."
			if isWarmWorker(j) {
				kind = "warm-worker"
				sug = "A keep-warm job that holds a node so later work starts without a boot wait. Its idle time is the price of that latency: compare it with ~2-5 min cold boots and shorten or stop it when nothing is queued."
			}
			if st == "TIMEOUT" {
				kind = "timeout-idle"
				sug = "Ran to its time limit while barely using the CPU: likely stuck (waiting on a download, a lock, input). Check the log before resubmitting with more time."
			}
			add(WasteItem{Kind: kind, JobID: fmt.Sprint(j.JobID), User: j.User, Partition: j.Partition,
				Detail:    fmt.Sprintf("%s, CPU efficiency %.0f%% on %d cores for %s", st, eff, int(cores), hhmm(j.Time.Elapsed)),
				NodeHours: nh, WasteHours: nh * (1 - eff/100), Suggestion: sug})
		}
		if peak, alloc := j.MaxRSSBytes()/(1024*1024), j.TRES.Allocated.Get("mem"); peak > 0 && alloc > 0 && st == "COMPLETED" {
			if cp, ok := partitionOf(cat, j.Partition); ok && j.Partition == "highmem" && float64(peak) < 0.25*float64(alloc) {
				add(WasteItem{Kind: "oversized-memory", JobID: fmt.Sprint(j.JobID), User: j.User, Partition: j.Partition,
					Detail:    fmt.Sprintf("peak memory %d MB of %d MB on highmem (%.0f GB nodes)", peak, alloc, cp.MemGBPerNode),
					NodeHours: nh, WasteHours: 0,
					Suggestion: fmt.Sprintf("Fits a cheaper partition: standard has 124 GB per node. highmem costs %s/node-hour.", priceText(s, cat, "highmem"))})
			}
		}
	}

	// 2. Repeated fast failures of the same job (same user, partition, name stem).
	for _, js := range failsByKey {
		if len(js) < 3 {
			continue
		}
		var nh float64
		ids := []string{}
		for _, j := range js {
			nh += float64(max(j.AllocationNodes, 1)) * float64(j.Time.Elapsed) / 3600
			ids = append(ids, fmt.Sprint(j.JobID))
		}
		sort.Strings(ids)
		j := js[0]
		add(WasteItem{Kind: "failed-fast-repeat", JobID: strings.Join(ids, ","), User: j.User, Partition: j.Partition,
			Detail:    fmt.Sprintf("%d jobs named like %q failed within 10 minutes", len(js), nameStem(j.Name)),
			NodeHours: nh, WasteHours: nh,
			Suggestion: "Each failed start still boots and bills a node. Run job_explain on one of them and fix the cause before resubmitting; test small with an interactive job first."})
	}

	// 3. Powered-up nodes with no job, and long-running jobs using no CPU now.
	if nb, err := s.run(ctx, backend.Nodes()); err == nil {
		var nr slurm.NodesResponse
		if slurm.Decode(nb, &nr) == nil {
			now := s.Now().Unix()
			for _, n := range nr.Nodes {
				if !n.PoweredUp() {
					continue
				}
				part := ""
				if len(n.Partitions) > 0 {
					part = n.Partitions[0]
				}
				switch {
				case n.HasState("IDLE") && !n.HasState("POWERING_UP"):
					idleH := 0.0
					if n.LastBusy.Valid() && n.LastBusy.Int() > 0 {
						idleH = float64(now-n.LastBusy.Int()) / 3600
					}
					add(WasteItem{Kind: "idle-node", Node: n.Name, Partition: part,
						Detail:    fmt.Sprintf("powered up with no job for %.1f h", idleH),
						NodeHours: idleH, WasteHours: idleH,
						Suggestion: "Billing while idle. Check SuspendTime for this partition (slurm-gcp powers nodes down after it), or whether something keeps it warm on purpose."})
				case n.HasState("ALLOCATED") && n.CPULoad < 10 && n.BootTime.Valid() && now-n.BootTime.Int() > 3600:
					upH := float64(now-n.BootTime.Int()) / 3600
					add(WasteItem{Kind: "allocated-idle-node", Node: n.Name, Partition: part,
						Detail:    fmt.Sprintf("allocated but load %.2f, up %.1f h (a warm or sleeping job holds it)", float64(n.CPULoad)/100, upH),
						NodeHours: upH, WasteHours: 0,
						Suggestion: "A job holds the node without computing. If it is a warm worker kept on purpose, weigh its hourly price against start-up latency; otherwise cancel it."})
				}
			}
		}
	}

	sort.SliceStable(rep.Items, func(i, j int) bool { return rep.Items[i].WasteHours > rep.Items[j].WasteHours })
	for _, it := range rep.Items {
		rep.ByKind[it.Kind]++
		rep.TotalWasteH += it.WasteHours
		rep.TotalWasteUS += it.CostUSD
	}
	rep.TotalWasteH = round(rep.TotalWasteH, 2)
	rep.TotalWasteUS = round(rep.TotalWasteUS, 2)
	if len(rep.Items) > s.Cfg.Limits.ListRows {
		rep.Items = rep.Items[:s.Cfg.Limits.ListRows]
		markTruncated(ctx)
	}
	if rep.Items == nil {
		rep.Items = []WasteItem{}
	}
	rep.Notes = []string{
		"Wasted node-hours = node-hours x (1 - CPU efficiency): an upper bound, because partitions bill whole nodes and some programs are I/O- or GPU-bound by design.",
		"Running jobs have no CPU totals until they end; they appear only through node load (allocated-idle-node).",
		"allocated-idle-node and oversized-memory are listed with 0 wasted hours: they need a person's judgment.",
		"warm-worker items are keep-warm jobs (name contains warm/keepalive): idle by design; their hours are the cost of fast starts.",
	}
	return rep, nil
}

// isWarmWorker recognizes keep-warm jobs (deep-research's lab-warm and similar
// names), whose idle CPU is intended.
func isWarmWorker(j slurm.AcctJob) bool {
	n := strings.ToLower(j.Name)
	return strings.Contains(n, "warm") || strings.Contains(n, "keepalive") || strings.Contains(n, "keep-alive")
}

func partitionOf(cat *Catalog, name string) (CatalogPartition, bool) {
	if cat == nil {
		return CatalogPartition{}, false
	}
	return cat.Partition(name)
}

func priceText(s *Service, cat *Catalog, part string) string {
	if p, ok := s.price(cat, part); ok && s.Cfg.ShowCost {
		return fmt.Sprintf("$%.2f", p)
	}
	return "more"
}

// nameStem groups "lab-76-gpu-lbm" and "lab-82-gpu-lbm" together by dropping
// digit-only tokens.
func nameStem(n string) string {
	var keep []string
	for _, t := range strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if strings.Trim(t, "0123456789") != "" {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, "-")
}

func hhmm(sec int64) string {
	switch {
	case sec >= 3600:
		return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
	case sec >= 60:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%ds", sec)
}

// ---- health ------------------------------------------------------------------------

// HealthIssue is one operational problem.
type HealthIssue struct {
	Severity string `json:"severity"` // error | warning | info
	Kind     string `json:"kind"`     // node-down | node-drained | long-pending | stuck-job | boot-failures | high-failure-rate | idle-billing
	Subject  string `json:"subject"`
	Detail   string `json:"detail"`
	Since    string `json:"since,omitempty"`
	Advice   string `json:"advice"`
}

// Health is the health answer.
type Health struct {
	OK          bool           `json:"ok"`
	Issues      []HealthIssue  `json:"issues"`
	Counts      map[string]int `json:"counts"`
	FailureRate *float64       `json:"failure_rate_24h_percent,omitempty"`
	Jobs24h     int            `json:"jobs_ended_24h"`
	Notes       []string       `json:"notes"`
}

// Health checks nodes, the queue and the last day's accounting for problems.
// Staff tier: it reads every user's jobs.
func (s *Service) Health(ctx context.Context) (*Health, error) {
	h := &Health{Counts: map[string]int{}, Issues: []HealthIssue{}}
	add := func(i HealthIssue) { h.Issues = append(h.Issues, i) }
	now := s.Now().Unix()

	var nr slurm.NodesResponse
	b, err := s.run(ctx, backend.Nodes())
	if err != nil {
		return nil, err
	}
	if err := slurm.Decode(b, &nr); err != nil {
		return nil, err
	}
	for _, n := range nr.Nodes {
		since := numTS(n.ReasonAt)
		reason := n.Reason
		if reason == "" {
			reason = "no reason recorded"
		}
		switch {
		case n.Broken():
			add(HealthIssue{Severity: "error", Kind: "node-down", Subject: n.Name, Since: since,
				Detail: strings.Join(n.State, "+") + ": " + reason,
				Advice: "On slurm-gcp a cloud node that failed to boot or was lost goes DOWN; check the resume log on the controller and `scontrol update nodename=... state=resume` once fixed (admin)."})
		case n.HasState("DRAIN") || n.HasState("DRAINING") || n.HasState("DRAINED"):
			add(HealthIssue{Severity: "warning", Kind: "node-drained", Subject: n.Name, Since: since,
				Detail: strings.Join(n.State, "+") + ": " + reason, Advice: "Drained nodes take no new jobs. Confirm the reason is still current."})
		case n.HasState("POWERING_UP") && n.BootTime.Valid() && n.BootTime.Int() > 0 && now-n.BootTime.Int() > 900:
			add(HealthIssue{Severity: "warning", Kind: "slow-boot", Subject: n.Name,
				Detail: "powering up for more than 15 minutes", Advice: "Likely a GCP stockout or a resume failure; check the zone's capacity and the slurm-gcp resume log."})
		}
		if n.PoweredUp() && n.HasState("IDLE") && n.LastBusy.Valid() && n.LastBusy.Int() > 0 && now-n.LastBusy.Int() > 2*3600 {
			add(HealthIssue{Severity: "info", Kind: "idle-billing", Subject: n.Name,
				Detail: fmt.Sprintf("powered up and idle for %.1f h", float64(now-n.LastBusy.Int())/3600),
				Advice: "Idle cloud nodes still bill; check SuspendTime."})
		}
	}

	var q slurm.QueueResponse
	b, err = s.run(ctx, backend.SqueueAll())
	if err != nil {
		return nil, err
	}
	if err := slurm.Decode(b, &q); err != nil {
		return nil, err
	}
	for _, j := range q.Jobs {
		switch j.State() {
		case "PENDING":
			if j.SubmitTime.Valid() && now-j.SubmitTime.Int() > 3600 && j.StateReason != "BeginTime" && j.StateReason != "Dependency" && !strings.HasPrefix(j.StateReason, "JobHeld") {
				m, adv := rules.PendingReason(j.StateReason)
				add(HealthIssue{Severity: "warning", Kind: "long-pending", Subject: fmt.Sprintf("job %d (%s, %s)", j.JobID, j.UserName, j.Partition),
					Since:  numTS(j.SubmitTime),
					Detail: fmt.Sprintf("pending %.1f h, reason %s: %s", float64(now-j.SubmitTime.Int())/3600, j.StateReason, m), Advice: adv})
			}
			if j.StateReason == "launch failed requeued held" || strings.Contains(j.StateReason, "Nodes required for job are DOWN") {
				add(HealthIssue{Severity: "error", Kind: "boot-failures", Subject: fmt.Sprintf("job %d", j.JobID),
					Detail: "reason " + j.StateReason, Advice: "Node launch failed (often a GCP stockout or a bad image). Check the slurm-gcp resume log."})
			}
		case "RUNNING":
			if j.TimeLimit.Valid() && j.StartTime.Valid() && j.TimeLimit.Int() == 0 {
				continue
			}
			if !j.TimeLimit.Valid() && j.StartTime.Valid() && now-j.StartTime.Int() > 7*24*3600 {
				add(HealthIssue{Severity: "warning", Kind: "stuck-job", Subject: fmt.Sprintf("job %d (%s)", j.JobID, j.UserName),
					Detail: fmt.Sprintf("running %.1f days with no time limit", float64(now-j.StartTime.Int())/86400),
					Advice: "Confirm it is still doing work (job_explain_any, node load)."})
			}
		}
	}

	// Failure rate over the last day (everyone).
	ac, err := backend.SacctAll("now-1days", "")
	if err != nil {
		return nil, err
	}
	if b, err := s.run(ctx, ac); err == nil {
		var a slurm.AcctResponse
		if slurm.Decode(b, &a) == nil {
			ended, failed := 0, 0
			nodeFail := map[string]int{}
			for _, j := range a.Jobs {
				st := j.StateName()
				switch st {
				case "RUNNING", "PENDING", "REQUEUED":
					continue
				}
				ended++
				switch st {
				case "FAILED", "NODE_FAIL", "OUT_OF_MEMORY", "TIMEOUT", "BOOT_FAIL":
					failed++
				}
				if st == "NODE_FAIL" || st == "BOOT_FAIL" {
					nodeFail[j.Partition]++
				}
			}
			h.Jobs24h = ended
			if ended > 0 {
				r := round(100*float64(failed)/float64(ended), 1)
				h.FailureRate = &r
				if ended >= 10 && r >= 50 {
					add(HealthIssue{Severity: "warning", Kind: "high-failure-rate", Subject: "last 24 h",
						Detail: fmt.Sprintf("%d of %d jobs failed (%.0f%%)", failed, ended, r),
						Advice: "Look for a shared cause: a broken module, a full filesystem, one user's retry loop (waste_report shows repeats)."})
				}
			}
			for p, n := range nodeFail {
				add(HealthIssue{Severity: "error", Kind: "boot-failures", Subject: "partition " + p,
					Detail: fmt.Sprintf("%d job(s) ended NODE_FAIL/BOOT_FAIL in 24 h", n),
					Advice: "Cloud nodes failed under jobs or did not boot; check GCP quota/stockouts for the zone."})
			}
		}
	}

	sort.SliceStable(h.Issues, func(i, j int) bool { return sevRank(h.Issues[i].Severity) < sevRank(h.Issues[j].Severity) })
	h.OK = true
	for _, i := range h.Issues {
		h.Counts[i.Kind]++
		if i.Severity == "error" {
			h.OK = false
		}
	}
	h.Notes = []string{"Read-only checks over scontrol, squeue and the last day of sacct. Stockouts show up as slow boots, NODE_FAIL/BOOT_FAIL or launch-failed holds; the slurm-gcp resume log on the controller has the GCP error text."}
	return h, nil
}

func sevRank(s string) int {
	switch s {
	case "error":
		return 0
	case "warning":
		return 1
	}
	return 2
}

// ---- ticket draft ------------------------------------------------------------------

// TicketDraft is the ticket_draft answer: everything a staff member needs to
// answer a "my job failed / is stuck" ticket. Nothing is posted anywhere.
type TicketDraft struct {
	JobID        string            `json:"job_id"`
	User         string            `json:"user"`
	Summary      string            `json:"summary"`
	WhatHappened []string          `json:"what_happened"`
	Evidence     []string          `json:"evidence"`
	Fix          []string          `json:"suggested_fix"`
	Reply        string            `json:"reply_draft"`
	InternalNote string            `json:"internal_note"`
	TicketText   *policy.Untrusted `json:"ticket_text_untrusted,omitempty"`
	Confidence   string            `json:"confidence"` // high | medium | low
	Note         string            `json:"note"`
}

// TicketDraftInput selects a ticket draft.
type TicketDraftInput struct {
	JobID      string
	TicketText string // optional: the researcher's message, treated as untrusted
	AnyUser    bool
}

// TicketDraft builds a reply draft from job_explain's findings. The reply is a
// template filled from deterministic findings; a person reviews and sends it.
func (s *Service) TicketDraft(ctx context.Context, in TicketDraftInput) (*TicketDraft, error) {
	ex, err := s.JobExplain(ctx, in.JobID, in.AnyUser, 120)
	if err != nil {
		return nil, err
	}
	d, err := s.JobShow(ctx, JobShowInput{JobID: in.JobID, AnyUser: in.AnyUser})
	if err != nil {
		return nil, err
	}
	t := &TicketDraft{JobID: in.JobID, User: d.User,
		Note: "Draft only: built from deterministic findings, not sent anywhere. Read the evidence and edit before replying."}
	if in.TicketText != "" {
		t.TicketText = policy.Wrap(in.TicketText, 8000)
		markUntrusted(ctx)
	}
	j := ex.Job
	t.WhatHappened = append(t.WhatHappened, fmt.Sprintf("Job %s (%q) on partition %s ended %s after %s, exit code %s.",
		j.JobID, j.Name, j.Partition, j.State, hhmm(j.ElapsedS), firstNonEmpty(j.ExitCode, "n/a")))
	if j.State == "PENDING" || j.State == "RUNNING" {
		t.WhatHappened[0] = fmt.Sprintf("Job %s (%q) on partition %s is %s.", j.JobID, j.Name, j.Partition, j.State)
	}
	if e := d.Efficiency; e != nil && e.MemAllocMB > 0 {
		t.WhatHappened = append(t.WhatHappened, fmt.Sprintf("It used %.0f%% of its CPU time and a peak of %d MB of %d MB memory.", e.CPUPercent, e.MemPeakMB, e.MemAllocMB))
	}
	var errs []rules.Finding
	for _, f := range ex.Findings {
		t.Evidence = append(t.Evidence, fmt.Sprintf("[%s] %s: %s", f.Rule, f.Title, strings.Join(f.Evidence, "; ")))
		if f.Severity == rules.Error || f.Rule == "pending" || f.Rule == "cancelled" {
			errs = append(errs, f)
		}
		t.Fix = append(t.Fix, f.Suggestion)
	}
	if ex.LogPath != "" {
		t.Evidence = append(t.Evidence, "log: "+ex.LogPath)
	}
	completed := jobSucceeded(j)
	switch {
	case len(errs) == 0 && completed:
		t.Confidence = "high"
		t.Summary = fmt.Sprintf("Job %s completed successfully (exit code 0); nothing failed.", j.JobID)
	case len(errs) == 0:
		t.Confidence = "low"
		t.Summary = fmt.Sprintf("Job %s: no known failure pattern found.", j.JobID)
	case errs[0].Rule == "script-error":
		t.Confidence = "low"
		t.Summary = fmt.Sprintf("Job %s failed with exit code %s; no known pattern matched.", j.JobID, j.ExitCode)
	default:
		t.Confidence = "high"
		if len(errs) > 2 {
			t.Confidence = "medium" // several independent errors: say so, a person picks
		}
		t.Summary = fmt.Sprintf("Job %s: %s.", j.JobID, strings.ToLower(errs[0].Title[:1])+errs[0].Title[1:])
	}
	t.Reply = replyText(t, errs, ex)
	t.InternalNote = fmt.Sprintf("bifrost ticket_draft, %d finding(s), confidence %s. Commands: see source in the envelope.", len(ex.Findings), t.Confidence)
	if ex.LogError != "" {
		t.InternalNote += " Log not readable: " + ex.LogError
		if t.Confidence == "high" {
			t.Confidence = "medium"
		}
	}
	return t, nil
}

func replyText(t *TicketDraft, errs []rules.Finding, ex *Explanation) string {
	var b strings.Builder
	b.WriteString("Hi,\n\n")
	b.WriteString("I looked at job " + t.JobID + " on Ursa Major. ")
	b.WriteString(t.WhatHappened[0] + "\n\n")
	if len(errs) == 0 && jobSucceeded(ex.Job) {
		b.WriteString("The job finished normally: Slurm recorded it as COMPLETED with exit code 0, and nothing in the accounting record or the end of the log points to a failure. ")
		if ex.LogPath != "" {
			b.WriteString("Its output log is at " + ex.LogPath + ". ")
		}
		b.WriteString("If the results are not what you expected (missing or wrong output files), could you tell me which files you were looking for and what you expected them to contain?\n\n")
		b.WriteString("Happy to take a closer look from there.\n")
		return b.String()
	}
	if len(errs) == 0 || errs[0].Rule == "script-error" {
		b.WriteString("I could not match the failure to a known cause from the accounting record and the end of the log. ")
		if ex.LogPath != "" {
			b.WriteString("The log is at " + ex.LogPath + "; the first error message near the end is usually the real cause. ")
		}
		b.WriteString("Could you send the exact command or script you ran and what you expected it to do?\n\n")
	} else {
		b.WriteString("What went wrong:\n")
		for _, f := range errs {
			b.WriteString("- " + f.Title)
			for _, ev := range f.Evidence {
				if strings.HasPrefix(ev, "log: ") {
					b.WriteString(" (the log shows: " + strings.TrimPrefix(ev, "log: ") + ")")
					break
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("\nWhat to change:\n")
		seen := map[string]bool{}
		for _, f := range errs {
			if !seen[f.Suggestion] {
				seen[f.Suggestion] = true
				b.WriteString("- " + f.Suggestion + "\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("Let me know if it still fails after that and I will take another look.\n")
	return b.String()
}

// jobSucceeded is true for a job Slurm recorded as COMPLETED with exit code 0.
func jobSucceeded(j JobSummary) bool {
	return j.State == "COMPLETED" && (j.ExitCode == "0" || j.ExitCode == "")
}
