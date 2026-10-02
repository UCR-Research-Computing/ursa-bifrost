package core

// Files in and out through the private staging bucket (SPEC sections 18.1,
// 18.2). Bytes go between the person and Cloud Storage, or between the job and
// Cloud Storage, through short-lived signed links; never through bifrost.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/staging"
)

// ErrNoStaging means the deployment has no staging bucket configured.
var ErrNoStaging = errors.New("file staging is not configured on this bifrost (no staging.bucket)")

var reUploadName = regexp.MustCompile(`^[A-Za-z0-9_+][A-Za-z0-9._+ -]{0,119}$`)

var reUploadID = regexp.MustCompile(`^u[0-9a-f]{16}$`)

// ValidUploadName checks a staged file name (it becomes inputs/<name> in the
// job folder and part of a URL).
func ValidUploadName(n string) error {
	if !reUploadName.MatchString(n) || strings.HasSuffix(n, " ") || strings.HasSuffix(n, ".") {
		return fmt.Errorf("file name %q: use 1-120 letters, digits, spaces and ._+- (not starting with . or -)", n)
	}
	return nil
}

// ownerKey is the staging folder for the caller: derived from the signed-in
// identity (or the cluster user on the laptop), never from tool input.
func (s *Service) ownerKey(ctx context.Context) (string, error) {
	who := strings.ToLower(strings.TrimSpace(s.Principal))
	if who == "" {
		u, err := s.User(ctx)
		if err != nil {
			return "", err
		}
		who = "cluster:" + u
	}
	h := sha256.Sum256([]byte(who))
	return hex.EncodeToString(h[:])[:20], nil
}

func (s *Service) staging() (staging.Client, error) {
	if s.Staging == nil || s.Cfg.Staging.Bucket == "" {
		return nil, ErrNoStaging
	}
	return s.Staging, nil
}

// ---- upload_prepare / uploads_list --------------------------------------------

// UploadTicket is upload_prepare's answer.
type UploadTicket struct {
	UploadID  string            `json:"upload_id"`
	Filename  string            `json:"filename"`
	MaxBytes  int64             `json:"max_bytes"`
	Method    string            `json:"method"`
	URL       string            `json:"upload_url"`
	Headers   map[string]string `json:"headers"` // must be sent exactly
	ExpiresAt string            `json:"expires_at"`
	Curl      string            `json:"curl"`
	Next      string            `json:"next"`
}

func newUploadID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "u" + hex.EncodeToString(b), nil
}

// UploadPrepare returns a signed upload link for one file.
func (s *Service) UploadPrepare(ctx context.Context, filename string, size int64) (*UploadTicket, error) {
	st, err := s.staging()
	if err != nil {
		return nil, err
	}
	if err := ValidUploadName(filename); err != nil {
		return nil, err
	}
	sc := s.Cfg.Staging
	if size <= 0 {
		return nil, errors.New("bytes (the file size) is required")
	}
	if size > sc.MaxUploadBytes {
		return nil, fmt.Errorf("%w: %.2f GB is over the %.2f GB per-file upload cap; for big data have the job fetch it directly (Cloud Storage, a URL, Ceph)", ErrCapExceeded, gb(size), gb(sc.MaxUploadBytes))
	}
	owner, err := s.ownerKey(ctx)
	if err != nil {
		return nil, err
	}
	have, err := st.List(ctx, "in/"+owner+"/")
	if err != nil {
		return nil, err
	}
	var used int64
	for _, o := range have {
		used += o.Size
	}
	if used+size > sc.MaxUserBytes {
		return nil, fmt.Errorf("%w: your staged uploads use %.2f GB; this file would pass the %.2f GB limit (uploads are deleted after %d days)", ErrCapExceeded, gb(used), gb(sc.MaxUserBytes), sc.RetainDays)
	}
	id, err := newUploadID()
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(sc.UploadMinutes) * time.Minute
	hdr := map[string]string{"x-goog-content-length-range": "0," + strconv.FormatInt(size, 10)}
	u, err := st.SignURL(ctx, http.MethodPut, "in/"+owner+"/"+id+"/"+filename, ttl, hdr)
	if err != nil {
		return nil, err
	}
	return &UploadTicket{UploadID: id, Filename: filename, MaxBytes: size, Method: http.MethodPut, URL: u, Headers: hdr,
		ExpiresAt: s.Now().Add(ttl).UTC().Format(time.RFC3339),
		Curl:      fmt.Sprintf("curl -f -X PUT -H 'x-goog-content-length-range: 0,%d' -T %s '%s'", size, shellWord(filename), u),
		Next: fmt.Sprintf("Upload the file with the link (PUT, exactly that header) before it expires. Then pass inputs=[\"%s\"] to job_submit: the job downloads it into inputs/%s in its folder when it starts.",
			id, filename)}, nil
}

