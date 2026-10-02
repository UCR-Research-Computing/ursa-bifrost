package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os/user"
	"path"
	"sort"
	"strings"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/rules"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/slurm"
)

// ---- jobs list -----------------------------------------------------------------

// JobSummary is one row of a job list.
type JobSummary struct {
	JobID     string  `json:"job_id"`
	Name      string  `json:"name"`
	User      string  `json:"user"`
	Partition string  `json:"partition"`
	State     string  `json:"state"`
	Reason    string  `json:"reason,omitempty"`
	Nodes     string  `json:"nodes,omitempty"`
	NodeCount int64   `json:"node_count"`
	CPUs      int64   `json:"cpus"`
	Submitted string  `json:"submitted,omitempty"`
	Started   string  `json:"started,omitempty"`
	Ended     string  `json:"ended,omitempty"`
	ElapsedS  int64   `json:"elapsed_s"`
	ExitCode  string  `json:"exit_code,omitempty"`
	CostUSD   float64 `json:"est_cost_usd,omitempty"`
}

// JobsListInput selects jobs.
// DefaultJobRows is how many jobs a list returns when the caller gives no limit.
const DefaultJobRows = 50

type JobsListInput struct {
	User  string // "" = caller
	All   bool   // every user (R2)
	State string // optional filter, e.g. FAILED or RUNNING
	Since string // default now-7days
	Limit int
}

// JobsList merges the live queue with recent accounting (newest first).
func (s *Service) JobsList(ctx context.Context, in JobsListInput) ([]JobSummary, error) {
	me, err := s.User(ctx)
	if err != nil {
		return nil, err
	}
	who := firstNonEmpty(in.User, me)
	if in.Since == "" {
		in.Since = "now-7days"
	}
	limit := in.Limit
	if limit <= 0 {
		// a default page, not the cap: 200 rows of a busy week was ~70 KB per call
		limit = min(DefaultJobRows, s.Cfg.Limits.ListRows)
	}
	if limit > s.Cfg.Limits.ListRows {
		limit = s.Cfg.Limits.ListRows
	}
	var qc, ac backend.Command
	if in.All {
		qc = backend.SqueueAll()
		ac, err = backend.SacctAll(in.Since, "")
	} else {
		qc, err = backend.SqueueUser(who)
		if err == nil {
			ac, err = backend.SacctUser(who, in.Since, "")
		}
	}
	if err != nil {
		return nil, err
	}
	var q slurm.QueueResponse
	b, err := s.run(ctx, qc)
	if err != nil {
		return nil, err
	}
	if err := slurm.Decode(b, &q); err != nil {
		return nil, err
	}
	var a slurm.AcctResponse
	b, err = s.run(ctx, ac)
	if err != nil {
		return nil, err
	}
	if err := slurm.Decode(b, &a); err != nil {
		return nil, err
	}
	cat, _ := s.Catalog(ctx) // prices are optional
	seen := map[string]bool{}
	var out []JobSummary
	for _, j := range q.Jobs {
		js := s.queueSummary(j, cat)
		seen[js.JobID] = true
		out = append(out, js)
	}
	for _, j := range a.Jobs {
		id := fmt.Sprint(j.JobID)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, s.acctSummary(j, cat))
	}
	if in.State != "" {
		want := strings.ToUpper(in.State)
		var f []JobSummary
		for _, j := range out {
			if j.State == want {
				f = append(f, j)
			}
		}
		out = f
	}
	sort.SliceStable(out, func(i, j int) bool { return jobNum(out[i].JobID) > jobNum(out[j].JobID) })
	if len(out) > limit {
		out = out[:limit]
		markTruncated(ctx)
	}
	if out == nil {
		out = []JobSummary{}
	}
	return out, nil
}

func jobNum(id string) int64 {
	var n int64
	fmt.Sscanf(id, "%d", &n)
	return n
}

func (s *Service) queueSummary(j slurm.QueueJob, cat *Catalog) JobSummary {
	js := JobSummary{
		JobID: fmt.Sprint(j.JobID), Name: policy.CleanLabel(j.Name, 80), User: j.UserName,
		Partition: j.Partition, State: j.State(), Reason: j.StateReason, Nodes: j.Nodes,
		NodeCount: j.NodeCount.Int(), CPUs: j.CPUs.Int(),
		Submitted: numTS(j.SubmitTime), Started: numTS(j.StartTime),
	}
	if js.Reason == "None" {
		js.Reason = ""
	}
	if js.State == "RUNNING" && j.StartTime.Valid() {
		js.ElapsedS = s.Now().Unix() - j.StartTime.Int()
	}
	if p, ok := s.price(cat, j.Partition); ok && s.Cfg.ShowCost && js.ElapsedS > 0 {
		js.CostUSD = round(p*float64(js.NodeCount)*float64(js.ElapsedS)/3600, 2)
	}
	return js
}

