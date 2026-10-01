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
	allowed := map[string]bool{"id": true, "squeue": true, "sacct": true, "sinfo": true, "scontrol": true, "cat": true, "tail": true, "bash": true}
	u, _ := SqueueUser("alice")
	j, _ := SqueueJob("1")
	st, _ := SqueueStart("1")
	a, _ := SacctJob("1")
	au, _ := SacctUser("alice", "now-1days", "")
	aa, _ := SacctAll("now-1days", "")
	cat, _ := Catalog("/apps/docs/catalog.json")
	tl, _ := Tail("/home/alice/x.log", 5)
	ms, _ := ModuleShow("gcc")
	for _, c := range []Command{Whoami(), u, SqueueAll(), j, st, a, au, aa, Sinfo(), Nodes(), cat, tl, ms} {
		if !allowed[c.argv[0]] {
			t.Errorf("unexpected program %q", c.argv[0])
		}
		if c.argv[0] == "scontrol" && (len(c.argv) < 2 || c.argv[1] != "show") {
			t.Errorf("scontrol must be read-only: %v", c.argv)
		}
		if c.argv[0] == "bash" && !strings.HasPrefix(c.argv[2], "module -t show ") {
			t.Errorf("bash only runs module show: %v", c.argv)
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
