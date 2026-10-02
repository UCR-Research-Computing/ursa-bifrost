// Command bifrost is the ursa-bifrost CLI and MCP server.
//
//	bifrost mcp                  run the MCP server on stdio
//	bifrost status               cluster status
//	bifrost jobs [--state S]     your jobs
//	bifrost job show|explain|log <id>
//	bifrost modules <query>      search modules
//	bifrost recipes [query]
//	bifrost check <script.sh>    static check of a batch script
//	bifrost usage [--since now-30days] [--by partition|state|user]
//	bifrost config init|show|path
//	bifrost doctor
//
// Every command takes --json (machine output) and --config PATH.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/core"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/mcpserver"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/version"
)

const usage = `bifrost - read-only bridge between AI assistants and the Ursa Major Slurm cluster

Usage:
  bifrost mcp                              run the MCP server on stdio
  bifrost status                           cluster status (nodes, queue, $/hour)
  bifrost partitions                       partitions from the cluster catalog
  bifrost jobs [--state S] [--since T] [--all] [--user U]
  bifrost job show <id> [--script] [--any]
  bifrost job explain <id> [--lines N] [--any]
  bifrost job log <id> [--stderr] [--lines N] [--any]
  bifrost modules [query]                  search software modules
  bifrost module <name>                    module show
  bifrost recipes [query]                  known-good recipes and site rules
  bifrost check <script.sh|->              static check of a batch script
  bifrost usage [--since T] [--until T] [--by partition|state|user] [--all]
  bifrost waste [--since T] [--cpu PCT] [--all] [--user U]   avoidable spend
  bifrost health                           down/drained nodes, stockouts, stuck jobs (R2)
  bifrost ticket <id> [--text FILE|-]      draft a ticket reply (R2; never sent)
  bifrost results <id> [--read FILE] [--download] [--files a,b]   job outputs (own jobs)
  bifrost submit <script.sh|-> [--partition P] [--nodes N] [--time T] [--name J] [--yes]   (A1)
  bifrost cancel|hold|release <id> [--yes]                      (A1)
  bifrost confirm <token>                  confirm a prepared action (A1)
  bifrost config init|show|path
  bifrost doctor                           check config, SSH and the catalog
  bifrost version

Global flags (anywhere): --json  --config PATH
Times: now-7days, now-12hours, YYYY-MM-DD. --all/--any need tier R2 in the config.
`

type moduleResult struct {
	Matches    []core.ModuleHit `json:"matches"`
	Containers []core.Container `json:"containers,omitempty"`
}

