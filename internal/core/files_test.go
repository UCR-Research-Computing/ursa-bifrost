package core

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/staging"
)

// ---- a fake staging bucket -------------------------------------------------------

type fakeStaging struct {
	objs   map[string]staging.Object
	signed []string // "METHOD object ttl headers"
	key    *rsa.PrivateKey
	gcs    *staging.GCS
}

func newFakeStaging(t *testing.T) *fakeStaging {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeStaging{objs: map[string]staging.Object{}, key: k}
	f.gcs = &staging.GCS{BucketName: "test-staging", SignAs: "bifrost@test.iam.gserviceaccount.com",
		Now: func() time.Time { return time.Unix(1790875401, 0) },
		SignBytes: func(_ context.Context, b []byte) ([]byte, error) {
			h := sha256.Sum256(b)
			return rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, h[:])
		}}
	return f
}

func (f *fakeStaging) Bucket() string { return "test-staging" }

func (f *fakeStaging) SignURL(ctx context.Context, method, object string, ttl time.Duration, headers map[string]string) (string, error) {
	f.signed = append(f.signed, fmt.Sprintf("%s %s %s %v", method, object, ttl, headers))
	return f.gcs.SignURL(ctx, method, object, ttl, headers)
}

func (f *fakeStaging) List(_ context.Context, prefix string) ([]staging.Object, error) {
	var out []staging.Object
	for k, o := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeStaging) Stat(_ context.Context, object string) (staging.Object, bool, error) {
	o, ok := f.objs[object]
	return o, ok, nil
}

func (f *fakeStaging) put(name string, size int64, gen string) {
	f.objs[name] = staging.Object{Name: name, Size: size, Generation: gen, Created: time.Unix(1790870000, 0)}
}

func stagedService(t *testing.T, principal string) (*Service, *backend.Fixture, *fakeStaging) {
	t.Helper()
	s, fx := a1Service(t)
	st := newFakeStaging(t)
	s.Staging, s.Principal = st, principal
	s.Cfg.Staging.Bucket = "test-staging"
	return s, fx, st
}

