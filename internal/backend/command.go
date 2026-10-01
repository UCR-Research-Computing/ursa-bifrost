// Package backend runs scheduler commands on the cluster.
//
// The allow-list is enforced by construction: a Command can only be made by the
// constructors in this file, each of which validates its arguments. There is no
// way to build an arbitrary command, and arguments are quoted individually before
// they reach the remote shell.
package backend

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind groups commands by how long their output may be cached.
type Kind int

// Cache kinds.
const (
	KindQueue Kind = iota
	KindNodes
	KindAcct
	KindCatalog
	KindModules
	KindNoCache
)

// Command is one allow-listed command. Fields are unexported on purpose.
type Command struct {
	argv []string
	kind Kind
	// okExit lists non-zero exit codes that still carry useful output.
	okExit []int
	// stdin is sent to the remote command (the batch script for sbatch).
	stdin []byte
	// merge sends stderr into stdout (sbatch --test-only reports on stderr).
	merge bool
	// write marks commands that change cluster state (A1 tier).
	write bool
	// big raises the timeout for bulk transfers (results download).
	big bool
}

// Stdin is the data sent to the command, if any.
func (c Command) Stdin() []byte { return c.stdin }

// Merge reports whether stderr is folded into stdout.
func (c Command) Merge() bool { return c.merge }

// Write reports whether the command changes cluster state.
func (c Command) Write() bool { return c.write }

// Argv returns a copy of the argument vector.
func (c Command) Argv() []string { return append([]string(nil), c.argv...) }

// Kind is the cache kind.
func (c Command) Kind() Kind { return c.kind }

// String is the command as it would be typed (for audit and `source`).
func (c Command) String() string {
	q := make([]string, len(c.argv))
	for i, a := range c.argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// Backend runs commands.
type Backend interface {
	Run(ctx context.Context, c Command) ([]byte, error)
	Name() string
}

// ErrUnreachable means the command never ran (ssh failed, token expired).
var ErrUnreachable = errors.New("cannot reach the cluster")

var (
	reUser      = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,63}$`)
	reModule    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,80}(/[A-Za-z0-9._+-]{1,40})?$`)
	reSince     = regexp.MustCompile(`^(now-\d{1,4}(days|hours|minutes)|\d{4}-\d{2}-\d{2}(T\d{2}:\d{2}(:\d{2})?)?)$`)
	reStateList = regexp.MustCompile(`^[A-Z_]{2,20}(,[A-Z_]{2,20}){0,10}$`)
)

// ValidJobID accepts "260" and array forms "260_3".
func ValidJobID(id string) error {
	if !regexp.MustCompile(`^\d{1,10}(_\d{1,7})?$`).MatchString(id) {
		return fmt.Errorf("job id %q: expected digits like 260 or 260_3", id)
	}
	return nil
}

// ValidUser checks a cluster user name.
func ValidUser(u string) error {
	if !reUser.MatchString(u) {
		return fmt.Errorf("user %q is not a valid cluster user name", u)
	}
	return nil
}

// ValidSince checks a sacct time (now-7days or 2026-09-01[T10:00]).
func ValidSince(s string) error {
	if !reSince.MatchString(s) {
		return fmt.Errorf("time %q: use now-7days, now-12hours or YYYY-MM-DD", s)
	}
	return nil
}

// ---- constructors (the allow-list) ----------------------------------------

// Whoami is `id -un`.
func Whoami() Command { return Command{argv: []string{"id", "-un"}, kind: KindCatalog} }

// SqueueUser lists one user's queued and running jobs.
func SqueueUser(user string) (Command, error) {
	if err := ValidUser(user); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"squeue", "--json", "-u", user}, kind: KindQueue}, nil
}

// SqueueAll lists every job in the queue.
func SqueueAll() Command { return Command{argv: []string{"squeue", "--json"}, kind: KindQueue} }

// SqueueJob shows one queued or running job. squeue exits 1 for a job that
// already left the queue; that is reported as an empty list, not an error.
func SqueueJob(id string) (Command, error) {
	if err := ValidJobID(id); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"squeue", "--json", "-j", id}, kind: KindQueue, okExit: []int{1}}, nil
}

// SqueueStart is the scheduler's start-time estimate for one pending job.
func SqueueStart(id string) (Command, error) {
	if err := ValidJobID(id); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"squeue", "--json", "--start", "-j", id}, kind: KindQueue, okExit: []int{1}}, nil
}

// SacctJob is the accounting record of one job.
func SacctJob(id string) (Command, error) {
	if err := ValidJobID(id); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"sacct", "--json", "-j", id}, kind: KindAcct}, nil
}

// SacctUser lists a user's jobs that ran between since and until ("" = now).
func SacctUser(user, since, until string) (Command, error) {
	if err := ValidUser(user); err != nil {
		return Command{}, err
	}
	return sacctRange([]string{"-u", user}, since, until)
}