type globals struct {
	json   bool
	config string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// splitGlobals pulls --json and --config out of args wherever they appear.
func splitGlobals(args []string) (globals, []string, error) {
	var g globals
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			g.json = true
		case a == "--config":
			if i+1 >= len(args) {
				return g, nil, errors.New("--config needs a path")
			}
			i++
			g.config = args[i]
		case strings.HasPrefix(a, "--config="):
			g.config = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	return g, rest, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	g, args, err := splitGlobals(args)
	if err != nil {
		return fail(stdout, stderr, g, err)
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version":
		return emit(stdout, g, map[string]string{"version": version.Version}, func(w io.Writer) {
			fmt.Fprintln(w, "bifrost", version.Version)
		})
	case "config":
		return configCmd(rest, stdout, stderr, g)
	}

	cfg, err := config.Load(g.config)
	if err != nil {
		return fail(stdout, stderr, g, err)
	}
	svc, err := core.New(cfg)
	if err != nil {
		return fail(stdout, stderr, g, err)
	}
	defer func() { _ = svc.Close() }() // iap backend: delete the OS Login key
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "mcp":
		srv := mcpserver.New(svc)
		if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "bifrost mcp:", err)
			return 1
		}
		return 0
	case "status":
		r, err := core.Call(ctx, svc, "cli", "cluster_status", "R1", nil, false, svc.ClusterStatus)
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printStatus(w, r.Data) })
	case "partitions":
		r, err := core.Call(ctx, svc, "cli", "partitions", "R1", nil, false, svc.Partitions)
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printPartitions(w, r.Data) })
	case "jobs":
		fs := newFlags("jobs")
		state := fs.String("state", "", "state filter")
		since := fs.String("since", "now-7days", "accounting window start")
		limit := fs.Int("limit", 50, "max rows")
		all := fs.Bool("all", false, "every user (R2)")
		user := fs.String("user", "", "one user (R2)")
		if err := fs.Parse(rest); err != nil {
			return fail(stdout, stderr, g, err)
		}
		tool, tier := "jobs_list", "R1"
		if *all || *user != "" {
			tool, tier = "jobs_list_all", "R2"
		}
		in := core.JobsListInput{User: *user, All: *all && *user == "", State: *state, Since: *since, Limit: *limit}
		r, err := core.Call(ctx, svc, "cli", tool, tier, map[string]any{"state": *state, "since": *since, "user": *user, "all": *all}, false,
			func(ctx context.Context) ([]core.JobSummary, error) { return svc.JobsList(ctx, in) })
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printJobs(w, r.Data, *all || *user != "") })
	case "job":
		return jobCmd(ctx, svc, rest, stdout, stderr, g)
	case "modules":
		q := strings.Join(rest, " ")
		r, err := core.Call(ctx, svc, "cli", "modules_search", "R1", map[string]any{"query": q}, false,
			func(ctx context.Context) (moduleResult, error) {
				cat, err := svc.Catalog(ctx)
				if err != nil {
					return moduleResult{}, err
				}
				return moduleResult{Matches: cat.SearchModules(q), Containers: cat.SearchContainers(q)}, nil
			})
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printModules(w, r.Data) })
	case "module":
		if len(rest) != 1 {
			return fail(stdout, stderr, g, errors.New("usage: bifrost module <name>"))
		}
		r, err := core.Call(ctx, svc, "cli", "module_show", "R1", map[string]any{"name": rest[0]}, false,
			func(ctx context.Context) (string, error) { return svc.ModuleShow(ctx, rest[0]) })
		return out(stdout, stderr, g, r, err, func(w io.Writer) { fmt.Fprint(w, r.Data) })
	case "recipes":
		q := strings.Join(rest, " ")
		r, err := core.Call(ctx, svc, "cli", "recipes", "R1", map[string]any{"query": q}, false,
			func(ctx context.Context) ([]core.Recipe, error) {
				cat, err := svc.Catalog(ctx)
				if err != nil {
					return nil, err
				}
				return cat.SearchRecipes(q), nil
			})
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printRecipes(w, r.Data) })
	case "check":
		if len(rest) != 1 {
			return fail(stdout, stderr, g, errors.New("usage: bifrost check <script.sh|->"))
		}
		var b []byte
		if rest[0] == "-" {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, int64(cfg.Limits.ScriptBytes)+1))
		} else {
			b, err = os.ReadFile(rest[0])
		}
		if err != nil {
			return fail(stdout, stderr, g, err)
		}
		r, err := core.Call(ctx, svc, "cli", "script_check", "R1", map[string]any{"file": rest[0]}, false,
			func(ctx context.Context) (*core.ScriptCheck, error) { return svc.ScriptCheck(ctx, string(b)) })
		code := out(stdout, stderr, g, r, err, func(w io.Writer) { printCheck(w, r.Data) })
		if code == 0 && r.Data != nil && !r.Data.OK {
			return 2
		}
		return code
	case "usage":
		fs := newFlags("usage")
		since := fs.String("since", "now-30days", "start")
		until := fs.String("until", "", "end")
		by := fs.String("by", "partition", "partition|state|user")
		all := fs.Bool("all", false, "every user (R2)")
		user := fs.String("user", "", "one user (R2)")
		if err := fs.Parse(rest); err != nil {
			return fail(stdout, stderr, g, err)
		}
		tool, tier := "my_usage", "R1"
		if *all || *user != "" {
			tool, tier = "usage_report", "R2"
		}
		in := core.UsageInput{User: *user, All: *all && *user == "", Since: *since, Until: *until, GroupBy: *by}
		r, err := core.Call(ctx, svc, "cli", tool, tier, map[string]any{"since": *since, "until": *until, "by": *by, "user": *user, "all": *all}, false,
			func(ctx context.Context) (*core.Usage, error) { return svc.Usage(ctx, in) })
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printUsage(w, r.Data) })
	case "waste":
		fs := newFlags("waste")
		since := fs.String("since", "now-7days", "start")
		cpu := fs.Float64("cpu", 25, "CPU efficiency threshold percent")
		minNH := fs.Float64("min-node-hours", 0.25, "ignore smaller jobs")
		all := fs.Bool("all", false, "every user (R2)")
		user := fs.String("user", "", "one user (R2)")
		if err := fs.Parse(rest); err != nil {
			return fail(stdout, stderr, g, err)
		}
		tool, tier := "waste_report", "R1"
		if *all || *user != "" {
			tool, tier = "waste_report_all", "R2"
		}
		in := core.WasteInput{Since: *since, All: *all && *user == "", User: *user, CPUThreshold: *cpu, MinNodeHours: *minNH}
		r, err := core.Call(ctx, svc, "cli", tool, tier, map[string]any{"since": *since, "cpu": *cpu, "user": *user, "all": *all}, false,
			func(ctx context.Context) (*core.WasteReport, error) { return svc.Waste(ctx, in) })
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printWaste(w, r.Data) })
	case "health":
		r, err := core.Call(ctx, svc, "cli", "health", "R2", nil, false, svc.Health)
		code := out(stdout, stderr, g, r, err, func(w io.Writer) { printHealth(w, r.Data) })
		if code == 0 && r.Data != nil && !r.Data.OK {
			return 2
		}
		return code
	case "ticket":
		if len(rest) < 1 {
			return fail(stdout, stderr, g, errors.New("usage: bifrost ticket <job id> [--text FILE|-]"))
		}
		id := rest[0]
		fs := newFlags("ticket")
		textFile := fs.String("text", "", "file with the researcher's message ('-' = stdin)")
		if err := fs.Parse(rest[1:]); err != nil {
			return fail(stdout, stderr, g, err)
		}
		var text string
		if *textFile != "" {
			var b []byte
			if *textFile == "-" {
				b, err = io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
			} else {
				b, err = os.ReadFile(*textFile)
			}
			if err != nil {
				return fail(stdout, stderr, g, err)
			}
			text = string(b)
		}
		r, err := core.Call(ctx, svc, "cli", "ticket_draft", "R2", map[string]any{"job_id": id, "ticket_text_chars": len(text)}, false,
			func(ctx context.Context) (*core.TicketDraft, error) {
				return svc.TicketDraft(ctx, core.TicketDraftInput{JobID: id, TicketText: text, AnyUser: true})
			})
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printTicket(w, r.Data) })
	case "results":
		if len(rest) < 1 {
			return fail(stdout, stderr, g, errors.New("usage: bifrost results <job id> [--read FILE] [--download] [--files a,b]"))
		}
		id := rest[0]
		fs := newFlags("results")
		read := fs.String("read", "", "show one text file")
		dl := fs.Bool("download", false, "copy the job folder to results_dir/<id>")
		files := fs.String("files", "", "comma-separated files to download")
		if err := fs.Parse(rest[1:]); err != nil {
			return fail(stdout, stderr, g, err)
		}
		in := core.ResultsInput{JobID: id, Read: *read, Download: *dl || *files != ""}
		if *files != "" {
			in.Files = strings.Split(*files, ",")
		}
		r, err := core.Call(ctx, svc, "cli", "job_results", "R1", map[string]any{"job_id": id, "read": *read, "download": in.Download}, false,
			func(ctx context.Context) (*core.Results, error) { return svc.JobResults(ctx, in) })
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printResults(w, r.Data) })
	case "submit":
		if len(rest) < 1 {
			return fail(stdout, stderr, g, errors.New("usage: bifrost submit <script.sh|-> [--partition P] [--nodes N] [--time T] [--name J] [--yes]"))
		}
		src := rest[0]
		fs := newFlags("submit")
		part := fs.String("partition", "", "partition")
		nodes := fs.Int("nodes", 0, "nodes")
		tl := fs.String("time", "", "time limit")
		name := fs.String("name", "", "job name")
		yes := fs.Bool("yes", false, "confirm without asking (still within caps)")
		if err := fs.Parse(rest[1:]); err != nil {
			return fail(stdout, stderr, g, err)
		}
		var b []byte
		if src == "-" {
			b, err = io.ReadAll(io.LimitReader(os.Stdin, int64(cfg.Limits.ScriptBytes)+1))
		} else {
			b, err = os.ReadFile(src)
		}
		if err != nil {
			return fail(stdout, stderr, g, err)
		}
		r, err := core.Call(ctx, svc, "cli", "job_submit", "A1", map[string]any{"file": src, "partition": *part, "nodes": *nodes, "time": *tl}, false,
			func(ctx context.Context) (*core.SubmitPlan, error) {
				return svc.PrepareSubmit(ctx, core.SubmitInput{Script: string(b), Partition: *part, Nodes: *nodes, Time: *tl, JobName: *name})
			})
		if err != nil {
			return fail(stdout, stderr, g, err)
		}
		if g.json && !*yes {
			return emit(stdout, g, r, nil)
		}
		if !g.json {
			printSubmitPlan(stdout, r.Data)
		}
		if !*yes && !askYes(stdout, "Submit this job?") {
			fmt.Fprintln(stdout, "Not submitted.")
			return 0
		}
		return confirmCmd(ctx, svc, "submit", r.Data.Token, stdout, stderr, g)
	case "cancel", "hold", "release":
		if len(rest) < 1 {
			return fail(stdout, stderr, g, fmt.Errorf("usage: bifrost %s <job id> [--yes]", cmd))
		}
		id := rest[0]
		fs := newFlags(cmd)
		yes := fs.Bool("yes", false, "confirm without asking")
		if err := fs.Parse(rest[1:]); err != nil {
			return fail(stdout, stderr, g, err)
		}
		r, err := core.Call(ctx, svc, "cli", "job_"+cmd, "A1", map[string]any{"job_id": id}, false,
			func(ctx context.Context) (*core.ActionPlan, error) { return svc.PrepareAction(ctx, cmd, id) })
		if err != nil {
			return fail(stdout, stderr, g, err)
		}
		if g.json && !*yes {
			return emit(stdout, g, r, nil)
		}
		if !g.json {
			fmt.Fprintf(stdout, "%s job %s (%s, %s)\n  %s\n", strings.ToUpper(cmd[:1])+cmd[1:], r.Data.JobID, r.Data.JobName, r.Data.State, r.Data.Effect)
		}
		if !*yes && !askYes(stdout, "Go ahead?") {
			fmt.Fprintln(stdout, "Nothing changed.")
			return 0
		}
		return confirmCmd(ctx, svc, cmd, r.Data.Token, stdout, stderr, g)
	case "confirm":
		if len(rest) != 1 {
			return fail(stdout, stderr, g, errors.New("usage: bifrost confirm <token>"))
		}
		return confirmCmd(ctx, svc, "", rest[0], stdout, stderr, g)
	case "doctor":
		return doctor(ctx, svc, stdout, g)
	}
	return fail(stdout, stderr, g, fmt.Errorf("unknown command %q (bifrost --help)", cmd))
}