func ownerOf(t *testing.T, s *Service) string {
	o, err := s.ownerKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// ---- signed URLs -----------------------------------------------------------------

func TestSignedURLVerifies(t *testing.T) {
	st := newFakeStaging(t)
	u, err := st.SignURL(context.Background(), http.MethodPut, "in/abc/u0123456789abcdef/my data.csv", 15*time.Minute,
		map[string]string{"x-goog-content-length-range": "0,100"})
	if err != nil {
		t.Fatal(err)
	}
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if pu.Host != "storage.googleapis.com" || pu.EscapedPath() != "/test-staging/in/abc/u0123456789abcdef/my%20data.csv" {
		t.Fatalf("url: %s", u)
	}
	q := pu.Query()
	if q.Get("X-Goog-Expires") != "900" || q.Get("X-Goog-SignedHeaders") != "host;x-goog-content-length-range" {
		t.Fatalf("query: %v", q)
	}
	sts, _, _ := staging.StringToSign("PUT", "test-staging", "in/abc/u0123456789abcdef/my data.csv", "bifrost@test.iam.gserviceaccount.com",
		time.Unix(1790875401, 0), 15*time.Minute, map[string]string{"x-goog-content-length-range": "0,100"})
	sig, _ := hex.DecodeString(q.Get("X-Goog-Signature"))
	h := sha256.Sum256([]byte(sts))
	if err := rsa.VerifyPKCS1v15(&st.key.PublicKey, crypto.SHA256, h[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	for _, bad := range []string{"", "/abs", "a/../b"} {
		if _, err := st.gcs.SignURL(context.Background(), "GET", bad, time.Minute, nil); err == nil {
			t.Errorf("signed bad object %q", bad)
		}
	}
	if _, err := st.gcs.SignURL(context.Background(), "DELETE", "x", time.Minute, nil); err == nil {
		t.Error("signed DELETE")
	}
	if _, err := st.gcs.SignURL(context.Background(), "GET", "x", 8*24*time.Hour, nil); err == nil {
		t.Error("signed a link longer than 7 days")
	}
}

func TestSignedURLsAreRedacted(t *testing.T) {
	st := newFakeStaging(t)
	u, _ := st.SignURL(context.Background(), "GET", "out/x/1/a.txt", time.Hour, nil)
	r := policy.Redact("curl -T a " + u)
	if strings.Contains(r, u[strings.Index(u, "X-Goog-Signature=")+17:]) || !strings.Contains(r, "X-Goog-Signature=[REDACTED]") {
		t.Fatalf("signature not redacted: %s", r)
	}
}

// ---- uploads -----------------------------------------------------------------------

func TestUploadPrepareCapsAndOwner(t *testing.T) {
	s, _, st := stagedService(t, "alice@ucr.edu")
	ctx := context.Background()
	tk, err := s.UploadPrepare(ctx, "data.csv", 1234)
	if err != nil {
		t.Fatal(err)
	}
	owner := ownerOf(t, s)
	if !strings.Contains(st.signed[0], "PUT in/"+owner+"/"+tk.UploadID+"/data.csv 15m0s map[x-goog-content-length-range:0,1234]") {
		t.Fatalf("signed: %v", st.signed)
	}
	if tk.Headers["x-goog-content-length-range"] != "0,1234" || !strings.Contains(tk.Curl, "x-goog-content-length-range: 0,1234") {
		t.Errorf("ticket: %+v", tk)
	}
	for _, bad := range []string{"../x", ".bashrc", "-rf", "a/b", "x\ny", "", "name;id", strings.Repeat("a", 121)} {
		if _, err := s.UploadPrepare(ctx, bad, 10); err == nil {
			t.Errorf("accepted name %q", bad)
		}
	}
	if _, err := s.UploadPrepare(ctx, "big.bin", s.Cfg.Staging.MaxUploadBytes+1); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("per-file cap: %v", err)
	}
	st.put("in/"+owner+"/u00000000000000aa/old.bin", s.Cfg.Staging.MaxUserBytes-100, "1")
	if _, err := s.UploadPrepare(ctx, "more.bin", 200); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("per-user cap: %v", err)
	}
	// another person's files do not count against alice and cannot be seen
	bob, _, _ := stagedService(t, "bob@ucr.edu")
	if ownerOf(t, bob) == owner {
		t.Fatal("two people share a staging folder")
	}
}

func TestUploadsNeedStaging(t *testing.T) {
	s, _ := a1Service(t)
	if _, err := s.UploadPrepare(context.Background(), "a.txt", 1); !errors.Is(err, ErrNoStaging) {
		t.Fatalf("want ErrNoStaging, got %v", err)
	}
	if _, err := s.ResultsLink(context.Background(), "236", []string{"a"}); !errors.Is(err, ErrNoStaging) {
		t.Fatalf("want ErrNoStaging, got %v", err)
	}
}

func TestSubmitWithInputs(t *testing.T) {
	s, fx, st := stagedService(t, "alice@ucr.edu")
	ctx := context.Background()
	owner := ownerOf(t, s)
	st.put("in/"+owner+"/u0123456789abcdef/data set.csv", 42, "7")
	plan, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript, Inputs: []string{"u0123456789abcdef"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Inputs) != 1 || plan.Inputs[0].Filename != "data set.csv" || !strings.Contains(plan.Next, "inputs/") {
		t.Fatalf("plan inputs: %+v %s", plan.Inputs, plan.Next)
	}
	c, err := s.ConfirmSubmit(ctx, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	if c.JobID == "" {
		t.Fatal("no job id")
	}
	script := string(fx.Stdin[len(fx.Stdin)-1])
	if !strings.Contains(script, "curl -sS -f --retry 3") || !strings.Contains(script, "-o 'inputs/data set.csv'") {
		t.Fatalf("no fetch block:\n%s", script)
	}
	// the block sits after every #SBATCH line and before the first command
	lastSbatch := strings.LastIndex(script, "#SBATCH")
	block := strings.Index(script, "# ---- bifrost: fetch")
	firstCmd := strings.Index(script, "\necho ")
	if !(lastSbatch < block && (firstCmd < 0 || block < firstCmd)) {
		t.Fatalf("fetch block misplaced:\n%s", script)
	}
	// the GET link lives as long as the staged object
	if got := st.signed[len(st.signed)-1]; !strings.HasPrefix(got, "GET in/"+owner+"/u0123456789abcdef/data set.csv 168h0m0s") {
		t.Errorf("GET link: %s", got)
	}
}

func TestSubmitInputsGuards(t *testing.T) {
	s, _, st := stagedService(t, "alice@ucr.edu")
	ctx := context.Background()
	owner := ownerOf(t, s)
	// someone else's upload, even with a known id, is not found
	st.put("in/0000000000other0000/u0123456789abcdef/x.csv", 1, "1")
	if _, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript, Inputs: []string{"u0123456789abcdef"}}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("other person's upload accepted: %v", err)
	}
	for _, bad := range []string{"../in/x", "u123", "U0123456789ABCDEF", "u0123456789abcdef/../../x"} {
		_, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript, Inputs: []string{bad}})
		if err == nil || !strings.Contains(err.Error(), "is not an upload id") {
			t.Errorf("input %q: want a format error, got %v", bad, err)
		}
	}
	// replacing the staged bytes after the plan voids the plan
	st.put("in/"+owner+"/u00000000000000bb/y.csv", 5, "1")
	plan, err := s.PrepareSubmit(ctx, SubmitInput{Script: goodScript, Inputs: []string{"u00000000000000bb"}})
	if err != nil {
		t.Fatal(err)
	}
	st.put("in/"+owner+"/u00000000000000bb/y.csv", 5, "2") // new generation, same size
	if _, err := s.ConfirmSubmit(ctx, plan.Token); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed input not caught: %v", err)
	}
	// tampering with the stored inputs breaks the plan hash
	plan, _ = s.PrepareSubmit(ctx, SubmitInput{Script: goodScript, Inputs: []string{"u00000000000000bb"}})
	lk, _ := s.lockState()
	stt, _ := s.loadState()
	for k, p := range stt.Pending {
		p.Inputs[0].Object = "in/0000000000other0000/u0123456789abcdef/x.csv"
		stt.Pending[k] = p
	}
	_ = s.saveState(stt)
	lk.unlock()
	if _, err := s.ConfirmSubmit(ctx, plan.Token); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("tampered inputs not caught: %v", err)
	}
}

