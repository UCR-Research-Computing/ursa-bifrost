package backend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Fixture answers commands from recorded files (tests, demos, offline use).
//
// Mapping: squeue -> squeue.json, sacct -> sacct_jobs.json (filtered by -j when
// given), sinfo -> sinfo.json, scontrol show nodes -> nodes.json, cat <catalog> ->
// catalog.json, tail <path> -> logs/job_<id>.log matched by job id in the path
// map, module show -> module_show.txt, id -un -> user.
type Fixture struct {
	Dir  string
	User string
	// Logs maps a remote log path to a local fixture file name under Dir/logs.
	Logs map[string]string
	// Calls records every command run (tests assert on it).
	Calls []string
	// Stdin records the stdin of each call (same index as Calls).
	Stdin [][]byte
	// NextJobID is what a fake sbatch returns (default 9001).
	NextJobID int
	// Files are fake remote job folders: folder -> relative path -> content.
	Files map[string]map[string]string
	// TestOnly is the fake `sbatch --test-only` answer ("" = a start estimate).
	TestOnly string
	// MPIOnly lists module names that `module show` only finds after a
	// `module load <mpi>` (hierarchical Lmod), as on the real cluster.
	MPIOnly []string
	// Fail makes a program fail ("scancel": "Invalid job id").
	Fail map[string]string
	// Paths are fake files outside job folders (files_list/files_read): absolute
	// path -> content. A path ending in "/" is a folder. Links maps a path to
	// the path realpath resolves it to.
	Paths map[string]string
	Links map[string]string
	// Puts records signed-URL uploads (results_link): URL -> local file path.
	Puts map[string]string
	// Out answers a bash template exactly (env_check, du ...): template -> output.
	Out map[string]string
}

// Name is the backend label.
func (f *Fixture) Name() string { return "fixture:" + filepath.Base(f.Dir) }