// confirmCmd runs a prepared action; kind "" looks the kind up from the token.
func confirmCmd(ctx context.Context, svc *core.Service, kind, token string, stdout, stderr io.Writer, g globals) int {
	var r core.Result[*core.Confirmed]
	var err error
	if kind == "" {
		kind, err = svc.PendingKind(token)
		if err != nil {
			return fail(stdout, stderr, g, err)
		}
	}
	tool := "job_" + kind + "_confirm"
	if kind == "submit" {
		r, err = core.Call(ctx, svc, "cli", tool, "A1", nil, false, func(ctx context.Context) (*core.Confirmed, error) { return svc.ConfirmSubmit(ctx, token) })
	} else {
		r, err = core.Call(ctx, svc, "cli", tool, "A1", nil, false, func(ctx context.Context) (*core.Confirmed, error) { return svc.ConfirmAction(ctx, kind, token) })
	}
	return out(stdout, stderr, g, r, err, func(w io.Writer) {
		fmt.Fprintln(w, r.Data.Message)
		if r.Data.RemoteDir != "" {
			fmt.Fprintf(w, "folder on the cluster: %s\n", r.Data.RemoteDir)
		}
		if r.Data.JobID != "" && kind == "submit" {
			fmt.Fprintf(w, "watch: bifrost job show %s    results: bifrost results %s --download\n", r.Data.JobID, r.Data.JobID)
		}
	})
}