func shellWord(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9._+-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func gb(n int64) float64 { return round(float64(n)/(1<<30), 2) }

// StagedUpload is one file waiting in the staging area.
type StagedUpload struct {
	UploadID  string `json:"upload_id"`
	Filename  string `json:"filename"`
	Bytes     int64  `json:"bytes"`
	Uploaded  string `json:"uploaded"`
	ExpiresAt string `json:"deleted_after"`
	object    string
	gen       string
}

// UploadsList is uploads_list's answer.
type UploadsList struct {
	Uploads    []StagedUpload `json:"uploads"`
	TotalBytes int64          `json:"total_bytes"`
	LimitBytes int64          `json:"limit_bytes"`
}

func (s *Service) uploads(ctx context.Context) ([]StagedUpload, error) {
	st, err := s.staging()
	if err != nil {
		return nil, err
	}
	owner, err := s.ownerKey(ctx)
	if err != nil {
		return nil, err
	}
	objs, err := st.List(ctx, "in/"+owner+"/")
	if err != nil {
		return nil, err
	}
	keep := time.Duration(s.Cfg.Staging.RetainDays) * 24 * time.Hour
	var out []StagedUpload
	for _, o := range objs {
		parts := strings.SplitN(strings.TrimPrefix(o.Name, "in/"+owner+"/"), "/", 2)
		if len(parts) != 2 || !reUploadID.MatchString(parts[0]) || ValidUploadName(parts[1]) != nil {
			continue
		}
		out = append(out, StagedUpload{UploadID: parts[0], Filename: parts[1], Bytes: o.Size,
			Uploaded: o.Created.UTC().Format(time.RFC3339), ExpiresAt: o.Created.Add(keep).UTC().Format(time.RFC3339),
			object: o.Name, gen: o.Generation})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Uploaded > out[j].Uploaded })
	return out, nil
}

// UploadsList lists the caller's staged uploads.
func (s *Service) UploadsList(ctx context.Context) (*UploadsList, error) {
	ups, err := s.uploads(ctx)
	if err != nil {
		return nil, err
	}
	r := &UploadsList{Uploads: []StagedUpload{}, LimitBytes: s.Cfg.Staging.MaxUserBytes}
	for _, u := range ups {
		r.Uploads = append(r.Uploads, u)
		r.TotalBytes += u.Bytes
	}
	return r, nil
}

// ---- inputs for job_submit -----------------------------------------------------

// SubmitInputFile is one staged file a plan will fetch.
type SubmitInputFile struct {
	UploadID string `json:"upload_id"`
	Filename string `json:"filename"`
	Bytes    int64  `json:"bytes"`
	// Object and Gen are kept in the A1 state file (the plan hash covers
	// them) but never shown to the caller.
	Object string `json:"object,omitempty"`
	Gen    string `json:"generation,omitempty"`
}

// shown drops the bucket object names from plan answers.
func shownInputs(in []SubmitInputFile) []SubmitInputFile {
	out := make([]SubmitInputFile, len(in))
	for i, f := range in {
		out[i] = SubmitInputFile{UploadID: f.UploadID, Filename: f.Filename, Bytes: f.Bytes}
	}
	return out
}

// resolveInputs checks each upload id belongs to the caller and exists.
func (s *Service) resolveInputs(ctx context.Context, ids []string) ([]SubmitInputFile, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 20 {
		return nil, errors.New("at most 20 input files per job")
	}
	ups, err := s.uploads(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]StagedUpload{}
	for _, u := range ups {
		byID[u.UploadID] = u
	}
	seen, names := map[string]bool{}, map[string]bool{}
	var out []SubmitInputFile
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if !reUploadID.MatchString(id) {
			return nil, fmt.Errorf("input %q is not an upload id (they look like u0123456789abcdef)", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		u, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("upload %s was not found in your staging area (not uploaded yet, expired, or not yours); see uploads_list", id)
		}
		if names[u.Filename] {
			return nil, fmt.Errorf("two inputs are both named %s", u.Filename)
		}
		names[u.Filename] = true
		out = append(out, SubmitInputFile{UploadID: id, Filename: u.Filename, Bytes: u.Bytes, Object: u.object, Gen: u.gen})
	}
	return out, nil
}

// inputsHash binds a plan to the exact staged bytes (object generation).
func inputsHash(in []SubmitInputFile) string {
	if len(in) == 0 {
		return ""
	}
	parts := make([]string, 0, len(in)*3)
	for _, f := range in {
		parts = append(parts, f.Object, f.Gen, strconv.FormatInt(f.Bytes, 10))
	}
	return hashOf(parts...)
}

// fetchBlock is inserted after the #SBATCH header: it downloads each staged
// file into inputs/ before the person's commands run. URLs are signed by
// bifrost and quoted for the shell; names were validated.
func fetchBlock(urls, names []string) string {
	var b strings.Builder
	b.WriteString("\n# ---- bifrost: fetch staged input files (SPEC 18.1) ----\n")
	b.WriteString("mkdir -p inputs\n")
	for i := range urls {
		fmt.Fprintf(&b, "curl -sS -f --retry 3 --max-time 3600 -o %s %s || { echo \"bifrost: could not fetch input %s\" >&2; exit 66; }\n",
			backend.Quote("inputs/"+names[i]), backend.Quote(urls[i]), strings.ReplaceAll(names[i], `"`, ``))
	}
	b.WriteString("# ---- end bifrost ----\n\n")
	return b.String()
}

