package staging

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPutRetriesTransientErrorsAndValidatesObject(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "hello-world" {
			t.Errorf("body = %q", string(b))
		}
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limit"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"in/abc/u1/hello.txt","size":"11","generation":"42","timeCreated":"2026-11-15T12:00:00Z"}`))
	}))
	defer srv.Close()

	// Point transport to test server while keeping URL validation happy.
	g := &GCS{
		BucketName: "test-bucket",
		Token:      func(context.Context) (string, error) { return "tok", nil },
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req2 := req.Clone(req.Context())
			req2.URL.Scheme = "http"
			req2.URL.Host = strings.TrimPrefix(srv.URL, "http://")
			return http.DefaultTransport.RoundTrip(req2)
		})},
	}

	for _, bad := range []string{"", "/leading", "a/../b"} {
		if _, err := g.Put(context.Background(), bad, []byte("x")); err == nil {
			t.Errorf("Put accepted bad object %q", bad)
		}
	}

	obj, err := g.Put(context.Background(), "in/abc/u1/hello.txt", []byte("hello-world"))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || obj.Size != 11 || obj.Generation != "42" {
		t.Fatalf("calls=%d obj=%+v", calls.Load(), obj)
	}
}

func TestCommandTokenStripsPYTHONPATH(t *testing.T) {
	t.Setenv("PYTHONPATH", "/bad/py314/site-packages")
	fn := CommandToken([]string{"sh", "-c", `test -z "${PYTHONPATH:-}" && echo tok-clean`})
	tok, err := fn(context.Background())
	if err != nil || tok != "tok-clean" {
		t.Fatalf("CommandToken leaked PYTHONPATH: tok=%q err=%v", tok, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