func askYes(w io.Writer, q string) bool {
	fmt.Fprintf(w, "%s [y/N] ", q)
	var a string
	if _, err := fmt.Fscanln(os.Stdin, &a); err != nil {
		return false
	}
	a = strings.ToLower(strings.TrimSpace(a))
	return a == "y" || a == "yes"
}

func jobCmd(ctx context.Context, svc *core.Service, args []string, stdout, stderr io.Writer, g globals) int {
	if len(args) < 2 {
		return fail(stdout, stderr, g, errors.New("usage: bifrost job show|explain|log <id>"))
	}
	sub, id, rest := args[0], args[1], args[2:]
	fs := newFlags("job " + sub)
	anyUser := fs.Bool("any", false, "another user's job (R2)")
	lines := fs.Int("lines", 0, "log lines")
	script := fs.Bool("script", false, "include the batch script")
	stderrStream := fs.Bool("stderr", false, "read stderr instead of stdout")
	if err := fs.Parse(rest); err != nil {
		return fail(stdout, stderr, g, err)
	}
	tier := "R1"
	suffix := ""
	if *anyUser {
		tier, suffix = "R2", "_any"
	}
	switch sub {
	case "show":
		r, err := core.Call(ctx, svc, "cli", "job_show"+suffix, tier, map[string]any{"job_id": id, "script": *script}, false,
			func(ctx context.Context) (*core.JobDetail, error) {
				return svc.JobShow(ctx, core.JobShowInput{JobID: id, AnyUser: *anyUser, IncludeScript: *script})
			})
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printJob(w, r.Data) })
	case "explain":
		r, err := core.Call(ctx, svc, "cli", "job_explain"+suffix, tier, map[string]any{"job_id": id}, false,
			func(ctx context.Context) (*core.Explanation, error) { return svc.JobExplain(ctx, id, *anyUser, *lines) })
		return out(stdout, stderr, g, r, err, func(w io.Writer) { printExplain(w, r.Data) })
	case "log":
		stream := "stdout"
		if *stderrStream {
			stream = "stderr"
		}
		r, err := core.Call(ctx, svc, "cli", "job_log_tail"+suffix, tier, map[string]any{"job_id": id, "stream": stream}, false,
			func(ctx context.Context) (*core.LogTail, error) {
				return svc.JobLogTail(ctx, id, stream, *lines, *anyUser)
			})
		return out(stdout, stderr, g, r, err, func(w io.Writer) {
			fmt.Fprintf(w, "# %s (%s, last %d lines, redacted)\n%s", r.Data.Path, r.Data.Stream, r.Data.Lines, r.Data.Tail.Text)
		})
	}
	return fail(stdout, stderr, g, fmt.Errorf("unknown job subcommand %q (show, explain, log)", sub))
}

