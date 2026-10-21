package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRCPageLookAndSafety(t *testing.T) {
	rec := httptest.NewRecorder()
	rcPage(rec, 200, "Ursa Major", "Sign in to Ursa Major", "<p>x</p>", false)
	b := rec.Body.String()
	for _, want := range []string{"#00287a", "Fira Sans", "#ffb81c", "Sign in to Ursa Major | Ursa Major", "UCR Research Computing"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q", want)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("csp %q", csp)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("frame")
	}
}

func TestWantsHTML(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if wantsHTML(r) {
		t.Fatal("no accept = program")
	}
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	if !wantsHTML(r) {
		t.Fatal("browser")
	}
	_ = http.StatusOK
}