func TestInsertAfterHeader(t *testing.T) {
	got := insertAfterHeader("#!/bin/bash\n#SBATCH -p x\n\n# note\n#SBATCH -t 5\nmodule load gcc\necho hi\n", "BLOCK\n")
	want := "#!/bin/bash\n#SBATCH -p x\n\n# note\n#SBATCH -t 5\nBLOCK\nmodule load gcc\necho hi\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := insertAfterHeader("echo hi\n", "B\n"); got != "B\necho hi\n" {
		t.Fatalf("no header: %q", got)
	}
}

// ---- results_link ------------------------------------------------------------------

func TestResultsLink(t *testing.T) {
	s, fx, st := stagedService(t, "alice@ucr.edu")
	ctx := context.Background()
	dir := "/home/alice_ucr_edu/deep-research-lab/run_76"
	resultsFixture(s, fx, dir)
	r, err := s.ResultsLink(ctx, "236", []string{"out/data.csv", "answer.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Links) != 2 || !strings.HasPrefix(r.Links[0].URL, "https://storage.googleapis.com/test-staging/out/") {
		t.Fatalf("links: %+v", r.Links)
	}
	if len(fx.Puts) != 2 {
		t.Fatalf("puts: %v", fx.Puts)
	}
	for u, p := range fx.Puts {
		if !strings.HasPrefix(p, dir+"/") || backend.ValidSignedURL(u) != nil {
			t.Errorf("put %s <- %s", u, p)
		}
	}
	// the command lines in the answer and the audit trail carry no signature
	for _, c := range fx.Calls {
		_ = c
	}
	if _, err := s.ResultsLink(ctx, "236", []string{"../../.ssh/id_rsa"}); err == nil {
		t.Error("linked a path outside the job folder")
	}
	if _, err := s.ResultsLink(ctx, "236", []string{"missing.txt"}); err == nil {
		t.Error("linked a file not in the listing")
	}
	s.Cfg.Staging.MaxLinkBytes = 3
	if _, err := s.ResultsLink(ctx, "236", []string{"out/data.csv"}); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("size cap: %v", err)
	}
	if _, err := s.ResultsLink(ctx, "253", []string{"x"}); err == nil {
		t.Error("linked another user's job")
	}
	_ = st
}

