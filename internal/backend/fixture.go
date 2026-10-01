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
	"sort"
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
	// Fail makes a program fail ("scancel": "Invalid job id").
	Fail map[string]string
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
			return read("module_show.txt")
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
