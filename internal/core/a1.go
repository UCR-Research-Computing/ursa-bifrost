package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/slurm"
)

// The A1 tier changes cluster state. Every action is two-step:
//
//  1. prepare: validate, check caps, compute the exact action, store it under a
//     random single-use token and return a human-readable plan;
//  2. confirm: with the token (and nothing else), run exactly the stored action.
//
// The token is bound to the plan's hash, expires after confirm_ttl_minutes, and is
// deleted when used. A model cannot change the script, partition or size between
// the two steps, because the confirm step does not accept them.

// ErrCapExceeded means a plan is over a configured cap.
var ErrCapExceeded = errors.New("over cap")

// SubmitPlan is what job_submit returns before confirmation.
type SubmitPlan struct {
	Token        string   `json:"confirm_token"`
	ExpiresAt    string   `json:"expires_at"`
	PlanHash     string   `json:"plan_hash"`
	Partition    string   `json:"partition"`
	Nodes        int      `json:"nodes"`
	TimeLimit    string   `json:"time_limit"`
	JobName      string   `json:"job_name"`
	RemoteDir    string   `json:"remote_dir"`
	ScriptBytes  int      `json:"script_bytes"`
	ScriptSHA256 string   `json:"script_sha256"`
	USDPerNodeH  float64  `json:"usd_per_node_hour,omitempty"`
	WorstCaseUSD float64  `json:"worst_case_usd"`
	SpentTodayUS float64  `json:"committed_today_usd"`
	DayCapUSD    float64  `json:"day_cap_usd"`
	Scheduler    string   `json:"scheduler_test"` // sbatch --test-only answer
	Warnings     []string `json:"warnings"`
	Overrides    []string `json:"overrides"` // #SBATCH values bifrost replaced
	Next         string   `json:"next"`
}

// ActionPlan is what job_cancel/hold/release return before confirmation.
type ActionPlan struct {
	Token     string `json:"confirm_token"`
	ExpiresAt string `json:"expires_at"`
	Action    string `json:"action"`
	JobID     string `json:"job_id"`
	JobName   string `json:"job_name"`
	State     string `json:"state"`
	Effect    string `json:"effect"`
	Next      string `json:"next"`
}

// Confirmed is the result of a confirmed action.
type Confirmed struct {
	Action    string  `json:"action"`
	JobID     string  `json:"job_id"`
	RemoteDir string  `json:"remote_dir,omitempty"`
	Committed float64 `json:"committed_usd,omitempty"`
	Message   string  `json:"message"`
	Next      string  `json:"next,omitempty"`
}

// SubmitInput is a submit request.
type SubmitInput struct {
	Script    string
	Partition string // overrides #SBATCH -p
	Nodes     int    // overrides #SBATCH -N
	Time      string // overrides #SBATCH -t (Slurm format)
	JobName   string // overrides #SBATCH -J
}

