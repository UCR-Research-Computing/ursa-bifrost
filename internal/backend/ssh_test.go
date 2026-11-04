package backend

import (
	"strings"
	"testing"
)

// TestControlDir: the ssh ControlMaster socket must fit in a Unix socket path
// (v0.10.8; macOS TMPDIR made the CLI fail with "path too long for Unix domain socket").
func TestControlDir(t *testing.T) {
	mac := "/var/folders/cx/b1gzbdc14x7f6km0l6_fyq1r0000gp/T/"
	cases := []struct{ runtime, tmp, want string }{
		{"/run/user/1000", mac, "/run/user/1000"},
		{"", "/tmp/", "/tmp"},
		{"", mac, "/tmp"},
		{"/" + strings.Repeat("x", 80), "", "/tmp"},
	}
	for _, c := range cases {
		got := controlDir(c.runtime, c.tmp)
		if got != c.want {
			t.Errorf("controlDir(%q, %q) = %q, want %q", c.runtime, c.tmp, got, c.want)
		}
		if n := len(got) + len("/bifrost-") + 40 + 17; n > maxSocketPath {
			t.Errorf("socket path for %q is %d bytes, over %d", got, n, maxSocketPath)
		}
	}
}
