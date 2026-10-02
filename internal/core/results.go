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
	TotalFiles int               `json:"total_files"` // matching prefix/pattern
	TotalBytes int64             `json:"total_bytes"` // of the matching files
	Offset     int               `json:"offset"`
	NextOffset int               `json:"next_offset,omitempty"` // 0 = last page
	Downloaded string            `json:"downloaded_to,omitempty"`
	Saved      []string          `json:"saved,omitempty"`
	Preview    *policy.Untrusted `json:"preview_untrusted,omitempty"`
	PreviewOf  string            `json:"preview_of,omitempty"`
	Chunk      *ChunkInfo        `json:"chunk,omitempty"`
	Grep       *GrepResult       `json:"grep,omitempty"`
	Notes      []string          `json:"notes"`
}

// ResultsInput selects a results action.
type ResultsInput struct {
	JobID string
	// listing page (SPEC 18.3)
	Prefix  string // only files under this subfolder
	Pattern string // glob on the relative path (or the base name when it has no "/")
	Offset  int
	Limit   int
	// one file: a chunk from ReadOffset, or a search
	Read       string
	ReadOffset int64
	ReadBytes  int
	Grep       string
	Download   bool // copy the folder (or Files) to results_dir/<job_id>
	Files      []string
}

// maxDownload bounds one download (a guard against pulling a dataset by accident).
const maxDownload = 2 << 30 // 2 GiB

// jobFolder resolves and checks one of the caller's job folders.
func (s *Service) jobFolder(ctx context.Context, jobID string) (*JobDetail, string, error) {
	d, err := s.JobShow(ctx, JobShowInput{JobID: jobID})
	if err != nil {
		return nil, "", err
	}
	dir := d.WorkDir
	if dir == "" {
		return nil, "", fmt.Errorf("job %s has no working directory on record", jobID)
	}
	if err := s.checkLogPath(dir+"/x", d.User); err != nil {
		return nil, "", err
	}
	home := "/home/" + d.User
	if dir == home || dir == home+"/" {
		return nil, "", fmt.Errorf("job %s ran in your home folder itself (%s); listing it would show everything you own. Submit jobs with bifrost (they run in ~/bifrost-jobs/...) or give it a folder", jobID, dir)
	}
	return d, dir, nil
}

// listJob returns every regular file in a job folder (up to MaxTreeFiles).
func (s *Service) listJob(ctx context.Context, dir string) ([]ResultFile, bool, error) {
	lc, err := backend.ListTree(dir)
	if err != nil {
		return nil, false, err
	}
	out, err := s.run(ctx, lc)
	if err != nil {
		return nil, false, fmt.Errorf("listing %s: %w", dir, err)
	}
	var files []ResultFile
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		n, _ := strconv.ParseInt(parts[0], 10, 64)
		mt, _ := strconv.ParseFloat(parts[1], 64)
		files = append(files, ResultFile{Path: parts[2], Bytes: n, Modified: time.Unix(int64(mt), 0).Format(time.RFC3339)})
	}
	cut := false
	if len(files) > backend.MaxTreeFiles {
		files, cut = files[:backend.MaxTreeFiles], true
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, cut, nil
}

func matchResult(f ResultFile, prefix, pattern string) bool {
	if prefix != "" && !strings.HasPrefix(f.Path, prefix+"/") {
		return false
	}
	if pattern == "" {
		return true
	}
	target := f.Path
	if !strings.Contains(pattern, "/") {
		target = path.Base(f.Path)
	}
	ok, _ := path.Match(pattern, target)
	return ok
}

// JobResults lists a job's output folder (paged), reads one text file in
// chunks or searches it, or downloads the folder to the laptop. Only the
// caller's own jobs; the folder is the job's working directory from the
// accounting record and must sit under the caller's allowed roots.
func (s *Service) JobResults(ctx context.Context, in ResultsInput) (*Results, error) {
	if in.Download && s.Remote {
		return nil, errors.New("download writes to the machine running bifrost; on the hosted server use results_link (signed download links) or read")
	}
	if in.Prefix != "" {
		in.Prefix = strings.TrimSuffix(in.Prefix, "/")
		if err := backend.ValidRelPath(in.Prefix); err != nil {
			return nil, err
		}
	}
	if err := validGlob(in.Pattern); err != nil {
		return nil, err
	}
	if in.Grep != "" && in.Read == "" {
		return nil, errors.New("grep searches one file: give read=<file> too")
	}
	d, dir, err := s.jobFolder(ctx, in.JobID)
	if err != nil {
		return nil, err
	}
	r := &Results{JobID: in.JobID, State: d.State, Folder: dir, Files: []ResultFile{}, Notes: []string{}}
	all, cut, err := s.listJob(ctx, dir)
	if err != nil {
		return nil, err
	}
	if cut {
		r.Notes = append(r.Notes, fmt.Sprintf("The folder has more than %d files; only the first %d were listed.", backend.MaxTreeFiles, backend.MaxTreeFiles))
		markTruncated(ctx)
	}
	var match []ResultFile
	for _, f := range all {
		if matchResult(f, in.Prefix, in.Pattern) {
			match = append(match, f)
			r.TotalBytes += f.Bytes
		}
	}
	r.TotalFiles = len(match)
	page, off, next := pageOf(match, in.Offset, in.Limit, 500, 1000)
	if page != nil {
		r.Files = page
	}
	r.Offset, r.NextOffset = off, next
	if next > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("Showing files %d-%d of %d; ask again with offset=%d for more, or narrow with prefix/pattern.", off+1, off+len(page), len(match), next))
	}
	if d.State == "RUNNING" || d.State == "PENDING" {
		r.Notes = append(r.Notes, "The job is still "+strings.ToLower(d.State)+"; files may be incomplete.")
	}

	if in.Read != "" {
		if err := backend.ValidRelPath(in.Read); err != nil {
			return nil, err
		}
		if !hasFile(all, in.Read) {
			return nil, fmt.Errorf("%s is not a file in the job folder (see files)", in.Read)
		}
		if in.Grep != "" {
			g, err := s.grepFile(ctx, dir+"/"+in.Read, in.Grep)
			if err != nil {
				return nil, err
			}
			r.Grep, r.PreviewOf = g, in.Read
		} else {
			info, text, err := s.readChunk(ctx, dir+"/"+in.Read, in.ReadOffset, in.ReadBytes)
			if errors.Is(err, errBinary) {
				return nil, fmt.Errorf("%s is binary; use results_link (or download on the laptop)", in.Read)
			}
			if err != nil {
				return nil, err
			}
			r.Preview, r.PreviewOf, r.Chunk = policy.Wrap(text, 0), in.Read, info
			markUntrusted(ctx)
		}
	}

	if in.Download {
		want := r.TotalBytes
		if len(in.Files) > 0 {
			want = 0
			for _, f := range in.Files {
				if err := backend.ValidRelPath(f); err != nil {
					return nil, err
				}
				if !hasFile(all, f) {
					return nil, fmt.Errorf("%s is not a file in the job folder", f)
				}
				want += sizeOf(all, f)
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