func (s *Service) acctSummary(j slurm.AcctJob, cat *Catalog) JobSummary {
	js := JobSummary{
		JobID: fmt.Sprint(j.JobID), Name: policy.CleanLabel(j.Name, 80), User: j.User,
		Partition: j.Partition, State: j.StateName(), Nodes: j.Nodes,
		NodeCount: j.AllocationNodes, CPUs: j.TRES.Allocated.Get("cpu"),
		Submitted: ts(j.Time.Submission), Started: ts(j.Time.Start), Ended: ts(j.Time.End),
		ElapsedS: j.Time.Elapsed, ExitCode: j.ExitCode.String(),
	}
	// Slurm keeps the last pending reason (e.g. BeginTime) on finished jobs; it is
	// noise once a job has run, so only queued jobs show a reason.
	if r := j.State.Reason; r != "" && r != "None" && js.State == "PENDING" {
		js.Reason = r
	}
	if p, ok := s.price(cat, j.Partition); ok && s.Cfg.ShowCost && js.ElapsedS > 0 {
		js.CostUSD = round(p*float64(max(js.NodeCount, 1))*float64(js.ElapsedS)/3600, 2)
	}
	return js
}

// ---- job show ---------------------------------------------------------------------

// JobDetail merges squeue (if still queued/running) and sacct.
type JobDetail struct {
	JobSummary
	TimeLimitMin  int64              `json:"time_limit_min,omitempty"`
	Account       string             `json:"account,omitempty"`
	WorkDir       string             `json:"work_dir,omitempty"`
	Stdout        string             `json:"stdout,omitempty"`
	Stderr        string             `json:"stderr,omitempty"`
	SubmitLine    *policy.Untrusted  `json:"submit_line_untrusted,omitempty"`
	Requested     map[string]int64   `json:"requested_tres,omitempty"`
	Allocated     map[string]int64   `json:"allocated_tres,omitempty"`
	Efficiency    *Efficiency        `json:"efficiency,omitempty"`
	Steps         []StepSummary      `json:"steps,omitempty"`
	RestartCount  int64              `json:"restart_count,omitempty"`
	KilledBy      string             `json:"cancelled_by,omitempty"`
	PendingReason *PendingExplain    `json:"pending,omitempty"`
	Script        *policy.Untrusted  `json:"script_untrusted,omitempty"`
	Raw           map[string]any     `json:"-"`
	acct          *slurm.AcctJob     `json:"-"`
	queue         *slurm.QueueJob    `json:"-"`
	extra         map[string]float64 `json:"-"`
}

// Efficiency compares what the job asked for with what it used.
type Efficiency struct {
	CPUPercent   float64 `json:"cpu_percent"`
	CPUSeconds   float64 `json:"cpu_seconds"`
	CoreSeconds  float64 `json:"core_seconds_allocated"`
	MemPeakMB    int64   `json:"mem_peak_mb"`
	MemAllocMB   int64   `json:"mem_alloc_mb"`
	MemPercent   float64 `json:"mem_percent"`
	Note         string  `json:"note,omitempty"`
	WholeNodeFee bool    `json:"whole_node_billing"`
}

// StepSummary is one job step.
type StepSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	ExitCode  string `json:"exit_code,omitempty"`
	ElapsedS  int64  `json:"elapsed_s"`
	MaxRSSMB  int64  `json:"max_rss_mb,omitempty"`
	CPUSecond int64  `json:"cpu_seconds,omitempty"`
}

// PendingExplain decodes why a job waits.
type PendingExplain struct {
	Reason  string `json:"reason"`
	Meaning string `json:"meaning"`
	Advice  string `json:"advice"`
}

// JobShowInput selects one job.
type JobShowInput struct {
	JobID         string
	AnyUser       bool // R2: allow other users' jobs
	IncludeScript bool // R2 only for other users
}