// pending is one stored action awaiting confirmation.
type pending struct {
	Kind     string    `json:"kind"` // submit | cancel | hold | release
	Hash     string    `json:"hash"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	JobID    string    `json:"job_id,omitempty"`
	Dir      string    `json:"dir,omitempty"`
	Script   string    `json:"script,omitempty"`
	Opts     subOpts   `json:"opts,omitempty"`
	WorstUSD float64   `json:"worst_usd,omitempty"`
}

type subOpts struct {
	Partition string `json:"partition"`
	Nodes     int    `json:"nodes"`
	TimeMin   int    `json:"time_min"`
	JobName   string `json:"job_name"`
	Comment   string `json:"comment"`
}

// ledgerEntry records one submitted job's worst-case cost (for the day cap).
type ledgerEntry struct {
	Time     time.Time `json:"time"`
	JobID    string    `json:"job_id"`
	WorstUSD float64   `json:"worst_usd"`
	Dir      string    `json:"dir"`
	Hash     string    `json:"hash"`
}

type a1State struct {
	Pending map[string]pending `json:"pending"` // key: sha256(token)
	Ledger  []ledgerEntry      `json:"ledger"`
}

var stateMu sync.Mutex

func (s *Service) statePath() string { return config.Expand(s.Cfg.StatePath) }

func (s *Service) loadState() (*a1State, error) {
	st := &a1State{Pending: map[string]pending{}}
	b, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("A1 state file %s is corrupt: %w", s.statePath(), err)
	}
	if st.Pending == nil {
		st.Pending = map[string]pending{}
	}
	return st, nil
}

func (s *Service) saveState(st *a1State) error {
	now := s.Now()
	for k, p := range st.Pending { // drop expired tokens
		if now.After(p.Expires) {
			delete(st.Pending, k)
		}
	}
	cut := now.Add(-40 * 24 * time.Hour) // keep ~a month of ledger
	keep := st.Ledger[:0]
	for _, e := range st.Ledger {
		if e.Time.After(cut) {
			keep = append(keep, e)
		}
	}
	st.Ledger = keep
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	p := s.statePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Service) committedToday(st *a1State) (usd float64, n int) {
	now := s.Now()
	y, m, d := now.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	for _, e := range st.Ledger {
		if !e.Time.Before(start) {
			usd += e.WorstUSD
			n++
		}
	}
	return round(usd, 2), n
}

func newToken() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bf1-" + hex.EncodeToString(b), nil
}

func tokenKey(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var reStartAt = regexp.MustCompile(`to start at (\S+) .* on nodes (\S+) in partition (\S+)`)

// PrepareSubmit validates a script, applies caps, asks the scheduler with
// sbatch --test-only and stores the exact submission under a confirm token.
func (s *Service) PrepareSubmit(ctx context.Context, in SubmitInput) (*SubmitPlan, error) {
	if strings.TrimSpace(in.Script) == "" {
		return nil, errors.New("empty script")
	}
	sc, err := s.ScriptCheck(ctx, in.Script)
	if err != nil {
		return nil, err
	}
	cat, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	req := sc.Request
	plan := &SubmitPlan{Warnings: []string{}, Overrides: []string{}}

	part := firstNonEmpty(in.Partition, req["partition"], cat.DefaultPartition())
	nodes := in.Nodes
	if nodes == 0 {
		nodes = atoiDefault(req["nodes"], 1)
	}
	timeStr := firstNonEmpty(in.Time, req["time"])
	mins, ok := slurmMinutes(timeStr)
	if !ok || mins <= 0 {
		return nil, errors.New("a time limit is required (#SBATCH -t or the time argument): bifrost will not start a job that can run without end")
	}
	name := firstNonEmpty(in.JobName, req["job-name"], "bifrost-job")
	name = regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(name, "-")
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" || !regexp.MustCompile(`^[A-Za-z0-9]`).MatchString(name) {
		name = "bifrost-" + name
	}
	for k, v := range map[string]string{"partition": part, "nodes": strconv.Itoa(nodes), "time": strconv.Itoa(mins) + " min"} {
		if orig := req[k]; orig != "" && ((k == "partition" && orig != part) || (k == "nodes" && atoiDefault(orig, 1) != nodes) || (k == "time" && !sameMinutes(orig, mins))) {
			plan.Overrides = append(plan.Overrides, fmt.Sprintf("#SBATCH %s %s replaced by %s", k, orig, v))
		}
	}
	sort.Strings(plan.Overrides)

	// static check errors (bad module, too many cores, missing GPU partition) block the plan
	var blocking []string
	for _, is := range sc.Issues {
		switch is.Severity {
		case "error":
			if strings.Contains(is.Message, "partition") && in.Partition != "" {
				continue // re-checked below against the override
			}
			blocking = append(blocking, is.Message)
		case "warning":
			if !strings.Contains(is.Message, "no --time") {
				plan.Warnings = append(plan.Warnings, is.Message)
			}
		}
	}
	cp, ok := cat.Partition(part)
	if !ok {
		blocking = append(blocking, fmt.Sprintf("partition %q does not exist", part))
	}
	if len(blocking) > 0 {
		return nil, fmt.Errorf("script_check found errors; fix them first: %s", strings.Join(blocking, "; "))
	}

	// caps
	caps := s.Cfg.Caps
	if nodes > caps.MaxNodes {
		return nil, fmt.Errorf("%w: %d nodes requested, cap is %d (caps.max_nodes)", ErrCapExceeded, nodes, caps.MaxNodes)
	}
	if nodes > cp.MaxNodes {
		return nil, fmt.Errorf("%d nodes requested; partition %s has %d", nodes, part, cp.MaxNodes)
	}
	if float64(mins) > caps.MaxHours*60 {
		return nil, fmt.Errorf("%w: time limit %d min is over the %.0f h cap (caps.max_hours)", ErrCapExceeded, mins, caps.MaxHours)
	}
	price, priced := s.price(cat, part)
	if !priced {
		return nil, fmt.Errorf("no price known for partition %s, so the cost cap cannot be checked; add it under usd_per_node_hour", part)
	}
	worst := round(price*float64(nodes)*float64(mins)/60, 2)
	if worst > caps.MaxCostPerJobUSD {
		return nil, fmt.Errorf("%w: worst case $%.2f (%d node(s) x %d min x $%.2f/node-h) is over the $%.2f per-job cap", ErrCapExceeded, worst, nodes, mins, price, caps.MaxCostPerJobUSD)
	}

	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := s.loadState()
	if err != nil {
		return nil, err
	}
	spent, n := s.committedToday(st)
	if spent+worst > caps.MaxCostPerDayUSD {
		return nil, fmt.Errorf("%w: today's committed worst case is $%.2f; this job adds $%.2f, over the $%.2f day cap", ErrCapExceeded, spent, worst, caps.MaxCostPerDayUSD)
	}
	if caps.MaxSubmitsPerDay > 0 && n >= caps.MaxSubmitsPerDay {
		return nil, fmt.Errorf("%w: %d submissions today (cap %d)", ErrCapExceeded, n, caps.MaxSubmitsPerDay)
	}

	scriptHash := hashOf(in.Script)
	stamp := s.Now().Format("20060102-150405")
	slug := strings.ToLower(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(name), "-"))
	slug = strings.Trim(slug, "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "job"
	}
	dir := s.Cfg.JobsRoot + "/" + stamp + "-" + slug
	opts := subOpts{Partition: part, Nodes: nodes, TimeMin: mins, JobName: name}
	planHash := hashOf("submit", dir, part, strconv.Itoa(nodes), strconv.Itoa(mins), name, scriptHash)
	opts.Comment = "bifrost:" + planHash[:12]

	// ask the scheduler (no submission)
	tc, err := backend.SbatchTestOnly(backend.SubmitOpts(opts), []byte(in.Script))
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, tc)
	if err != nil {
		return nil, fmt.Errorf("the scheduler rejected the job (sbatch --test-only): %v", err)
	}
	ans := strings.TrimSpace(string(out))
	if m := reStartAt.FindStringSubmatch(ans); m != nil {
		plan.Scheduler = fmt.Sprintf("would start at %s on %s (%s)", m[1], m[2], m[3])
	} else {
		plan.Scheduler = policy.CleanLabel(ans, 300)
	}
	// sbatch --test-only does not know about GCP stockouts: check the partition's
	// nodes for boot failures before the user commits money to a job that will wait.
	if w := s.partitionTrouble(ctx, part); w != "" {
		plan.Warnings = append(plan.Warnings, w)
	}

	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	exp := s.Now().Add(time.Duration(caps.ConfirmTTLMinutes) * time.Minute)
	st.Pending[tokenKey(tok)] = pending{Kind: "submit", Hash: planHash, Created: s.Now(), Expires: exp,
		Dir: dir, Script: in.Script, Opts: opts, WorstUSD: worst}
	if err := s.saveState(st); err != nil {
		return nil, err
	}

	*plan = SubmitPlan{Token: tok, ExpiresAt: exp.Format(time.RFC3339), PlanHash: planHash[:12],
		Partition: part, Nodes: nodes, TimeLimit: minutesText(mins), JobName: name, RemoteDir: "~/" + dir,
		ScriptBytes: len(in.Script), ScriptSHA256: scriptHash[:16], USDPerNodeH: price, WorstCaseUSD: worst,
		SpentTodayUS: spent, DayCapUSD: caps.MaxCostPerDayUSD, Scheduler: plan.Scheduler,
		Warnings: plan.Warnings, Overrides: plan.Overrides,
		Next: "Show this plan to the user. Only if they approve, call job_submit_confirm with the confirm_token. Nothing has been submitted yet."}
	if !s.Cfg.ShowCost {
		plan.USDPerNodeH = 0
	}
	return plan, nil
}

// partitionTrouble reports recent boot failures (stockouts) on a partition and
// suggests a partition with a node already up or no failures, or "" when fine.
func (s *Service) partitionTrouble(ctx context.Context, part string) string {
	b, err := s.run(ctx, backend.Nodes())
	if err != nil {
		return ""
	}
	var nr slurm.NodesResponse
	if slurm.Decode(b, &nr) != nil {
		return ""
	}
	bad, stockout := 0, false
	upIdle := map[string]bool{}
	troubled := map[string]bool{}
	for _, n := range nr.Nodes {
		down := n.HasState("DOWN") || n.HasState("NOT_RESPONDING") || n.HasState("FAIL")
		for _, p := range n.Partitions {
			if down {
				troubled[p] = true
			}
			if n.PoweredUp() && n.HasState("IDLE") && !n.HasState("POWERING_UP") {
				upIdle[p] = true
			}
		}
		if down && contains(n.Partitions, part) {
			bad++
			if strings.Contains(n.Reason, "RESOURCE_POOL_EXHAUSTED") || strings.Contains(n.Reason, "stockout") {
				stockout = true
			}
		}
	}
	if bad == 0 {
		return ""
	}
	why := fmt.Sprintf("%d %s node(s) failed to start recently", bad, part)
	if stockout {
		why = fmt.Sprintf("GCP has no capacity for %s right now (%d node(s) failed with ZONE_RESOURCE_POOL_EXHAUSTED); the job may wait a long time", part, bad)
	}
	alt := ""
	for _, p := range []string{"computehigh", "standard", "spot", "nvmescratch", "highmem"} {
		if p != part && upIdle[p] {
			alt = p + " (a node is already up and idle)"
			break
		}
	}
	if alt == "" {
		for _, p := range []string{"computehigh", "standard", "spot"} {
			if p != part && !troubled[p] {
				alt = p
				break
			}
		}
	}
	if alt != "" {
		why += "; consider partition " + alt
	}
	return why
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func sameMinutes(slurmTime string, mins int) bool {
	m, ok := slurmMinutes(slurmTime)
	return ok && m == mins
}

func minutesText(m int) string {
	if m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %02d min", m/60, m%60)
}

// take removes and returns a pending action if the token is valid.
func (s *Service) take(st *a1State, tok, kind string) (pending, error) {
	key := tokenKey(strings.TrimSpace(tok))
	p, ok := st.Pending[key]
	if !ok {
		return pending{}, errors.New("unknown or already used confirm_token: prepare the action again")
	}
	delete(st.Pending, key) // single use, even when it then fails
	if s.Now().After(p.Expires) {
		return pending{}, errors.New("confirm_token expired: prepare the action again")
	}
	if p.Kind != kind {
		return pending{}, fmt.Errorf("this confirm_token is for %s, not %s", p.Kind, kind)
	}
	return p, nil
}

// ConfirmSubmit runs exactly the stored submission for a token.
func (s *Service) ConfirmSubmit(ctx context.Context, tok string) (*Confirmed, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := s.loadState()
	if err != nil {
		return nil, err
	}
	p, err := s.take(st, tok, "submit")
	if err != nil {
		_ = s.saveState(st)
		return nil, err
	}
	// integrity: the stored action must still hash to the plan
	if hashOf("submit", p.Dir, p.Opts.Partition, strconv.Itoa(p.Opts.Nodes), strconv.Itoa(p.Opts.TimeMin), p.Opts.JobName, hashOf(p.Script)) != p.Hash {
		_ = s.saveState(st)
		return nil, errors.New("stored plan does not match its hash; refusing")
	}
	// re-check the day cap at confirm time (another submit may have landed)
	spent, _ := s.committedToday(st)
	if spent+p.WorstUSD > s.Cfg.Caps.MaxCostPerDayUSD {
		_ = s.saveState(st)
		return nil, fmt.Errorf("%w: day cap reached since the plan was made ($%.2f committed)", ErrCapExceeded, spent)
	}
	c, err := backend.SubmitBatch(p.Dir, backend.SubmitOpts(p.Opts), []byte(p.Script))
	if err != nil {
		_ = s.saveState(st)
		return nil, err
	}
	out, runErr := s.run(ctx, c)
	if runErr != nil {
		_ = s.saveState(st)
		return nil, fmt.Errorf("sbatch failed: %w", runErr)
	}
	id := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), ";", 2)[0])
	if backend.ValidJobID(id) != nil {
		_ = s.saveState(st)
		return nil, fmt.Errorf("sbatch returned an unexpected answer: %q", policy.CleanLabel(string(out), 200))
	}
	st.Ledger = append(st.Ledger, ledgerEntry{Time: s.Now(), JobID: id, WorstUSD: p.WorstUSD, Dir: p.Dir, Hash: p.Hash})
	if err := s.saveState(st); err != nil {
		return nil, fmt.Errorf("job %s was submitted but the ledger could not be saved: %w", id, err)
	}
	s.invalidateQueue()
	return &Confirmed{Action: "submit", JobID: id, RemoteDir: "~/" + p.Dir, Committed: p.WorstUSD,
		Message: fmt.Sprintf("Submitted job %s (%s, %d node(s), %s).", id, p.Opts.Partition, p.Opts.Nodes, minutesText(p.Opts.TimeMin)),
		Next:    fmt.Sprintf("Follow with job_show %s; when it ends, job_results %s lists and downloads the outputs.", id, id)}, nil
}

// PrepareAction stores a cancel/hold/release of one of the caller's jobs.
func (s *Service) PrepareAction(ctx context.Context, kind, jobID string) (*ActionPlan, error) {
	if kind != "cancel" && kind != "hold" && kind != "release" {
		return nil, fmt.Errorf("unknown action %q", kind)
	}
	d, err := s.JobShow(ctx, JobShowInput{JobID: jobID}) // own jobs only: denies other users
	if err != nil {
		return nil, err
	}
	switch kind {
	case "cancel":
		if d.State != "PENDING" && d.State != "RUNNING" && d.State != "SUSPENDED" && d.State != "CONFIGURING" {
			return nil, fmt.Errorf("job %s is %s; only pending or running jobs can be cancelled", jobID, d.State)
		}
	case "hold":
		if d.State != "PENDING" {
			return nil, fmt.Errorf("job %s is %s; only pending jobs can be held", jobID, d.State)
		}
	case "release":
		if d.State != "PENDING" {
			return nil, fmt.Errorf("job %s is %s; only held (pending) jobs can be released", jobID, d.State)
		}
	}
	effect := map[string]string{
		"cancel":  "The job stops now (running work is killed; partial outputs stay in its folder).",
		"hold":    "The job stays queued but will not start until released.",
		"release": "The held job becomes eligible to start.",
	}[kind]
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := s.loadState()
	if err != nil {
		return nil, err
	}
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	exp := s.Now().Add(time.Duration(s.Cfg.Caps.ConfirmTTLMinutes) * time.Minute)
	st.Pending[tokenKey(tok)] = pending{Kind: kind, Hash: hashOf(kind, jobID), Created: s.Now(), Expires: exp, JobID: jobID}
	if err := s.saveState(st); err != nil {
		return nil, err
	}
	return &ActionPlan{Token: tok, ExpiresAt: exp.Format(time.RFC3339), Action: kind, JobID: jobID,
		JobName: d.Name, State: d.State, Effect: effect,
		Next: fmt.Sprintf("Show this to the user. Only if they approve, call job_%s_confirm with the confirm_token.", kind)}, nil
}

// ConfirmAction runs a stored cancel/hold/release.
func (s *Service) ConfirmAction(ctx context.Context, kind, tok string) (*Confirmed, error) {
	stateMu.Lock()
	st, err := s.loadState()
	if err != nil {
		stateMu.Unlock()
		return nil, err
	}
	p, err := s.take(st, tok, kind)
	_ = s.saveState(st)
	stateMu.Unlock()
	if err != nil {
		return nil, err
	}
	// ownership is re-checked at confirm time
	if _, err := s.JobShow(ctx, JobShowInput{JobID: p.JobID}); err != nil {
		return nil, err
	}
	var c backend.Command
	switch kind {
	case "cancel":
		c, err = backend.Scancel(p.JobID)
	case "hold":
		c, err = backend.Hold(p.JobID)
	case "release":
		c, err = backend.Release(p.JobID)
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.run(ctx, c); err != nil {
		return nil, fmt.Errorf("%s failed: %w", kind, err)
	}
	s.invalidateQueue()
	past := map[string]string{"cancel": "Cancelled", "hold": "Held", "release": "Released"}[kind]
	return &Confirmed{Action: kind, JobID: p.JobID, Message: fmt.Sprintf("%s job %s.", past, p.JobID)}, nil
}

// PendingKind says which action a token belongs to, without using it.
func (s *Service) PendingKind(tok string) (string, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := s.loadState()
	if err != nil {
		return "", err
	}
	p, ok := st.Pending[tokenKey(strings.TrimSpace(tok))]
	if !ok || s.Now().After(p.Expires) {
		return "", errors.New("unknown, used or expired confirm token")
	}
	return p.Kind, nil
}

// SpendToday reports the A1 ledger for today (for status and tests).
func (s *Service) SpendToday() (usd float64, submits int, err error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	st, err := s.loadState()
	if err != nil {
		return 0, 0, err
	}
	usd, submits = s.committedToday(st)
	return usd, submits, nil
}

// invalidateQueue drops cached queue/accounting answers after a state change.
func (s *Service) invalidateQueue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.cache {
		if strings.HasPrefix(k, "squeue") || strings.HasPrefix(k, "sacct") || strings.HasPrefix(k, "scontrol show nodes") {
			delete(s.cache, k)
		}
	}
}