func configCmd(args []string, stdout, stderr io.Writer, g globals) int {
	path := g.config
	if path == "" {
		path = config.DefaultPath()
	}
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "path":
		fmt.Fprintln(stdout, path)
		return 0
	case "init":
		if _, err := os.Stat(path); err == nil {
			return fail(stdout, stderr, g, fmt.Errorf("%s already exists; edit it or remove it first", path))
		}
		if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
			return fail(stdout, stderr, g, err)
		}
		if err := os.WriteFile(path, []byte(config.Example), 0o600); err != nil {
			return fail(stdout, stderr, g, err)
		}
		fmt.Fprintln(stdout, "wrote", path)
		return 0
	case "show":
		c, err := config.Load(path)
		if err != nil {
			return fail(stdout, stderr, g, err)
		}
		return emit(stdout, g, c, func(w io.Writer) {
			b, _ := json.MarshalIndent(c, "", "  ")
			fmt.Fprintln(w, string(b))
		})
	}
	return fail(stdout, stderr, g, fmt.Errorf("unknown config subcommand %q (init, show, path)", sub))
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// out prints a result as JSON or text, or the error.
func out[T any](stdout, stderr io.Writer, g globals, r core.Result[T], err error, text func(io.Writer)) int {
	if err != nil {
		return fail(stdout, stderr, g, err)
	}
	return emit(stdout, g, r, text)
}

func emit(stdout io.Writer, g globals, v any, text func(io.Writer)) int {
	if g.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return 0
	}
	text(stdout)
	return 0
}

func fail(stdout, stderr io.Writer, g globals, err error) int {
	if g.json {
		enc := json.NewEncoder(stdout)
		_ = enc.Encode(map[string]string{"error": err.Error()})
	} else {
		fmt.Fprintln(stderr, "bifrost:", err)
	}
	return 1
}