// SacctAll lists every user's jobs between since and until (staff tier).
func SacctAll(since, until string) (Command, error) {
	return sacctRange([]string{"-a"}, since, until)
}

func sacctRange(who []string, since, until string) (Command, error) {
	if err := ValidSince(since); err != nil {
		return Command{}, err
	}
	argv := append([]string{"sacct", "--json"}, who...)
	argv = append(argv, "-S", since)
	if until != "" {
		if err := ValidSince(until); err != nil {
			return Command{}, err
		}
		argv = append(argv, "-E", until)
	}
	return Command{argv: argv, kind: KindAcct}, nil
}

// SacctStates filters a user's jobs by state list (FAILED,TIMEOUT).
func SacctStates(user, since, states string) (Command, error) {
	c, err := SacctUser(user, since, "")
	if err != nil {
		return c, err
	}
	if !reStateList.MatchString(states) {
		return Command{}, fmt.Errorf("states %q: use names like FAILED,TIMEOUT", states)
	}
	c.argv = append(c.argv, "-s", states)
	return c, nil
}

// Sinfo is `sinfo --json`.
func Sinfo() Command { return Command{argv: []string{"sinfo", "--json"}, kind: KindNodes} }

// Nodes is `scontrol show nodes --json`.
func Nodes() Command {
	return Command{argv: []string{"scontrol", "show", "nodes", "--json"}, kind: KindNodes}
}

// Catalog reads the cluster's published catalog (fixed path from config).
func Catalog(p string) (Command, error) {
	if !path.IsAbs(p) || path.Clean(p) != p || !strings.HasSuffix(p, ".json") {
		return Command{}, fmt.Errorf("catalog path %q must be an absolute, clean .json path", p)
	}
	return Command{argv: []string{"cat", "--", p}, kind: KindCatalog}, nil
}

// Tail reads the last n lines of a file. The caller must have checked the path
// against the job record and the allowed roots (see core.checkLogPath).
func Tail(p string, n int) (Command, error) {
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\x00\n") {
		return Command{}, fmt.Errorf("log path %q must be absolute and clean", p)
	}
	if n < 1 || n > 2000 {
		return Command{}, fmt.Errorf("lines must be 1-2000")
	}
	return Command{argv: []string{"tail", "-n", strconv.Itoa(n), "--", p}, kind: KindNoCache}, nil
}

// ModuleShow is `module show NAME` (needs a login shell for Lmod).
func ModuleShow(name string) (Command, error) {
	if !reModule.MatchString(name) {
		return Command{}, fmt.Errorf("module name %q has unexpected characters", name)
	}
	return Command{argv: []string{"bash", "-lc", "module -t show " + name + " 2>&1"}, kind: KindModules}, nil
}

// ---- quoting ----------------------------------------------------------------

var reSafe = regexp.MustCompile(`^[A-Za-z0-9_./=:,@%+-]+$`)

