package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
)

// ResultFile is one file in a job's folder.
type ResultFile struct {
	Path     string `json:"path"` // relative to the job folder
	Bytes    int64  `json:"bytes"`
	Modified string `json:"modified"`
}

// Results is job_results' answer.
type Results struct {
	JobID      string            `json:"job_id"`
	State      string            `json:"state"`
	Folder     string            `json:"folder"`
	Files      []ResultFile      `json:"files"`
	TotalBytes int64             `json:"total_bytes"`
	Downloaded string            `json:"downloaded_to,omitempty"`
	Saved      []string          `json:"saved,omitempty"`
	Preview    *policy.Untrusted `json:"preview_untrusted,omitempty"`
	PreviewOf  string            `json:"preview_of,omitempty"`
	Notes      []string          `json:"notes"`
}

// ResultsInput selects a results action.
type ResultsInput struct {
	JobID    string
	Read     string // relative path of one text file to show (optional)
	Download bool   // copy the folder (or Files) to results_dir/<job_id>
	Files    []string
}

// maxDownload bounds one download (a guard against pulling a dataset by accident).
const maxDownload = 2 << 30 // 2 GiB

// JobResults lists a job's output folder, shows one small text file, or
// downloads the folder to the laptop. Only the caller's own jobs; the folder
// is the job's working directory from the accounting record and must sit
// under the caller's allowed roots.
func (s *Service) JobResults(ctx context.Context, in ResultsInput) (*Results, error) {
	d, err := s.JobShow(ctx, JobShowInput{JobID: in.JobID})
	if err != nil {
		return nil, err
	}
	dir := d.WorkDir
	if dir == "" {
		return nil, fmt.Errorf("job %s has no working directory on record", in.JobID)
	}
	if err := s.checkLogPath(dir+"/x", d.User); err != nil {
		return nil, err
	}
	home := "/home/" + d.User
	if dir == home || dir == home+"/" {
		return nil, fmt.Errorf("job %s ran in your home folder itself (%s); listing it would show everything you own. Submit jobs with bifrost (they run in ~/bifrost-jobs/...) or give it a folder", in.JobID, dir)
	}
	r := &Results{JobID: in.JobID, State: d.State, Folder: dir, Notes: []string{}}
	lc, err := backend.ListFiles(dir)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, lc)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		n, _ := strconv.ParseInt(parts[0], 10, 64)
		mt, _ := strconv.ParseFloat(parts[1], 64)
		r.Files = append(r.Files, ResultFile{Path: parts[2], Bytes: n, Modified: time.Unix(int64(mt), 0).Format(time.RFC3339)})
		r.TotalBytes += n
	}
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })
	if len(r.Files) > 500 {
		r.Files = r.Files[:500]
		markTruncated(ctx)
	}
	if r.Files == nil {
		r.Files = []ResultFile{}
	}
	if d.State == "RUNNING" || d.State == "PENDING" {
		r.Notes = append(r.Notes, "The job is still "+strings.ToLower(d.State)+"; files may be incomplete.")
	}

	if in.Read != "" {
		if err := backend.ValidRelPath(in.Read); err != nil {
			return nil, err
		}
		if !hasFile(r.Files, in.Read) {
			return nil, fmt.Errorf("%s is not a file in the job folder (see files)", in.Read)
		}
		hc, err := backend.Head(dir+"/"+in.Read, 64*1024)
		if err != nil {
			return nil, err
		}
		b, err := s.run(ctx, hc)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			return nil, fmt.Errorf("%s is binary; download it instead", in.Read)
		}
		r.Preview = policy.Wrap(string(b), s.Cfg.Limits.UntrustedCh)
		r.PreviewOf = in.Read
		markUntrusted(ctx)
	}

	if in.Download {
		want := r.TotalBytes
		if len(in.Files) > 0 {
			want = 0
			for _, f := range in.Files {
				if err := backend.ValidRelPath(f); err != nil {
					return nil, err
				}
				if !hasFile(r.Files, f) {
					return nil, fmt.Errorf("%s is not a file in the job folder", f)
				}
				want += sizeOf(r.Files, f)
			}
		}
		if want > maxDownload {
			return nil, fmt.Errorf("download would be %.1f GB (limit %.0f GB); pick files with files=[...]", float64(want)/(1<<30), float64(maxDownload)/(1<<30))
		}
		dest, saved, err := s.download(ctx, dir, in.JobID, in.Files)
		if err != nil {
			return nil, err
		}
		r.Downloaded, r.Saved = dest, saved
	}
	return r, nil
}

func hasFile(fs []ResultFile, p string) bool {
	for _, f := range fs {
		if f.Path == p {
			return true
		}
	}
	return false
}

func sizeOf(fs []ResultFile, p string) int64 {
	for _, f := range fs {
		if f.Path == p {
			return f.Bytes
		}
	}
	return 0
}

// download streams a tar.gz of the folder and unpacks it safely into
// results_dir/<job_id>: regular files and folders only, no absolute paths, no
// "..", no links; existing files are kept (a numbered copy is written).
func (s *Service) download(ctx context.Context, dir, jobID string, files []string) (string, []string, error) {
	tc, err := backend.TarDir(dir, files)
	if err != nil {
		return "", nil, err
	}
	b, err := s.run(ctx, tc)
	if err != nil {
		return "", nil, fmt.Errorf("downloading %s: %w", dir, err)
	}
	dest := filepath.Join(config.Expand(s.Cfg.ResultsDir), jobID)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return "", nil, fmt.Errorf("unpacking: %w", err)
	}
	tr := tar.NewReader(gz)
	var saved []string
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("unpacking: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return "", nil, fmt.Errorf("refusing unsafe path in archive: %q", h.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
			return "", nil, fmt.Errorf("refusing path outside the results folder: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", nil, err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxDownload {
				return "", nil, errors.New("archive larger than the download limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", nil, err
			}
			target = freeName(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return "", nil, err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, h.Size)); err != nil {
				f.Close()
				return "", nil, err
			}
			f.Close()
			rel, _ := filepath.Rel(dest, target)
			saved = append(saved, rel)
		default:
			// links, devices, fifos: skipped on purpose
		}
	}
	sort.Strings(saved)
	return dest, saved, nil
}

// freeName returns p, or p.1, p.2 ... when p exists (never overwrite).
func freeName(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	for i := 1; ; i++ {
		q := fmt.Sprintf("%s.%d", p, i)
		if _, err := os.Lstat(q); errors.Is(err, os.ErrNotExist) {
			return q
		}
	}
}
