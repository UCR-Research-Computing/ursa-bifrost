package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

// Name is the backend label.
func (f *Fixture) Name() string { return "fixture:" + filepath.Base(f.Dir) }

// Run returns recorded output for an allow-listed command.
func (f *Fixture) Run(_ context.Context, c Command) ([]byte, error) {
	f.Calls = append(f.Calls, c.String())
	a := c.argv
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