// JobShow returns the merged record of one job.
func (s *Service) JobShow(ctx context.Context, in JobShowInput) (*JobDetail, error) {
	if err := backend.ValidJobID(in.JobID); err != nil {
		return nil, err
	}
	me, err := s.User(ctx)
	if err != nil {
		return nil, err
	}
	var d JobDetail
	qc, _ := backend.SqueueJob(in.JobID)
	if b, err := s.run(ctx, qc); err == nil {
		var q slurm.QueueResponse
		if slurm.Decode(b, &q) == nil && len(q.Jobs) > 0 {
			j := q.Jobs[0]
			d.queue = &j
		}
	}
	ac, _ := backend.SacctJob(in.JobID)
	b, err := s.run(ctx, ac)
	if err != nil {
		return nil, err
	}
	var a slurm.AcctResponse
	if err := slurm.Decode(b, &a); err != nil {
		return nil, err
	}
	for i := range a.Jobs {
		if fmt.Sprint(a.Jobs[i].JobID) == strings.SplitN(in.JobID, "_", 2)[0] {
			d.acct = &a.Jobs[i]
			break
		}
	}
	if d.acct == nil && d.queue == nil {
		return nil, fmt.Errorf("job %s not found (accounting keeps finished jobs; check the id)", in.JobID)
	}
	owner := ""
	if d.acct != nil {
		owner = d.acct.User
	} else {
		owner = d.queue.UserName
	}
	if owner != me && !in.AnyUser {
		return nil, fmt.Errorf("%w: job %s belongs to another user; staff tier R2 can read it with the *_any tools", policy.ErrDenied, in.JobID)
	}
	cat, _ := s.Catalog(ctx)
	if d.acct != nil {
		d.JobSummary = s.acctSummary(*d.acct, cat)
		j := d.acct
		d.Account = j.Account
		d.WorkDir = j.WorkDir
		d.Stdout = j.StdoutExpanded
		d.Stderr = j.StderrExpanded
		d.TimeLimitMin = j.Time.Limit.Int()
		d.Requested = tresMap(j.TRES.Requested)
		d.Allocated = tresMap(j.TRES.Allocated)
		d.RestartCount = j.RestartCnt
		d.KilledBy = j.KillRequestUser
		if j.SubmitLine != "" {
			d.SubmitLine = policy.Wrap(j.SubmitLine, 2000)
			markUntrusted(ctx)
		}
		for _, st := range j.Steps {
			d.Steps = append(d.Steps, StepSummary{
				ID: st.Step.ID, Name: st.Step.Name, State: strings.Join(st.State, ","),
				ExitCode: st.ExitCode.String(), ElapsedS: st.Time.Elapsed,
				MaxRSSMB: st.MaxRSSBytes() / (1024 * 1024), CPUSecond: int64(st.Time.Total.Float()),
			})
		}
		if len(d.Steps) > 20 {
			d.Steps = d.Steps[:20]
			markTruncated(ctx)
		}
		d.Efficiency = efficiency(j)
		if in.IncludeScript && j.Script != "" {
			d.Script = policy.Wrap(j.Script, s.Cfg.Limits.ScriptBytes)
			markUntrusted(ctx)
		}
	}
	if q := d.queue; q != nil { // live state wins
		live := s.queueSummary(*q, cat)
		d.State, d.Reason, d.Nodes, d.ElapsedS = live.State, live.Reason, live.Nodes, live.ElapsedS
		if live.CostUSD > 0 {
			d.CostUSD = live.CostUSD
		}
		if d.acct == nil {
			d.JobSummary = live
			d.Stdout, d.Stderr, d.WorkDir = q.StdoutExpanded, q.StderrExpanded, q.WorkDir
			d.TimeLimitMin = q.TimeLimit.Int()
		}
		if d.State == "PENDING" {
			m, adv := rules.PendingReason(q.StateReason)
			d.PendingReason = &PendingExplain{Reason: q.StateReason, Meaning: m, Advice: adv}
		}
	}
	return &d, nil
}

func tresMap(l slurm.TRESList) map[string]int64 {
	m := map[string]int64{}
	for _, t := range l {
		switch t.Type {
		case "cpu", "mem", "node", "gres":
			m[t.Key()] = t.Count
		}
	}
	return m
}

func efficiency(j *slurm.AcctJob) *Efficiency {
	cpus := j.TRES.Allocated.Get("cpu")
	if cpus == 0 || j.Time.Elapsed == 0 {
		return nil
	}
	e := &Efficiency{
		CPUSeconds:   round(j.Time.Total.Float(), 1),
		CoreSeconds:  float64(cpus * j.Time.Elapsed),
		MemPeakMB:    j.MaxRSSBytes() / (1024 * 1024),
		MemAllocMB:   j.TRES.Allocated.Get("mem"),
		WholeNodeFee: true,
	}
	e.CPUPercent = round(100*e.CPUSeconds/e.CoreSeconds, 1)
	if e.MemAllocMB > 0 {
		e.MemPercent = round(100*float64(e.MemPeakMB)/float64(e.MemAllocMB), 1)
	}
	if e.MemPeakMB == 0 {
		e.Note = "peak memory not recorded for this job"
	}
	return e
}