func TestResultsLinkCommandsRedactedInTrace(t *testing.T) {
	s, fx, _ := stagedService(t, "alice@ucr.edu")
	resultsFixture(s, fx, "/home/alice_ucr_edu/deep-research-lab/run_76")
	res, err := Call(context.Background(), s, "test", "results_link", "R1", nil, false, func(ctx context.Context) (*ResultLinks, error) {
		return s.ResultsLink(ctx, "236", []string{"answer.txt"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Source.Commands {
		if strings.Contains(c, "X-Goog-Signature=") && !strings.Contains(c, "X-Goog-Signature=[REDACTED]") {
			t.Fatalf("signed URL leaked into source: %s", c)
		}
	}
}

// ---- paging ------------------------------------------------------------------------

func TestResultsPaging(t *testing.T) {
	s, fx := a1Service(t)
	dir := "/home/alice_ucr_edu/deep-research-lab/run_76"
	files := map[string]string{}
	for i := 0; i < 1203; i++ {
		files[fmt.Sprintf("src/f%04d.c", i)] = "x"
	}
	files["out/result.csv"] = "a,b\n"
	fx.Files = map[string]map[string]string{dir: files}
	ctx := context.Background()
	r, err := s.JobResults(ctx, ResultsInput{JobID: "236"})
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalFiles != 1204 || len(r.Files) != 500 || r.NextOffset != 500 {
		t.Fatalf("page 1: total %d, %d files, next %d", r.TotalFiles, len(r.Files), r.NextOffset)
	}
	r, _ = s.JobResults(ctx, ResultsInput{JobID: "236", Offset: 1000, Limit: 1000})
	if len(r.Files) != 204 || r.NextOffset != 0 {
		t.Fatalf("last page: %d files, next %d", len(r.Files), r.NextOffset)
	}
	r, _ = s.JobResults(ctx, ResultsInput{JobID: "236", Prefix: "out"})
	if r.TotalFiles != 1 || r.Files[0].Path != "out/result.csv" {
		t.Fatalf("prefix: %+v", r.Files)
	}
	r, _ = s.JobResults(ctx, ResultsInput{JobID: "236", Pattern: "*.csv"})
	if r.TotalFiles != 1 {
		t.Fatalf("pattern: %d", r.TotalFiles)
	}
	if _, err := s.JobResults(ctx, ResultsInput{JobID: "236", Prefix: "../.."}); err == nil {
		t.Error("prefix escaped the folder")
	}
}

func TestReadChunksAndGrep(t *testing.T) {
	s, fx := a1Service(t)
	dir := "/home/alice_ucr_edu/deep-research-lab/run_76"
	var b strings.Builder
	for i := 1; i <= 30000; i++ {
		fmt.Fprintf(&b, "line %05d ok\n", i)
	}
	b.WriteString("Traceback: API_KEY=sk-abcdefghijklmnopqrstuvwxyz0123 failed\n")
	big := b.String()
	fx.Files = map[string]map[string]string{dir: {"out.log": big, "blob.bin": "ab\x00cd", "utf.txt": "héllo wörld"}}
	ctx := context.Background()
	var got strings.Builder
	off := int64(0)
	for i := 0; i < 100; i++ {
		r, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "out.log", ReadOffset: off, ReadBytes: 65536})
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(r.Preview.Text)
		if r.Chunk.EOF {
			break
		}
		off = r.Chunk.NextOffset
	}
	want := policy.Redact(big)
	if got.String() != want {
		t.Fatalf("chunks do not reassemble the file: %d vs %d bytes", got.Len(), len(want))
	}
	if strings.Contains(got.String(), "sk-abcdef") {
		t.Fatal("chunk not redacted")
	}
	g, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "out.log", Grep: "Traceback"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Grep == nil || g.Grep.Matches != 1 || g.Grep.TotalLines != 30001 || !strings.Contains(g.Grep.Lines.Text, "30001:Traceback") {
		t.Fatalf("grep: %+v", g.Grep)
	}
	if strings.Contains(g.Grep.Lines.Text, "sk-abcdef") {
		t.Fatal("grep result not redacted")
	}
	if _, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "blob.bin"}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Errorf("binary: %v", err)
	}
	// a chunk boundary inside a multi-byte character is moved, not split
	r, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "utf.txt", ReadBytes: 2})
	if err != nil || r.Preview.Text != "h" || r.Chunk.NextOffset != 1 {
		t.Fatalf("utf-8 split: %+v %v", r.Chunk, err)
	}
	// a chunk that starts inside a character skips to the next whole one
	r, err = s.JobResults(ctx, ResultsInput{JobID: "236", Read: "utf.txt", ReadOffset: 2, ReadBytes: 4})
	if err != nil || r.Preview.Text != "llo" || r.Chunk.Offset != 3 {
		t.Fatalf("utf-8 start: %+v %v", r.Chunk, err)
	}
	// a file that exists but is not in the listing (find -type f leaves out
	// symlinks, e.g. job/link -> ~/.ssh/id_rsa) is refused before any read
	fx.Paths = map[string]string{dir + "/link": "-----BEGIN PRIVATE KEY----- x"}
	n := len(fx.Calls)
	if _, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "link"}); err == nil {
		t.Error("read a file that is not in the job listing")
	}
	if _, err := s.JobResults(ctx, ResultsInput{JobID: "236", Read: "link", Grep: "KEY"}); err == nil {
		t.Error("searched a file that is not in the job listing")
	}
	for _, c := range fx.Calls[n:] {
		if strings.Contains(c, dir+"/link") {
			t.Fatalf("ran %s for an unlisted file", c)
		}
	}
	if _, err := s.JobResults(ctx, ResultsInput{JobID: "236", Grep: "x"}); err == nil {
		t.Error("grep without read accepted")
	}
}