// Run returns recorded output for an allow-listed command.
func (f *Fixture) Run(_ context.Context, c Command) ([]byte, error) {
	f.Calls = append(f.Calls, c.String())
	f.Stdin = append(f.Stdin, c.stdin)
	a := c.argv
	if msg, ok := f.Fail[a[0]]; ok {
		return nil, fmt.Errorf("%s exited 1: %s", a[0], msg)
	}
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(f.Dir, name)) }
	switch a[0] {
	case "id":
		return []byte(f.User + "\n"), nil
	case "squeue":
		b, err := read("squeue.json")
		if err != nil {
			return nil, err
		}
		if id := flag(a, "-j"); id != "" {
			return filterJobs(b, id, "job_id")
		}
		return b, nil
	case "sacct":
		b, err := read("sacct_jobs.json")
		if err != nil {
			return nil, err
		}
		if id := flag(a, "-j"); id != "" {
			return filterJobs(b, id, "job_id")
		}
		return b, nil
	case "sinfo":
		return read("sinfo.json")
	case "scontrol":
		if a[1] == "hold" || a[1] == "release" {
			return nil, nil
		}
		return read("nodes.json")
	case "cat":
		return read("catalog.json")
	case "tail":
		p := a[len(a)-1]
		if len(f.Logs) == 0 {
			if b, err := read(filepath.Join("logs", "index.json")); err == nil {
				_ = json.Unmarshal(b, &f.Logs)
			}
		}
		name, ok := f.Logs[p]
		if !ok {
			return nil, fmt.Errorf("tail exited 1: tail: cannot open '%s' for reading: No such file or directory", p)
		}
		return read(filepath.Join("logs", name))
	case "bash":
		if strings.Contains(a[2], "module -t show") {
			// emulate the site's hierarchical Lmod: MPI-built packages are only
			// in the module path after an MPI is loaded (Lmod prints a warning
			// and exits 1 otherwise; bifrost runs it with 2>&1)
			if f.MPIOnly != nil {
				for _, m := range f.MPIOnly {
					if strings.Contains(a[2], "show "+m) && !strings.Contains(a[2], "module load ") {
						out := "Lmod Warning: Failed to find the following module(s): \"" + m + "\" in your\nMODULEPATH\n"
						if okExit(c, 1) { // as the ssh/iap backends do
							return []byte(out), nil
						}
						return []byte(out), fmt.Errorf("bash exited 1: no error text")
					}
				}
			}
			return read("module_show.txt")
		}
		switch a[2] {
		case treeTemplate:
			files := f.Files[a[4]]
			var b strings.Builder
			for _, k := range sortedKeys(files) {
				fmt.Fprintf(&b, "%d\t1790875000.0\t%s\n", len(files[k]), k)
			}
			return []byte(b.String()), nil
		case readRangeTemplate:
			v, err := f.content(a[4])
			if err != nil {
				return nil, err
			}
			off, _ := strconv.ParseInt(a[5], 10, 64)
			n, _ := strconv.Atoi(a[6])
			if off > int64(len(v)) {
				off = int64(len(v))
			}
			end := min(off+int64(n), int64(len(v)))
			return []byte(fmt.Sprintf("%d\n%s", len(v), v[off:end])), nil
		case grepTemplate:
			v, err := f.content(a[4])
			if err != nil {
				return nil, err
			}
			return fakeGrep(v, a[5], a[6], a[7])
		case logWindowTemplate:
			v, err := f.logContent(a[4])
			if err != nil {
				return nil, err
			}
			return fakeWindow(v, a[5], a[6]), nil
		case listDirTemplate:
			return f.listDir(a[4])
		}
		if out, ok := f.Out[a[2]]; ok {
			return []byte(out), nil
		}
		if a[2] == submitTemplate {
			if f.NextJobID == 0 {
				f.NextJobID = 9001
			}
			id := f.NextJobID
			f.NextJobID++
			return []byte(fmt.Sprintf("%d\n", id)), nil
		}
	case "sbatch":
		if f.TestOnly != "" {
			if strings.HasPrefix(f.TestOnly, "ERROR:") {
				return nil, fmt.Errorf("sbatch exited 1: %s", strings.TrimPrefix(f.TestOnly, "ERROR:"))
			}
			return []byte(f.TestOnly), nil
		}
		return []byte("sbatch: Job 9001 to start at 2026-10-01T19:14:40 using 22 processors on nodes ucrslurmcl-c3nodeset-0 in partition computehigh\n"), nil
	case "scancel":
		return nil, nil
	case "find":
		files := f.Files[a[1]]
		var b strings.Builder
		for _, k := range sortedKeys(files) {
			fmt.Fprintf(&b, "%d\t1790875000.0\t%s\n", len(files[k]), k)
		}
		return []byte(b.String()), nil
	case "realpath":
		p := a[len(a)-1]
		if r, ok := f.Links[p]; ok {
			return []byte(r + "\n"), nil
		}
		for l, r := range f.Links { // a path through a linked folder
			if strings.HasPrefix(p, l+"/") {
				q := r + strings.TrimPrefix(p, l)
				if f.exists(q) {
					return []byte(q + "\n"), nil
				}
			}
		}
		if f.exists(p) {
			return []byte(p + "\n"), nil
		}
		return nil, fmt.Errorf("realpath exited 1: %s: No such file or directory", p)
	case "df":
		if out, ok := f.Out["df"]; ok {
			return []byte(out), nil
		}
		return nil, fmt.Errorf("fixture backend: no df recording")
	case "curl":
		if f.Puts == nil {
			f.Puts = map[string]string{}
		}
		f.Puts[a[len(a)-1]] = a[len(a)-2]
		return nil, nil
	case "head":
		p := a[len(a)-1]
		for dir, files := range f.Files {
			if strings.HasPrefix(p, dir+"/") {
				if v, ok := files[strings.TrimPrefix(p, dir+"/")]; ok {
					return []byte(v), nil
				}
			}
		}
		return nil, fmt.Errorf("head exited 1: cannot open %s", p)
	case "tar":
		return fakeTar(f.Files[a[2]], a)
	}
	if a[0] == "scontrol" && (a[1] == "hold" || a[1] == "release") {
		return nil, nil
	}
	return nil, fmt.Errorf("fixture backend: no recording for %s", c.String())
}

func flag(argv []string, name string) string {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == name {
			return argv[i+1]
		}
	}
	return ""
}