// ---- explain --------------------------------------------------------------------

// Explanation is job_explain's answer.
type Explanation struct {
	Job      JobSummary        `json:"job"`
	Findings []rules.Finding   `json:"findings"`
	LogPath  string            `json:"log_path,omitempty"`
	LogTail  *policy.Untrusted `json:"log_tail_untrusted,omitempty"`
	LogError string            `json:"log_error,omitempty"`
	Note     string            `json:"note"`
}

// JobExplain runs the diagnosis rules over a job's record and log tail.
func (s *Service) JobExplain(ctx context.Context, jobID string, anyUser bool, lines int) (*Explanation, error) {
	d, err := s.JobShow(ctx, JobShowInput{JobID: jobID, AnyUser: anyUser})
	if err != nil {
		return nil, err
	}
	if lines <= 0 {
		lines = 80
	}
	if lines > 200 { // the rules only need the end of the log
		lines = 200
	}
	f := rules.Facts{State: d.State, Reason: d.Reason, ExitCode: d.ExitCode, Partition: d.Partition,
		ElapsedSec: d.ElapsedS, LimitMin: d.TimeLimitMin, CPUsAlloc: d.CPUs, RestartCnt: d.RestartCount,
		KilledBy: d.KilledBy}
	if e := d.Efficiency; e != nil {
		f.CPUSeconds, f.MemReqMB, f.MaxRSSMB = e.CPUSeconds, e.MemAllocMB, e.MemPeakMB
	}
	f.GPUsAlloc = d.Allocated["gres/gpu"]
	if cat, err := s.Catalog(ctx); err == nil {
		if p, ok := cat.Partition(d.Partition); ok {
			f.SpotPartition = p.Spot
			// GPUs are not in AccountingStorageTRES on Ursa Major; partitions are
			// whole-node, so a gpul4 job holds the node's GPU.
			if f.GPUsAlloc == 0 && p.GPUsPerNode > 0 {
				f.GPUsAlloc = int64(p.GPUsPerNode) * max(d.NodeCount, 1)
			}
		}
	}
	ex := &Explanation{Job: d.JobSummary, Note: "Deterministic rules over accounting data and the log tail; no model involved. Verify before telling a user."}
	if d.State != "PENDING" {
		tail, p, err := s.readLog(ctx, d, "stdout", lines)
		ex.LogPath = p
		if err != nil {
			ex.LogError = err.Error()
		} else {
			f.Log, f.LogReadable = tail, true
			ex.LogTail = policy.Wrap(tail, s.Cfg.Limits.UntrustedCh)
			markUntrusted(ctx)
		}
	}
	ex.Findings = rules.Explain(f)
	if ex.Findings == nil {
		ex.Findings = []rules.Finding{}
	}
	return ex, nil
}

// ---- logs -------------------------------------------------------------------------

// LogTail is job_log_tail's answer: a window of the log, or a search.
type LogTail struct {
	JobID      string            `json:"job_id"`
	Stream     string            `json:"stream"`
	Path       string            `json:"path"`
	Lines      int               `json:"lines"`                // lines returned
	TotalLines int               `json:"total_lines"`          // lines in the log
	FirstLine  int               `json:"first_line,omitempty"` // 1-based number of the first line returned
	LastLine   int               `json:"last_line,omitempty"`  // number of the last line returned
	Tail       *policy.Untrusted `json:"untrusted,omitempty"`  // the window
	Grep       *GrepResult       `json:"grep,omitempty"`       // matches with line numbers
	Next       string            `json:"next,omitempty"`       // how to page
}

// LogInput selects a log window or search.
type LogInput struct {
	JobID     string
	Stream    string
	Lines     int    // window size (default 100, cap limits.log_lines)
	StartLine int    // 0 = the last Lines lines; otherwise from this line
	Grep      string // extended regex: matching lines anywhere in the log
	AnyUser   bool
}

// JobLogTail returns the redacted tail of a job's stdout or stderr.
func (s *Service) JobLogTail(ctx context.Context, jobID, stream string, lines int, anyUser bool) (*LogTail, error) {
	return s.JobLog(ctx, LogInput{JobID: jobID, Stream: stream, Lines: lines, AnyUser: anyUser})
}