// Quote makes one argument safe for a POSIX shell.
func Quote(s string) string {
	if s != "" && reSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemoteLine is the single string ssh passes to the remote shell.
func RemoteLine(c Command) string { return c.String() }

func okExit(c Command, code int) bool {
	for _, x := range c.okExit {
		if x == code {
			return true
		}
	}
	return false
}

// Timeout picks a per-command timeout.
func Timeout(c Command) time.Duration {
	switch {
	case c.big:
		return 15 * time.Minute
	case c.kind == KindAcct, c.write:
		return 120 * time.Second
	}
	return 60 * time.Second
}

// ---- A1: state-changing commands ---------------------------------------------

var (
	rePartition = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	reJobName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	reJobsDir   = regexp.MustCompile(`^bifrost-jobs/\d{8}-\d{6}-[a-z0-9-]{1,40}$`)
	reComment   = regexp.MustCompile(`^bifrost:[0-9a-f]{12}$`)
	reRelPath   = regexp.MustCompile(`^[A-Za-z0-9._+][A-Za-z0-9._+/ -]{0,250}$`) // no leading "-" (option injection)
)

// SubmitOpts are the resources bifrost enforces on the sbatch command line.
// Command-line options override #SBATCH directives in the script, so these are
// the limits that actually apply, whatever the script says.
type SubmitOpts struct {
	Partition string
	Nodes     int
	TimeMin   int
	JobName   string
	Comment   string // bifrost:<plan hash prefix>
}

func (o SubmitOpts) validate() error {
	if !rePartition.MatchString(o.Partition) {
		return fmt.Errorf("partition %q is not a valid name", o.Partition)
	}
	if o.Nodes < 1 || o.Nodes > 1000 {
		return fmt.Errorf("nodes must be 1-1000")
	}
	if o.TimeMin < 1 || o.TimeMin > 60*24*30 {
		return fmt.Errorf("time limit must be 1 minute to 30 days")
	}
	if o.JobName != "" && !reJobName.MatchString(o.JobName) {
		return fmt.Errorf("job name %q has unexpected characters", o.JobName)
	}
	if !reComment.MatchString(o.Comment) {
		return fmt.Errorf("comment %q is not a bifrost plan tag", o.Comment)
	}
	return nil
}

func (o SubmitOpts) flags() []string {
	f := []string{"--partition=" + o.Partition, fmt.Sprintf("--nodes=%d", o.Nodes),
		fmt.Sprintf("--time=%d", o.TimeMin), "--comment=" + o.Comment}
	if o.JobName != "" {
		f = append(f, "--job-name="+o.JobName)
	}
	return f
}

// SbatchTestOnly asks the scheduler whether and when the job would start,
// without submitting it (`sbatch --test-only`, script on stdin).
func SbatchTestOnly(o SubmitOpts, script []byte) (Command, error) {
	if err := o.validate(); err != nil {
		return Command{}, err
	}
	argv := append([]string{"sbatch", "--test-only"}, o.flags()...)
	return Command{argv: argv, kind: KindNoCache, stdin: script, merge: true}, nil
}

// submitTemplate is fixed text: the job folder and sbatch flags arrive as
// positional parameters, never spliced into the shell code.
const submitTemplate = `set -e; d="$HOME/$1"; shift; if [ -e "$d" ]; then echo "bifrost: $d already exists" >&2; exit 3; fi; mkdir -p "$d"; cat > "$d/job.sbatch"; cd "$d"; exec sbatch --parsable --chdir="$d" "$@" job.sbatch`

// SubmitBatch writes the script into a new ~/bifrost-jobs/<dir> folder and
// submits it with the enforced options. Returns the job id on stdout.
func SubmitBatch(dir string, o SubmitOpts, script []byte) (Command, error) {
	if !reJobsDir.MatchString(dir) {
		return Command{}, fmt.Errorf("job folder %q is not a bifrost-jobs path", dir)
	}
	if err := o.validate(); err != nil {
		return Command{}, err
	}
	if len(script) == 0 {
		return Command{}, fmt.Errorf("empty script")
	}
	argv := append([]string{"bash", "-c", submitTemplate, "bifrost", dir}, o.flags()...)
	return Command{argv: argv, kind: KindNoCache, stdin: script, write: true}, nil
}

// Scancel cancels one job.
func Scancel(id string) (Command, error) {
	if err := ValidJobID(id); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"scancel", id}, kind: KindNoCache, write: true}, nil
}

// Hold keeps a pending job from starting.
func Hold(id string) (Command, error) {
	if err := ValidJobID(id); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"scontrol", "hold", id}, kind: KindNoCache, write: true}, nil
}

// Release lets a held job start.
func Release(id string) (Command, error) {
	if err := ValidJobID(id); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"scontrol", "release", id}, kind: KindNoCache, write: true}, nil
}

// ---- results (read) -----------------------------------------------------------

func validDir(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\n") {
		return fmt.Errorf("folder %q must be an absolute, clean path", p)
	}
	return nil
}

// ValidRelPath checks a path inside a job folder: relative, no "..", no hidden
// leading dash, printable.
func ValidRelPath(rel string) error {
	if !reRelPath.MatchString(rel) || path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || rel == ".." || strings.Contains(rel, "/../") {
		return fmt.Errorf("file %q must be a relative path inside the job folder", rel)
	}
	return nil
}

// ListFiles lists regular files under a job folder: size, mtime, relative path.
func ListFiles(dir string) (Command, error) {
	if err := validDir(dir); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"find", dir, "-maxdepth", "4", "-type", "f", "-printf", `%s\t%T@\t%P\n`}, kind: KindNoCache}, nil
}

// Head reads the first n bytes of a file.
func Head(p string, n int) (Command, error) {
	if err := validDir(p); err != nil {
		return Command{}, err
	}
	if n < 1 || n > 1<<20 {
		return Command{}, fmt.Errorf("bytes must be 1-1048576")
	}
	return Command{argv: []string{"head", "-c", strconv.Itoa(n), "--", p}, kind: KindNoCache}, nil
}

// TarDir streams a gzip tar of a job folder (or listed files inside it).
func TarDir(dir string, files []string) (Command, error) {
	if err := validDir(dir); err != nil {
		return Command{}, err
	}
	argv := []string{"tar", "-C", dir, "-czf", "-", "--exclude-vcs"}
	if len(files) == 0 {
		argv = append(argv, ".")
	} else {
		argv = append(argv, "--")
		for _, f := range files {
			if err := ValidRelPath(f); err != nil {
				return Command{}, err
			}
			argv = append(argv, f)
		}
	}
	return Command{argv: argv, kind: KindNoCache, big: true}, nil
}
