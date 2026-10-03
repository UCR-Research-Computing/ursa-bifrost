package backend

// Commands for v0.7.0 (SPEC section 18): paged listings, byte-range reads,
// searches, log windows, the helper tools and the results upload to staging.
//
// The same rule as command.go applies: every Command is built here from
// validated arguments. Shell templates are fixed text; values supplied by the
// caller only ever arrive as positional parameters ("$1", "$2", ...), so they
// are never parsed as shell code.

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxTreeFiles bounds one job-folder listing (the template stops after one
// more, so the caller can tell the listing was cut).
const MaxTreeFiles = 200000

const treeTemplate = `find "$1" -maxdepth 10 -type f -printf '%s\t%T@\t%P\n' 2>/dev/null | head -n 200001`

// ListTree lists regular files under a folder, up to 10 levels deep:
// size, mtime, relative path (one per line), at most MaxTreeFiles+1 lines.
func ListTree(dir string) (Command, error) {
	if err := validDir(dir); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"bash", "-c", treeTemplate, "bifrost", dir}, kind: KindNoCache}, nil
}

const readRangeTemplate = `set -e; [ -f "$1" ] || { echo "$1: not a regular file" >&2; exit 4; }; stat -L --printf '%s\n' -- "$1"; dd if="$1" iflag=skip_bytes,count_bytes skip="$2" count="$3" bs=65536 status=none`

// MaxReadBytes bounds one byte-range read.
const MaxReadBytes = 64 * 1024

// ReadRange returns the file size on the first line, then up to n bytes from
// offset.
func ReadRange(p string, offset int64, n int) (Command, error) {
	if err := validDir(p); err != nil {
		return Command{}, err
	}
	if offset < 0 || offset > 1<<50 {
		return Command{}, fmt.Errorf("offset out of range")
	}
	if n < 1 || n > MaxReadBytes {
		return Command{}, fmt.Errorf("bytes must be 1-%d", MaxReadBytes)
	}
	return Command{argv: []string{"bash", "-c", readRangeTemplate, "bifrost", p, strconv.FormatInt(offset, 10), strconv.Itoa(n)}, kind: KindNoCache}, nil
}

const grepTemplate = `set -e; [ -f "$1" ] || { echo "$1: not a regular file" >&2; exit 4; }; grep -c '' < "$1" || true; grep -n -I -E -m "$3" -C "$4" -e "$2" -- "$1" || [ $? -eq 1 ]`

// MaxGrepMatches bounds one search.
const MaxGrepMatches = 200

// ValidPattern checks a search pattern (extended regular expression).
func ValidPattern(p string) error {
	if p == "" || len(p) > 200 || strings.ContainsAny(p, "\x00\n\r") {
		return fmt.Errorf("pattern must be 1-200 characters on one line")
	}
	return nil
}

// GrepFile prints the file's line count, then matching lines as "N:text"
// with context lines as "N-text" (GNU grep format). Binary files never match.
func GrepFile(p, pattern string, max, context int) (Command, error) {
	if err := validDir(p); err != nil {
		return Command{}, err
	}
	if err := ValidPattern(pattern); err != nil {
		return Command{}, err
	}
	if max < 1 || max > MaxGrepMatches || context < 0 || context > 5 {
		return Command{}, fmt.Errorf("matches must be 1-%d and context 0-5", MaxGrepMatches)
	}
	return Command{argv: []string{"bash", "-c", grepTemplate, "bifrost", p, pattern, strconv.Itoa(max), strconv.Itoa(context)}, kind: KindNoCache}, nil
}

const logWindowTemplate = `set -e; [ -f "$1" ] || { echo "$1: not a regular file" >&2; exit 4; }; n=$(grep -c '' < "$1" || true); if [ "$2" -le 0 ]; then s=$(( n - $3 + 1 )); else s=$2; fi; if [ "$s" -lt 1 ]; then s=1; fi; e=$(( s + $3 - 1 )); printf '%s %s\n' "$n" "$s"; sed -n "${s},${e}p;${e}q" -- "$1"`

// MaxWindowLines bounds one log window.
const MaxWindowLines = 2000

