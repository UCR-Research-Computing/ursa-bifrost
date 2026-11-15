package backend

import (
	"strings"
	"testing"
)

func TestNormalizeSinceShorthands(t *testing.T) {
	cases := map[string]string{
		"now-7days":           "now-7days",
		"now-1hour":           "now-1hours",
		"now-2h":              "now-2hours",
		"now-30m":             "now-30minutes",
		"now-1d":              "now-1days",
		"now-2w":              "now-2weeks",
		"24h":                 "now-24hours",
		"1hour":               "now-1hours",
		"45m":                 "now-45minutes",
		"7d":                  "now-7days",
		"3days":               "now-3days",
		"2w":                  "now-2weeks",
		"2026-11-15":          "2026-11-15",
		"2026-11-15T10:00:00": "2026-11-15T10:00:00",
	}
	for in, want := range cases {
		got := NormalizeSince(in)
		if got != want {
			t.Errorf("NormalizeSince(%q) = %q, want %q", in, got, want)
		}
		if _, err := SacctSummaryUser("alice_ucr_edu", in, ""); err != nil {
			t.Errorf("SacctSummaryUser rejected %q: %v", in, err)
		}
	}
	for _, bad := range []string{"yesterday", "1y", "now-5years", "2026-11-15;id", "-u root"} {
		if _, err := SacctSummaryUser("alice_ucr_edu", bad, ""); err == nil {
			t.Errorf("SacctSummaryUser accepted invalid %q", bad)
		}
	}
}

func TestEnvWithoutPythonpathAndGcloudErrorLine(t *testing.T) {
	in := []string{
		"PATH=/usr/bin:/bin",
		"PYTHONPATH=/opt/broken/py314/site-packages",
		"CLOUDSDK_CONFIG=/Users/alice/.config/gcloud",
		"PYTHONPATH=",
	}
	got := envWithout(in, "PYTHONPATH")
	for _, kv := range got {
		if strings.HasPrefix(kv, "PYTHONPATH=") {
			t.Fatalf("PYTHONPATH leaked into subprocess env: %v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("unexpected filtered env: %v", got)
	}

	stderr := strings.Join([]string{
		"ERROR: (gcloud.compute.ssh) There was a problem refreshing your current auth tokens: Reauthentication failed.",
		"Please run:",
		"  $ gcloud auth login",
		"To set the active account, run:",
		"    $ gcloud config set account `ACCOUNT`",
	}, "\n")
	line := gcloudErrorLine(stderr)
	if !strings.Contains(line, "authentication token expired") || !strings.Contains(line, "Reauthentication failed") {
		t.Fatalf("gcloudErrorLine lost auth error: %q", line)
	}
}