// JobLog returns a window of a job's log (the tail, or from a line) or the
// lines matching a pattern, with line numbers for paging (SPEC 18.3).
func (s *Service) JobLog(ctx context.Context, in LogInput) (*LogTail, error) {
	stream := in.Stream
	if stream == "" {
		stream = "stdout"
	}
	if stream != "stdout" && stream != "stderr" {
		return nil, fmt.Errorf("stream must be stdout or stderr")
	}
	lines := in.Lines
	if lines <= 0 {
		lines = 100
	}
	if lines > s.Cfg.Limits.LogLines {
		lines = s.Cfg.Limits.LogLines
		markTruncated(ctx)
	}
	if in.StartLine < 0 {
		return nil, fmt.Errorf("start_line must be 1 or more")
	}
	if in.Grep != "" {
		if err := backend.ValidPattern(in.Grep); err != nil {
			return nil, err
		}
	}
	d, err := s.JobShow(ctx, JobShowInput{JobID: in.JobID, AnyUser: in.AnyUser})
	if err != nil {
		return nil, err
	}
	p, err := s.logPath(d, stream)
	if err != nil {
		return nil, err
	}
	lt := &LogTail{JobID: in.JobID, Stream: stream, Path: p}
	markUntrusted(ctx)
	if in.Grep != "" {
		g, err := s.grepFile(ctx, p, in.Grep)
		if err != nil {
			return nil, fmt.Errorf("searching %s: %w", p, err)
		}
		lt.Grep, lt.TotalLines = g, g.TotalLines
		return lt, nil
	}
	c, err := backend.LogWindow(p, in.StartLine, lines)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", p, err)
	}
	head, body, _ := strings.Cut(string(out), "\n")
	var total, first int
	if _, err := fmt.Sscanf(head, "%d %d", &total, &first); err != nil {
		return nil, fmt.Errorf("unexpected answer reading %s", p)
	}
	if len(body) > pageChars { // keep whole lines from the end of the window
		cut := strings.IndexByte(body[len(body)-pageChars:], '\n')
		dropped := body[:len(body)-pageChars+cut+1]
		first += strings.Count(dropped, "\n")
		body = body[len(dropped):]
		markTruncated(ctx)
	}
	n := strings.Count(body, "\n")
	if body != "" && !strings.HasSuffix(body, "\n") {
		n++
	}
	lt.TotalLines, lt.Lines, lt.Tail = total, n, policy.Wrap(body, 0)
	if n > 0 {
		lt.FirstLine, lt.LastLine = first, first+n-1
	}
	switch {
	case lt.FirstLine > 1:
		lt.Next = fmt.Sprintf("Lines %d-%d of %d. Earlier: start_line=%d. Search the whole log with grep.", lt.FirstLine, lt.LastLine, total, max(1, lt.FirstLine-lines))
	case lt.LastLine < total:
		lt.Next = fmt.Sprintf("Lines %d-%d of %d. Later: start_line=%d.", lt.FirstLine, lt.LastLine, total, lt.LastLine+1)
	}
	return lt, nil
}

// logPath picks the job's log path from its record and checks the roots.
func (s *Service) logPath(d *JobDetail, stream string) (string, error) {
	p := d.Stdout
	if stream == "stderr" {
		p = firstNonEmpty(d.Stderr, d.Stdout)
	}
	if p == "" {
		return "", fmt.Errorf("the job record has no %s path (interactive job?)", stream)
	}
	if err := s.checkLogPath(p, d.User); err != nil {
		return p, err
	}
	return p, nil
}

// readLog resolves the log path from the job record (never from the caller) and
// checks it against the allowed roots for the job's owner.
func (s *Service) readLog(ctx context.Context, d *JobDetail, stream string, lines int) (string, string, error) {
	p, err := s.logPath(d, stream)
	if err != nil {
		return "", p, err
	}
	c, err := backend.Tail(p, lines)
	if err != nil {
		return "", p, err
	}
	b, err := s.run(ctx, c)
	if err != nil {
		return "", p, fmt.Errorf("reading %s: %w", p, err)
	}
	return string(b), p, nil
}

func (s *Service) checkLogPath(p, owner string) error {
	clean := path.Clean(p)
	if clean != p || !path.IsAbs(p) {
		return fmt.Errorf("%w: log path %q is not a clean absolute path", policy.ErrDenied, p)
	}
	for _, root := range s.Cfg.LogRoots {
		r := strings.ReplaceAll(root, "{user}", owner)
		if !strings.HasSuffix(r, "/") {
			r += "/"
		}
		if strings.HasPrefix(clean, r) {
			return nil
		}
	}
	return fmt.Errorf("%w: log %s is outside the allowed roots %v for user %s", policy.ErrDenied, p, s.Cfg.LogRoots, owner)
}

func localUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

func approxSize(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}
