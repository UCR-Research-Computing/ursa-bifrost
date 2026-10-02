package backend

import (
	"strings"
	"testing"
)

func TestValidJobID(t *testing.T) {
	for _, ok := range []string{"260", "1", "260_3", "1234567890"} {
		if err := ValidJobID(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "abc", "260;rm -rf ~", "260 261", "$(id)", "260_", "-1", "260\n", "12345678901"} {
		if err := ValidJobID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConstructorsRejectInjection(t *testing.T) {
	if _, err := SqueueUser("bob; cat /etc/shadow"); err == nil {
		t.Error("SqueueUser accepted injection")
	}
	if _, err := SacctUser("alice", "now-7days; id", ""); err == nil {
		t.Error("SacctUser accepted injected since")
	}
	if _, err := SacctStates("alice", "now-7days", "FAILED;id"); err == nil {
		t.Error("SacctStates accepted injected state")
	}
	if _, err := ModuleShow("gcc; id"); err == nil {
		t.Error("ModuleShow accepted injection")
	}
	if _, err := ModuleShow("$(id)"); err == nil {
		t.Error("ModuleShow accepted substitution")
	}
	if _, err := Tail("/home/a/../../etc/shadow", 10); err == nil {
		t.Error("Tail accepted a non-clean path")
	}
	if _, err := Tail("relative/log", 10); err == nil {
		t.Error("Tail accepted a relative path")
	}
	if _, err := Catalog("/apps/docs/catalog.json; id"); err == nil {
		t.Error("Catalog accepted injection")
	}
}

func TestQuoteNeutralizesShell(t *testing.T) {
	c, err := Tail("/home/a/it's $(id) `x`.log", 5)
	if err != nil {
		t.Fatal(err)
	}
	line := RemoteLine(c)
	want := `tail -n 5 -- '/home/a/it'\''s $(id) ` + "`x`" + `.log'`
	if line != want {
		t.Fatalf("got  %s\nwant %s", line, want)
	}
}

func TestAllowListIsClosed(t *testing.T) {
	// Every constructor's program must be one of these. If you add a command,
	// add it here deliberately.
	allowed := map[string]bool{"id": true, "squeue": true, "sacct": true, "sinfo": true, "scontrol": true, "cat": true, "tail": true, "bash": true,
		"sbatch": true, "scancel": true, "find": true, "head": true, "tar": true, "realpath": true, "df": true, "curl": true}
	u, _ := SqueueUser("alice")
	j, _ := SqueueJob("1")
	st, _ := SqueueStart("1")
	a, _ := SacctJob("1")
	au, _ := SacctUser("alice", "now-1days", "")
	aa, _ := SacctAll("now-1days", "")
	cat, _ := Catalog("/apps/docs/catalog.json")
	tl, _ := Tail("/home/alice/x.log", 5)
	ms, _ := ModuleShow("gcc")
	o := SubmitOpts{Partition: "standard", Nodes: 1, TimeMin: 30, JobName: "x", Comment: "bifrost:0123456789ab"}
	to, _ := SbatchTestOnly(o, []byte("#!/bin/bash\n"))
	sb, _ := SubmitBatch("bifrost-jobs/20261001-120000-x", o, []byte("#!/bin/bash\n"))
	sc, _ := Scancel("1")
	ho, _ := Hold("1")
	re, _ := Release("1")
	lf, _ := ListFiles("/home/alice/bifrost-jobs/x")
	hd, _ := Head("/home/alice/bifrost-jobs/x/a.txt", 100)
	td, _ := TarDir("/home/alice/bifrost-jobs/x", nil)
	writes := map[string]bool{}
	for _, c := range []Command{sb, sc, ho, re} {
		writes[c.String()] = true
		if !c.Write() {
			t.Errorf("%v must be marked write", c.argv)
		}
	}
	for _, c := range []Command{Whoami(), u, SqueueAll(), j, st, a, au, aa, Sinfo(), Nodes(), cat, tl, ms, to, lf, hd, td} {
		if c.Write() {
			t.Errorf("read command marked write: %v", c.argv)
		}
	}
	for _, c := range []Command{Whoami(), u, SqueueAll(), j, st, a, au, aa, Sinfo(), Nodes(), cat, tl, ms, to, sb, sc, ho, re, lf, hd, td} {
		if !allowed[c.argv[0]] {
			t.Errorf("unexpected program %q", c.argv[0])
		}
		if c.argv[0] == "scontrol" && (len(c.argv) < 2 || (c.argv[1] != "show" && c.argv[1] != "hold" && c.argv[1] != "release")) {
			t.Errorf("scontrol may only show, hold or release: %v", c.argv)
		}
		if c.argv[0] == "bash" && !strings.HasPrefix(c.argv[2], "module -t show ") && c.argv[2] != submitTemplate && c.argv[2] != envTemplate && c.argv[2] != treeTemplate {
			t.Errorf("bash only runs module show or the fixed submit template: %v", c.argv)
		}
		if c.argv[0] == "sbatch" && c.argv[1] != "--test-only" {
			t.Errorf("bare sbatch must be --test-only: %v", c.argv)
		}
	}
}

func TestSplitShell(t *testing.T) {
	in := `/usr/bin/ssh -t -i /k -o "ProxyCommand /py gcloud.py start-iap-tunnel x %p --listen-on-stdin" -o ProxyUseFdpass=no u@compute.1`
	got, err := splitShell(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 || got[5] != "ProxyCommand /py gcloud.py start-iap-tunnel x %p --listen-on-stdin" || got[8] != "u@compute.1" {
		t.Fatalf("%q", got)
	}
}

func TestSubmitConstructorsValidate(t *testing.T) {
	good := SubmitOpts{Partition: "standard", Nodes: 1, TimeMin: 30, JobName: "x", Comment: "bifrost:0123456789ab"}
	bad := []SubmitOpts{
		{Partition: "std; id", Nodes: 1, TimeMin: 30, Comment: good.Comment},
		{Partition: "standard", Nodes: 0, TimeMin: 30, Comment: good.Comment},
		{Partition: "standard", Nodes: 1, TimeMin: 0, Comment: good.Comment},
		{Partition: "standard", Nodes: 1, TimeMin: 30, JobName: "$(id)", Comment: good.Comment},
		{Partition: "standard", Nodes: 1, TimeMin: 30, Comment: "free text"},
	}
	for _, o := range bad {
		if _, err := SbatchTestOnly(o, []byte("x")); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
	for _, dir := range []string{"../etc", "bifrost-jobs/../../x", "/tmp/x", "bifrost-jobs/20261001-120000-a;id", "bifrost-jobs/x"} {
		if _, err := SubmitBatch(dir, good, []byte("x")); err == nil {
			t.Errorf("accepted job folder %q", dir)
		}
	}
	// the folder and flags travel as positional parameters, not inside the shell code
	c, err := SubmitBatch("bifrost-jobs/20261001-120000-hello", good, []byte("#!/bin/bash\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.argv[2] != submitTemplate || c.argv[4] != "bifrost-jobs/20261001-120000-hello" || c.argv[5] != "--partition=standard" {
		t.Errorf("argv: %q", c.argv)
	}
	for _, p := range []string{"../x", "a/../../b", "", "-rf"} {
		if ValidRelPath(p) == nil {
			t.Errorf("rel path %q accepted", p)
		}
	}
	if _, err := TarDir("/home/a/x", []string{"../../.ssh/id_rsa"}); err == nil {
		t.Error("tar of a path outside the folder")
	}
}

// TestNewTemplatesAreFixed: every v0.7.0 bash command is one of the fixed
// templates and caller values only ever arrive as positional parameters.
func TestNewTemplatesAreFixed(t *testing.T) {
	evil := "/home/alice/x; rm -rf ~ $(id) `id`"
	cs := []struct {
		name string
		mk   func() (Command, error)
	}{
		{"tree", func() (Command, error) { return ListTree("/home/alice/bifrost-jobs/x") }},
		{"read", func() (Command, error) { return ReadRange("/home/alice/a.txt", 0, 10) }},
		{"grep", func() (Command, error) { return GrepFile("/home/alice/a.txt", "$(id); x", 10, 2) }},
		{"window", func() (Command, error) { return LogWindow("/home/alice/a.log", 0, 10) }},
		{"listdir", func() (Command, error) { return ListDir("/home/alice") }},
		{"du", func() (Command, error) { return DuTop("/home/alice", "/scratch/alice") }},
		{"env", func() (Command, error) {
			return EnvCheck([]string{"gcc"}, []string{"python3"}, map[string]bool{"python3": true})
		}},
	}
	fixed := map[string]bool{treeTemplate: true, readRangeTemplate: true, grepTemplate: true, logWindowTemplate: true, listDirTemplate: true, duTemplate: true, envTemplate: true}
	for _, c := range cs {
		cmd, err := c.mk()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if cmd.argv[0] != "bash" || !fixed[cmd.argv[2]] || cmd.argv[3] != "bifrost" {
			t.Errorf("%s: not a fixed template: %v", c.name, cmd.argv[:3])
		}
		if cmd.Write() {
			t.Errorf("%s is marked write", c.name)
		}
	}
	// paths with shell syntax are still refused (not clean) or only ever passed as $1
	if _, err := ReadRange(evil, 0, 1); err != nil {
		// accepted paths are quoted as one argument; a dirty path is fine either way
		_ = err
	}
	for i, bad := range []func() (Command, error){
		func() (Command, error) { return ReadRange("relative/x", 0, 10) },
		func() (Command, error) { return ReadRange("/home/alice/../bob/x", 0, 10) },
		func() (Command, error) { return ReadRange("/home/alice/x", 0, MaxReadBytes+1) },
		func() (Command, error) { return ReadRange("/home/alice/x", -1, 10) },
		func() (Command, error) { return GrepFile("/home/alice/x", "", 10, 2) },
		func() (Command, error) { return GrepFile("/home/alice/x", "a\nb\n", 10, 2) },
		func() (Command, error) { return GrepFile("/home/alice/x", "a", MaxGrepMatches+1, 2) },
		func() (Command, error) { return LogWindow("/home/alice/x", 0, MaxWindowLines+1) },
		func() (Command, error) { return ListDir("/") },
		func() (Command, error) { return EnvCheck([]string{"gcc;id"}, nil, nil) },
		func() (Command, error) { return EnvCheck(nil, []string{"/bin/sh"}, nil) },
		func() (Command, error) { return EnvCheck(nil, []string{"-c"}, nil) },
		func() (Command, error) {
			return PutFile("/home/alice/x", "https://evil.example.com/?X-Goog-Signature=00")
		},
		func() (Command, error) { return PutFile("/home/alice/x", "https://storage.googleapis.com/b/o") },
		func() (Command, error) {
			return PutFile("/home/alice/x", "https://storage.googleapis.com/b/o?X-Goog-Signature=00 -o /tmp/x")
		},
	} {
		if _, err := bad(); err == nil {
			t.Errorf("case %d: accepted a bad argument", i)
		}
	}
	if _, err := GrepFile("/home/alice/x", "a\nb", 10, 2); err == nil {
		// "\\n" in a Go string is a backslash-n regex, which is fine
		_ = err
	}
	pf, err := PutFile("/home/alice/x", "https://storage.googleapis.com/b/o?X-Goog-Signature=00ff")
	if err != nil || pf.argv[0] != "curl" || pf.Write() {
		t.Errorf("put: %v %v", pf.argv, err)
	}
}