// filterJobs keeps jobs whose job_id matches id (array suffix ignored).
func filterJobs(b []byte, id, key string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	var jobs []map[string]json.RawMessage
	if err := json.Unmarshal(doc["jobs"], &jobs); err != nil {
		return nil, err
	}
	want := strings.SplitN(id, "_", 2)[0]
	var keep []map[string]json.RawMessage
	for _, j := range jobs {
		if strings.TrimSpace(string(j[key])) == want {
			keep = append(keep, j)
		}
	}
	if keep == nil {
		keep = []map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(keep)
	if err != nil {
		return nil, err
	}
	doc["jobs"] = raw
	return json.Marshal(doc)
}

func sortedKeys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// FakeTarEntry lets tests inject hostile archive entries.
var FakeTarExtra []tar.Header

func fakeTar(files map[string]string, argv []string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	want := map[string]bool{}
	for i, a := range argv {
		if a == "--" {
			for _, f := range argv[i+1:] {
				want[f] = true
			}
		}
	}
	for _, k := range sortedKeys(files) {
		if len(want) > 0 && !want[k] {
			continue
		}
		v := files[k]
		if err := tw.WriteHeader(&tar.Header{Name: "./" + k, Mode: 0o644, Size: int64(len(v)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(v)); err != nil {
			return nil, err
		}
	}
	for _, h := range FakeTarExtra {
		h := h
		if err := tw.WriteHeader(&h); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			_, _ = tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes(), nil
}

// content finds a fake file in Files (job folders) or Paths.
func (f *Fixture) content(p string) (string, error) {
	if v, ok := f.Paths[p]; ok && !strings.HasSuffix(p, "/") {
		return v, nil
	}
	for dir, files := range f.Files {
		if strings.HasPrefix(p, dir+"/") {
			if v, ok := files[strings.TrimPrefix(p, dir+"/")]; ok {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("bash exited 4: %s: not a regular file", p)
}

// logContent reads a recorded job log (logs/index.json) or a fake file.
func (f *Fixture) logContent(p string) (string, error) {
	if len(f.Logs) == 0 {
		if b, err := os.ReadFile(filepath.Join(f.Dir, "logs", "index.json")); err == nil {
			_ = json.Unmarshal(b, &f.Logs)
		}
	}
	if v, err := f.content(p); err == nil { // a test's own file wins
		return v, nil
	}
	if name, ok := f.Logs[p]; ok {
		b, err := os.ReadFile(filepath.Join(f.Dir, "logs", name))
		return string(b), err
	}
	return f.content(p)
}

func (f *Fixture) exists(p string) bool {
	if _, ok := f.Paths[p]; ok {
		return true
	}
	if _, ok := f.Paths[p+"/"]; ok {
		return true
	}
	if _, err := f.content(p); err == nil {
		return true
	}
	for k := range f.Paths {
		if strings.HasPrefix(k, p+"/") {
			return true
		}
	}
	return false
}

func (f *Fixture) listDir(dir string) ([]byte, error) {
	if !f.exists(dir) {
		return nil, fmt.Errorf("bash exited 4: %s: not a folder", dir)
	}
	seen := map[string]string{}
	for k, v := range f.Paths {
		if !strings.HasPrefix(k, dir+"/") {
			continue
		}
		rest := strings.TrimPrefix(k, dir+"/")
		if rest == "" {
			continue
		}
		name, sub, deeper := strings.Cut(rest, "/")
		if deeper || sub != "" {
			seen[name] = "d\t4096"
		} else if _, isDir := seen[name]; !isDir {
			seen[name] = fmt.Sprintf("f\t%d", len(v))
		}
		if strings.HasSuffix(k, "/") && !strings.Contains(strings.TrimSuffix(rest, "/"), "/") {
			seen[strings.TrimSuffix(rest, "/")] = "d\t4096"
		}
	}
	var b strings.Builder
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ts := strings.SplitN(seen[k], "\t", 2)
		fmt.Fprintf(&b, "%s\t%s\t1790875000.0\t%s\n", ts[0], ts[1], k)
	}
	return []byte(b.String()), nil
}

// fakeGrep mimics `grep -c ”` then `grep -n -E -m max -C ctx` (GNU format).
func fakeGrep(v, pattern, maxS, ctxS string) ([]byte, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("bash exited 2: grep: invalid pattern")
	}
	maxN, _ := strconv.Atoi(maxS)
	c, _ := strconv.Atoi(ctxS)
	lines := strings.SplitAfter(v, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var hits []int
	for i, l := range lines {
		if re.MatchString(strings.TrimRight(l, "\n")) {
			hits = append(hits, i)
			if len(hits) == maxN {
				break
			}
		}
	}
	show := map[int]bool{}
	isHit := map[int]bool{}
	for _, h := range hits {
		isHit[h] = true
		for j := max(0, h-c); j <= min(len(lines)-1, h+c); j++ {
			show[j] = true
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", len(lines))
	prev := -2
	for i := range lines {
		if !show[i] {
			continue
		}
		if prev >= 0 && i != prev+1 {
			b.WriteString("--\n")
		}
		sep := "-"
		if isHit[i] {
			sep = ":"
		}
		fmt.Fprintf(&b, "%d%s%s", i+1, sep, strings.TrimRight(lines[i], "\n")+"\n")
		prev = i
	}
	return []byte(b.String()), nil
}

// fakeWindow mimics logWindowTemplate.
func fakeWindow(v, startS, nS string) []byte {
	start, _ := strconv.Atoi(startS)
	n, _ := strconv.Atoi(nS)
	lines := strings.SplitAfter(v, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	s := start
	if start <= 0 {
		s = total - n + 1
	}
	if s < 1 {
		s = 1
	}
	e := min(s+n-1, total)
	var b strings.Builder
	fmt.Fprintf(&b, "%d %d\n", total, s)
	for i := s; i <= e; i++ {
		b.WriteString(lines[i-1])
	}
	return []byte(b.String())
}