// LogWindow prints "<total lines> <first line>" and then up to n lines: the
// last n when start <= 0, otherwise from line start.
func LogWindow(p string, start, n int) (Command, error) {
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\x00\n") {
		return Command{}, fmt.Errorf("log path %q must be absolute and clean", p)
	}
	if n < 1 || n > MaxWindowLines || start < 0 || start > 1<<40 {
		return Command{}, fmt.Errorf("lines must be 1-%d", MaxWindowLines)
	}
	return Command{argv: []string{"bash", "-c", logWindowTemplate, "bifrost", p, strconv.Itoa(start), strconv.Itoa(n)}, kind: KindNoCache}, nil
}

// Realpath resolves symlinks (the result is checked against the allowed roots
// again before anything is read).
func Realpath(p string) (Command, error) {
	if err := validDir(p); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"realpath", "-e", "--", p}, kind: KindNoCache}, nil
}

const listDirTemplate = `set -e; [ -d "$1" ] || { echo "$1: not a folder" >&2; exit 4; }; find "$1" -mindepth 1 -maxdepth 1 -printf '%y\t%s\t%T@\t%f\n' 2>/dev/null | head -n 20001`

// MaxDirEntries bounds one folder listing.
const MaxDirEntries = 20000

// ListDir lists one folder: type letter, size, mtime, name.
func ListDir(dir string) (Command, error) {
	if err := validDir(dir); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"bash", "-c", listDirTemplate, "bifrost", dir}, kind: KindNoCache}, nil
}

// DiskFree is `df -P -B1` on the given folders (exit 1 still has output when
// one of them is missing).
func DiskFree(dirs ...string) (Command, error) {
	if len(dirs) == 0 || len(dirs) > 4 {
		return Command{}, fmt.Errorf("1-4 folders")
	}
	argv := []string{"df", "-P", "-B1", "--"}
	for _, d := range dirs {
		if err := validDir(d); err != nil {
			return Command{}, err
		}
		argv = append(argv, d)
	}
	return Command{argv: argv, kind: KindStorage, okExit: []int{1}}, nil
}

// duTemplate sizes each top-level entry of the given folders within a time
// budget (whole-home du took over four minutes on the live cluster). Entries
// it could not finish print "?".
const duTemplate = `end=$(( $(date +%s) + 100 )); for root in "$@"; do [ -d "$root" ] || continue; for d in "$root"/* "$root"/.[!.]* "$root"/..?*; do [ -e "$d" ] || continue; [ -L "$d" ] && continue; left=$(( end - $(date +%s) )); if [ "$left" -le 1 ]; then printf '?\t%s\n' "$d"; continue; fi; t=$(( left < 30 ? left : 30 )); s=$(timeout "$t" du -x -s -B1 -- "$d" 2>/dev/null | cut -f1); if [ -n "$s" ]; then printf '%s\t%s\n' "$s" "$d"; else printf '?\t%s\n' "$d"; fi; done; done`

// DuTop sizes the top-level entries of the given folders ("bytes<TAB>path").
func DuTop(dirs ...string) (Command, error) {
	if len(dirs) == 0 || len(dirs) > 4 {
		return Command{}, fmt.Errorf("1-4 folders")
	}
	argv := []string{"bash", "-c", duTemplate, "bifrost"}
	for _, d := range dirs {
		if err := validDir(d); err != nil {
			return Command{}, err
		}
		argv = append(argv, d)
	}
	return Command{argv: argv, kind: KindStorage, timeout: 150 * time.Second}, nil
}

var reCommandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,40}$`)

// ValidCommandName checks a program name for env_check (no paths, no options).
func ValidCommandName(c string) error {
	if !reCommandName.MatchString(c) {
		return fmt.Errorf("command %q: expected a program name like python3 or mpirun", c)
	}
	return nil
}

// envTemplate loads modules in a login shell, then reports the module list
// and where each command resolves. M/L/C prefixes keep the parse simple; J
// names the Slurm job and node it ran on.
const envTemplate = `[ -n "${SLURM_JOB_ID:-}" ] && printf 'J\t%s\t%s\n' "$SLURM_JOB_ID" "$(hostname -s)"; n=$1; shift; i=0; while [ "$i" -lt "$n" ]; do m=$1; shift; i=$((i+1)); if module load "$m" >/dev/null 2>&1; then printf 'M\t%s\tok\n' "$m"; else printf 'M\t%s\tfail\t%s\n' "$m" "$(module load "$m" 2>&1 | grep -v '^[[:space:]]*$' | grep -m 3 -i -E 'error|unknown|not found|cannot|conflict|requires|prereq' | tr '\n\t' '  ')"; fi; done; echo L; module -t list 2>&1 | grep -v '^[[:space:]]*$' | grep -v ':$'; for a in "$@"; do c=${a#?:}; p=$(command -v -- "$c" 2>/dev/null || true); v=''; if [ -n "$p" ] && [ "${a%%:*}" = v ]; then v=$(timeout 10 "$c" --version 2>&1 </dev/null | grep -v '^[[:space:]]*$' | head -n 1 | tr '\t' ' '); fi; printf 'C\t%s\t%s\t%s\n' "$c" "$p" "$v"; done`

// EnvJobArgs is the srun prefix env_check runs under: one core for at most 3
// minutes on the given partition (the always-on check partition), never on
// the login node. --immediate gives up if no node is allocated within 2
// minutes (a powered-down node booting counts as allocated).
func EnvJobArgs(partition string) []string {
	return []string{"srun", "-p", partition, "-N", "1", "-n", "1", "-c", "1", "-t", "3",
		"--immediate=120", "--quiet", "-J", "bifrost-env-check"}
}

// EnvCheck loads modules and resolves commands inside a tiny Slurm job on
// partition; version is asked only for the commands in withVersion (a fixed
// list chosen by the caller).
func EnvCheck(partition string, modules, commands []string, withVersion map[string]bool) (Command, error) {
	if !rePartition.MatchString(partition) {
		return Command{}, fmt.Errorf("env_check partition %q is not a partition name", partition)
	}
	if len(modules) > 10 || len(commands) > 15 {
		return Command{}, fmt.Errorf("at most 10 modules and 15 commands")
	}
	argv := append(EnvJobArgs(partition), "bash", "-lc", envTemplate, "bifrost", strconv.Itoa(len(modules)))
	for _, m := range modules {
		if !reModule.MatchString(m) {
			return Command{}, fmt.Errorf("module name %q has unexpected characters", m)
		}
		argv = append(argv, m)
	}
	for _, c := range commands {
		if err := ValidCommandName(c); err != nil {
			return Command{}, err
		}
		if withVersion[c] {
			argv = append(argv, "v:"+c)
		} else {
			argv = append(argv, "n:"+c)
		}
	}
	return Command{argv: argv, kind: KindNoCache, timeout: 200 * time.Second}, nil
}

var reSignedURL = regexp.MustCompile(`^https://storage\.googleapis\.com/[A-Za-z0-9._~%/?&=+-]+$`)

// ValidSignedURL checks a Cloud Storage signed URL made by bifrost.
func ValidSignedURL(u string) error {
	if len(u) > 4000 || !reSignedURL.MatchString(u) || !strings.Contains(u, "X-Goog-Signature=") {
		return fmt.Errorf("not a Cloud Storage signed URL")
	}
	return nil
}

// PutFile uploads one file to a signed Cloud Storage URL (results_link). It
// changes nothing on the cluster; re-running it is harmless.
func PutFile(p, signedURL string) (Command, error) {
	if err := validDir(p); err != nil {
		return Command{}, err
	}
	if err := ValidSignedURL(signedURL); err != nil {
		return Command{}, err
	}
	return Command{argv: []string{"curl", "-sS", "-f", "--retry", "2", "--max-time", "840", "-T", p, signedURL}, kind: KindNoCache, big: true}, nil
}

// DuTemplateForTest and EnvTemplateForTest let fixtures answer these templates.
func DuTemplateForTest() string { return duTemplate }

// EnvTemplateForTest is the env_check template.
func EnvTemplateForTest() string { return envTemplate }