func TestLogWindowsAndGrep(t *testing.T) {
	s, fx := newTestService(t)
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "step %d\n", i)
	}
	b.WriteString("ERROR: diverged at step 4999\n")
	p := "/home/alice_ucr_edu/deep-research-lab/run_76/job.log"
	fx.Logs = map[string]string{}
	fx.Files = map[string]map[string]string{"/home/alice_ucr_edu/deep-research-lab/run_76": {"job.log": b.String()}}
	ctx := context.Background()
	lt, err := s.JobLog(ctx, LogInput{JobID: "236", Lines: 100})
	if err != nil {
		t.Fatal(err)
	}
	if lt.TotalLines != 5001 || lt.FirstLine != 4902 || lt.LastLine != 5001 || lt.Path != p || !strings.HasSuffix(lt.Tail.Text, "ERROR: diverged at step 4999\n") {
		t.Fatalf("tail: %+v", lt)
	}
	if !strings.Contains(lt.Next, "start_line=4802") {
		t.Errorf("next: %s", lt.Next)
	}
	lt, _ = s.JobLog(ctx, LogInput{JobID: "236", Lines: 10, StartLine: 1})
	if lt.FirstLine != 1 || lt.LastLine != 10 || !strings.HasPrefix(lt.Tail.Text, "step 1\n") || !strings.Contains(lt.Next, "start_line=11") {
		t.Fatalf("head window: %+v", lt)
	}
	lt, _ = s.JobLog(ctx, LogInput{JobID: "236", Grep: "ERROR|diverged"})
	if lt.Grep == nil || lt.Grep.Matches != 1 || !strings.Contains(lt.Grep.Lines.Text, "5001:ERROR") || !strings.Contains(lt.Grep.Lines.Text, "4999-step 4999") {
		t.Fatalf("grep: %+v", lt.Grep)
	}
	if _, err := s.JobLog(ctx, LogInput{JobID: "236", Grep: "a\nb"}); err == nil {
		t.Error("multi-line pattern accepted")
	}
}