// insertAfterHeader puts block after the shebang, #SBATCH lines and the
// blank/comment lines between them (sbatch stops reading #SBATCH at the first
// command, so the block must come after the header).
func insertAfterHeader(script, block string) string {
	lines := strings.SplitAfter(script, "\n")
	cut := 0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if i == 0 && strings.HasPrefix(t, "#!") {
			cut = i + 1
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") {
			if strings.HasPrefix(t, "#SBATCH") {
				cut = i + 1
			}
			continue
		}
		break
	}
	return strings.Join(lines[:cut], "") + block + strings.Join(lines[cut:], "")
}

// withInputs signs a GET link per input (valid until the staged object is
// deleted) and returns the script with the fetch block inserted.
func (s *Service) withInputs(ctx context.Context, script string, in []SubmitInputFile) (string, error) {
	if len(in) == 0 {
		return script, nil
	}
	st, err := s.staging()
	if err != nil {
		return "", err
	}
	ttl := time.Duration(s.Cfg.Staging.RetainDays) * 24 * time.Hour
	if ttl > staging.MaxTTL || ttl <= 0 {
		ttl = staging.MaxTTL
	}
	var urls, names []string
	for _, f := range in {
		o, ok, err := st.Stat(ctx, f.Object)
		if err != nil {
			return "", err
		}
		if !ok || o.Generation != f.Gen || o.Size != f.Bytes {
			return "", fmt.Errorf("staged input %s changed or disappeared since the plan; prepare the job again", f.Filename)
		}
		u, err := st.SignURL(ctx, http.MethodGet, f.Object, ttl, nil)
		if err != nil {
			return "", err
		}
		urls, names = append(urls, u), append(names, f.Filename)
	}
	return insertAfterHeader(script, fetchBlock(urls, names)), nil
}

// ---- results_link --------------------------------------------------------------

// ResultLink is one signed download link.
type ResultLink struct {
	File      string `json:"file"`
	Bytes     int64  `json:"bytes"`
	URL       string `json:"download_url"`
	ExpiresAt string `json:"expires_at"`
}

// ResultLinks is results_link's answer.
type ResultLinks struct {
	JobID string       `json:"job_id"`
	Links []ResultLink `json:"links"`
	Notes []string     `json:"notes"`
}

// ResultsLink copies chosen files from one of the caller's job folders to the
// staging bucket and returns signed download links.
func (s *Service) ResultsLink(ctx context.Context, jobID string, files []string) (*ResultLinks, error) {
	st, err := s.staging()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("files is required: list the files to link (see job_results)")
	}
	if len(files) > 20 {
		return nil, errors.New("at most 20 files per call")
	}
	d, dir, err := s.jobFolder(ctx, jobID)
	if err != nil {
		return nil, err
	}
	all, _, err := s.listJob(ctx, dir)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, f := range files {
		if err := backend.ValidRelPath(f); err != nil {
			return nil, err
		}
		if !hasFile(all, f) {
			return nil, fmt.Errorf("%s is not a file in the job folder (see job_results)", f)
		}
		total += sizeOf(all, f)
	}
	if total > s.Cfg.Staging.MaxLinkBytes {
		return nil, fmt.Errorf("%w: %.2f GB selected; the limit per call is %.2f GB. Pick fewer files, or have the job copy big outputs to Cloud Storage or Ceph itself", ErrCapExceeded, gb(total), gb(s.Cfg.Staging.MaxLinkBytes))
	}
	owner, err := s.ownerKey(ctx)
	if err != nil {
		return nil, err
	}
	r := &ResultLinks{JobID: jobID, Links: []ResultLink{}, Notes: []string{}}
	stamp := s.Now().UTC().Format("20060102-150405")
	link := time.Duration(s.Cfg.Staging.LinkMinutes) * time.Minute
	for _, f := range files {
		object := "out/" + owner + "/" + jobID + "/" + stamp + "/" + f
		put, err := st.SignURL(ctx, http.MethodPut, object, 20*time.Minute, nil)
		if err != nil {
			return nil, err
		}
		c, err := backend.PutFile(dir+"/"+f, put)
		if err != nil {
			return nil, err
		}
		if _, err := s.run(ctx, c); err != nil {
			return nil, fmt.Errorf("copying %s to staging: %w", f, err)
		}
		get, err := st.SignURL(ctx, http.MethodGet, object, link, nil)
		if err != nil {
			return nil, err
		}
		r.Links = append(r.Links, ResultLink{File: f, Bytes: sizeOf(all, f), URL: get, ExpiresAt: s.Now().Add(link).UTC().Format(time.RFC3339)})
	}
	if d.State == "RUNNING" || d.State == "PENDING" {
		r.Notes = append(r.Notes, "The job is still "+strings.ToLower(d.State)+"; these copies may be incomplete.")
	}
	r.Notes = append(r.Notes, fmt.Sprintf("Links work for %d minutes for anyone who has them; share them only with people who may see the data. The copies are deleted after %d days.", s.Cfg.Staging.LinkMinutes, s.Cfg.Staging.RetainDays))
	return r, nil
}