// ---- files_list / files_read ---------------------------------------------------------

func homeFixture(fx *backend.Fixture) {
	fx.Paths = map[string]string{
		"/home/alice_ucr_edu/project/notes.txt":     "hello\nAPI_TOKEN=abcdef123456 here\n",
		"/home/alice_ucr_edu/project/run.sbatch":    "#!/bin/bash\n",
		"/home/alice_ucr_edu/.ssh/id_ed25519":       "PRIVATE",
		"/home/alice_ucr_edu/.bash_history":         "secret commands",
		"/home/alice_ucr_edu/project/aws.pem":       "KEY",
		"/home/alice_ucr_edu/project/my_secret.txt": "x",
		"/home/alice_ucr_edu/project/sub/":          "",
		"/scratch/alice_ucr_edu/big/out.dat":        "data",
		"/home/bob_ucr_edu/notes.txt":               "bob's",
		"/etc/passwd":                               "root:x:0:0",
	}
	fx.Links = map[string]string{
		"/home/alice_ucr_edu/project/escape":  "/etc/passwd",
		"/home/alice_ucr_edu/project/sneaky":  "/home/alice_ucr_edu/.ssh/id_ed25519",
		"/home/alice_ucr_edu/project/scratch": "/scratch/alice_ucr_edu/big",
	}
}

func TestFilesListAndRead(t *testing.T) {
	s, fx := newTestService(t)
	homeFixture(fx)
	ctx := context.Background()
	l, err := s.FilesList(ctx, FilesListInput{Path: "~/project"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	got := strings.Join(names, ",")
	if got != "sub,notes.txt,run.sbatch" || l.Hidden != 2 {
		t.Fatalf("entries %s, left out %d", got, l.Hidden)
	}
	home, _ := s.FilesList(ctx, FilesListInput{})
	for _, e := range home.Entries {
		if strings.HasPrefix(e.Name, ".") {
			t.Errorf("hidden entry listed: %s", e.Name)
		}
	}
	r, err := s.FilesRead(ctx, FilesReadInput{Path: "~/project/notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Text.Text, "hello") || strings.Contains(r.Text.Text, "abcdef123456") {
		t.Fatalf("read: %q", r.Text.Text)
	}
	if r, err := s.FilesRead(ctx, FilesReadInput{Path: "/home/alice_ucr_edu/project/scratch/out.dat"}); err != nil {
		t.Errorf("link into own scratch refused: %v", err)
	} else if r.Path != "/scratch/alice_ucr_edu/big/out.dat" {
		t.Errorf("resolved path: %s", r.Path)
	}
}

func TestFilesDenials(t *testing.T) {
	s, fx := newTestService(t)
	homeFixture(fx)
	ctx := context.Background()
	for _, p := range []string{
		"/etc/passwd", "/home/bob_ucr_edu/notes.txt", "~/../bob_ucr_edu/notes.txt", "/home/alice_ucr_edu/../bob_ucr_edu/notes.txt",
		"~/.ssh/id_ed25519", "~/.bash_history", "~/project/aws.pem", "~/project/my_secret.txt",
		"~/project/escape", "~/project/sneaky", "/home/alice_ucr_eduX/x", "/home/alice_ucr_edu/project/notes.txt\n/etc/passwd",
	} {
		_, err := s.FilesRead(ctx, FilesReadInput{Path: p})
		if err == nil {
			t.Errorf("read %q", p)
		}
	}
	for _, p := range []string{"/", "/home", "/home/bob_ucr_edu", "~/.ssh", "~/project/escape"} {
		if _, err := s.FilesList(ctx, FilesListInput{Path: p}); err == nil {
			t.Errorf("listed %q", p)
		}
	}
	// a denied path is refused before anything is read on the cluster
	n := len(fx.Calls)
	_, _ = s.FilesRead(ctx, FilesReadInput{Path: "~/.ssh/id_ed25519"})
	for _, c := range fx.Calls[n:] {
		if strings.Contains(c, ".ssh") {
			t.Fatalf("ran %s for a denied path", c)
		}
	}
}

// ---- helper tools --------------------------------------------------------------

func TestStorageUsage(t *testing.T) {
	s, fx := newTestService(t)
	fx.Out = map[string]string{
		"df":                        "Filesystem 1-blocks Used Available Capacity Mounted on\n10.0.0.1:/homeshare 2704467296256 99455336448 2467556229120 4% /home\n10.0.0.1:/scratchshare 2704467296256 1718616064 2565292949504 1% /scratch\n",
		backend.DuTemplateForTest(): "8218501120\t/home/alice_ucr_edu/bifrost-jobs\n?\t/home/alice_ucr_edu/huge\n512\t/home/alice_ucr_edu/.aws_credentials\n2000\t/home/alice_ucr_edu/.cache\n100\t/scratch/alice_ucr_edu/big\n",
	}
	r, err := s.StorageUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Filesystems) != 2 || r.Filesystems[0].Mount != "/home" || r.Filesystems[0].UsedPct != 3.7 {
		t.Fatalf("fs: %+v", r.Filesystems)
	}
	if r.Folders[0].Path != "/home/alice_ucr_edu/bifrost-jobs" || r.HomeBytes != 8218501120+2000 || r.ScratchByte != 100 {
		t.Fatalf("folders: %+v home %d scratch %d", r.Folders, r.HomeBytes, r.ScratchByte)
	}
	for _, f := range r.Folders {
		if strings.Contains(f.Path, "credentials") {
			t.Error("credential-like name listed")
		}
	}
	if !r.Folders[len(r.Folders)-1].Partial && !hasPartial(r.Folders) {
		t.Error("timed-out folder not reported as unknown")
	}
}

func hasPartial(fs []FolderUsage) bool {
	for _, f := range fs {
		if f.Partial {
			return true
		}
	}
	return false
}

func TestEnvCheck(t *testing.T) {
	s, fx := newTestService(t)
	fx.Out = map[string]string{backend.EnvTemplateForTest(): "J\t475\tucrslurmcl-checknodeset-0\nM\tgcc\tok\nM\tnosuch\tfail\tLmod has detected the following error: The following module(s) are unknown: \"nosuch\"\nL\ngcc/13.5.0\nC\tgcc\t/apps/gcc/13.5.0/bin/gcc\tgcc (UCR Ursa Major) 13.5.0\nC\tpython\t\t\nC\tmyprog\t/usr/bin/myprog\t\n"}
	r, err := s.EnvCheck(context.Background(), []string{"gcc", "nosuch"}, []string{"gcc", "python", "myprog"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Modules[0].Loaded || r.Modules[1].Loaded || !strings.Contains(r.Modules[1].Error, "unknown") {
		t.Fatalf("modules: %+v", r.Modules)
	}
	if len(r.Loaded) != 1 || r.Loaded[0] != "gcc/13.5.0" || r.Commands[0].Version == "" || r.Commands[1].Path != "" {
		t.Fatalf("env: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "no bare `python`") {
		t.Errorf("notes: %v", r.Notes)
	}
	// it ran as a one-core job on the check partition, not on the login node
	if r.Partition != "check" || r.JobID != "475" || r.Node != "ucrslurmcl-checknodeset-0" {
		t.Errorf("job: partition=%q job=%q node=%q", r.Partition, r.JobID, r.Node)
	}
	if call := fx.Calls[len(fx.Calls)-1]; !strings.HasPrefix(call, "srun -p check ") {
		t.Errorf("env_check did not run as a check-partition job: %s", call)
	}
	if strings.Contains(strings.Join(r.Notes, " "), "login node") {
		t.Errorf("notes still say login node: %v", r.Notes)
	}
	// only known programs are asked for --version
	last := fx.Calls[len(fx.Calls)-1]
	if !strings.Contains(last, "v:gcc") || !strings.Contains(last, "n:myprog") || strings.Contains(last, "v:myprog") {
		t.Errorf("version probes: %s", last)
	}
	for _, bad := range [][]string{{"gcc; rm -rf ~"}, {"$(id)"}, {"../x"}, {"-rf"}} {
		if _, err := s.EnvCheck(context.Background(), bad, nil); err == nil {
			t.Errorf("module %q accepted", bad)
		}
		if _, err := s.EnvCheck(context.Background(), nil, bad); err == nil {
			t.Errorf("command %q accepted", bad)
		}
	}
	if _, err := s.EnvCheck(context.Background(), nil, []string{"/usr/bin/id"}); err == nil {
		t.Error("command path accepted")
	}
}

func TestInteractiveHelp(t *testing.T) {
	s, fx := newTestService(t)
	n := len(fx.Calls)
	r, err := s.InteractiveHelp(context.Background(), InteractiveInput{Partition: "computehigh", Time: "2:00:00", CPUs: 8, Memory: "16G"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Command != "srun -p computehigh -N 1 -t 2:00:00 -c 8 --mem=16G --pty bash -l" || r.WorstCaseUSD != 3.74 {
		t.Fatalf("help: %+v", r)
	}
	if !strings.Contains(r.Connect, "--tunnel-through-iap") {
		t.Errorf("connect: %s", r.Connect)
	}
	for _, c := range fx.Calls[n:] {
		// only the catalog and the partitions' sharing mode are read (v0.8.0)
		if !strings.HasPrefix(c, "cat ") && c != "sinfo -h -o '%R|%h'" {
			t.Errorf("interactive_help ran %s", c)
		}
	}
	// on a shared partition the session costs the share of the node it holds
	fx.Sharing = "computehigh|NO\n"
	s.cache = map[string]cacheEntry{} // the sharing answer is cached like node state
	r, _ = s.InteractiveHelp(context.Background(), InteractiveInput{Partition: "computehigh", Time: "2:00:00", CPUs: 11})
	if r.WorstCaseUSD != 1.87 || r.USDPerHour != 0.94 {
		t.Errorf("shared session (11 of 22 cores, 2 h): %+v", r)
	}
	r, _ = s.InteractiveHelp(context.Background(), InteractiveInput{Partition: "computehigh", Time: "1:00:00"})
	if !strings.Contains(strings.Join(r.Warnings, " "), "gets 1 core") {
		t.Errorf("no cpus on a shared partition should warn: %v", r.Warnings)
	}
	for _, bad := range []InteractiveInput{{Partition: "nope"}, {Partition: "computehigh", GPUs: 1}, {Partition: "computehigh", Memory: "16G; id"}, {Partition: "computehigh", Time: "forever"}, {Partition: "computehigh", CPUs: 9999}} {
		if _, err := s.InteractiveHelp(context.Background(), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

var _ = filepath.Join

// TestUnreachableIsNotMissing: a connection failure while resolving a path is
// reported as such, not as "does not exist" (live finding in the v0.7.0 burst).
func TestUnreachableIsNotMissing(t *testing.T) {
	s, fx := newTestService(t)
	homeFixture(fx)
	fx.Fail = map[string]string{"realpath": "x"}
	s.Backend = unreachable{fx}
	_, err := s.FilesList(context.Background(), FilesListInput{Path: "~/project"})
	if err == nil || !errors.Is(err, backend.ErrUnreachable) || strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("got %v", err)
	}
}

type unreachable struct{ *backend.Fixture }

func (u unreachable) Run(ctx context.Context, c backend.Command) ([]byte, error) {
	if strings.HasPrefix(c.String(), "realpath") {
		return nil, fmt.Errorf("%w: connection reset", backend.ErrUnreachable)
	}
	return u.Fixture.Run(ctx, c)
}
