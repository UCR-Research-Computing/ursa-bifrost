package core

// Paged reads, searches and the user-file tools (SPEC section 18.3, 18.4).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/policy"
)

// pageChars caps the text of one page (a read chunk, a log window, a search).
const pageChars = 64000

// defaultReadBytes is one read chunk when the caller does not say.
const defaultReadBytes = 16 * 1024

// ChunkInfo says which part of a file a read returned.
type ChunkInfo struct {
	FileBytes  int64 `json:"file_bytes"`
	Offset     int64 `json:"offset"`
	Bytes      int   `json:"bytes"`
	NextOffset int64 `json:"next_offset,omitempty"` // 0 when eof
	EOF        bool  `json:"eof"`
}

// GrepResult is a search inside one file.
type GrepResult struct {
	Pattern    string            `json:"pattern"`
	TotalLines int               `json:"total_lines"`
	Matches    int               `json:"matches"`
	MaybeMore  bool              `json:"more_matches_possible"`
	Lines      *policy.Untrusted `json:"untrusted"` // "N:matching line" and "N-context line"
}

var errBinary = errors.New("binary file")

// readChunk reads up to n bytes from offset as text, never splitting a UTF-8
// character or (when the page cap applies) a line.
func (s *Service) readChunk(ctx context.Context, p string, offset int64, n int) (*ChunkInfo, string, error) {
	if n <= 0 {
		n = defaultReadBytes
	}
	if n > backend.MaxReadBytes {
		n = backend.MaxReadBytes
		markTruncated(ctx)
	}
	c, err := backend.ReadRange(p, offset, n)
	if err != nil {
		return nil, "", err
	}
	out, err := s.run(ctx, c)
	if err != nil {
		return nil, "", err
	}
	nl := bytes.IndexByte(out, '\n')
	if nl < 0 {
		return nil, "", fmt.Errorf("unexpected answer reading %s", p)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out[:nl])), 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("unexpected size reading %s", p)
	}
	b := out[nl+1:]
	info := &ChunkInfo{FileBytes: size, Offset: offset}
	// started inside a multi-byte character: skip its tail
	for len(b) > 0 && offset > 0 && b[0]&0xC0 == 0x80 {
		b = b[1:]
		info.Offset++
	}
	end := info.Offset + int64(len(b))
	if end < size {
		// do not end inside a multi-byte character
		for i := 0; i < 3 && len(b) > 0; i++ {
			r, sz := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || sz != 1 {
				break
			}
			b = b[:len(b)-1]
		}
	}
	if len(b) > pageChars {
		cut := pageChars
		if i := bytes.LastIndexByte(b[:cut], '\n'); i > 0 {
			cut = i + 1
		}
		for cut > 0 && cut < len(b) && b[cut]&0xC0 == 0x80 {
			cut--
		}
		b = b[:cut]
		markTruncated(ctx)
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return nil, "", errBinary
	}
	info.Bytes = len(b)
	if next := info.Offset + int64(len(b)); next < size {
		info.NextOffset = next
	} else {
		info.EOF = true
	}
	return info, string(b), nil
}

// grepFile searches one file (extended regular expression, 2 lines of context).
func (s *Service) grepFile(ctx context.Context, p, pattern string) (*GrepResult, error) {
	c, err := backend.GrepFile(p, pattern, backend.MaxGrepMatches, 2)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, c)
	if err != nil {
		return nil, err
	}
	first, rest, _ := strings.Cut(string(out), "\n")
	total, _ := strconv.Atoi(strings.TrimSpace(first))
	matches := 0
	for _, line := range strings.Split(rest, "\n") {
		if i := strings.IndexAny(line, ":-"); i > 0 && line[i] == ':' && isDigits(line[:i]) {
			matches++
		}
	}
	if len(rest) > pageChars {
		cut := strings.LastIndexByte(rest[:pageChars], '\n')
		if cut <= 0 {
			cut = pageChars
		}
		rest = rest[:cut]
		markTruncated(ctx)
	}
	markUntrusted(ctx)
	return &GrepResult{Pattern: pattern, TotalLines: total, Matches: matches, MaybeMore: matches >= backend.MaxGrepMatches,
		Lines: policy.Wrap(rest, 0)}, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- user paths ----------------------------------------------------------------

// userRoots are the folders a person may browse with files_list/files_read.
func (s *Service) userRoots(ctx context.Context) (user string, roots []string, err error) {
	user, err = s.User(ctx)
	if err != nil {
		return "", nil, err
	}
	return user, []string{"/home/" + user, "/scratch/" + user}, nil
}

var sensitiveName = regexp.MustCompile(`(?i)(^id_|\.pem$|\.key$|\.p12$|\.pfx$|\.keystore$|\.jks$|\.kdbx$|\.env$|^\.?env$|^\.netrc$|^\.pgpass$|credential|secret|token|password|passwd|private)`)

// sensitive reports names that are refused (credential-like) in any listing.
func sensitive(name string) bool { return sensitiveName.MatchString(name) }

// hiddenOrSensitive is the deny rule for one path component.
func hiddenOrSensitive(name string) bool { return strings.HasPrefix(name, ".") || sensitive(name) }

// checkUserPath accepts a clean absolute path under one of the roots (or a root
// itself) whose components below the root are neither hidden nor sensitive.
func checkUserPath(p string, roots []string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\x00\n") {
		return fmt.Errorf("%w: %q must be an absolute, clean path", policy.ErrDenied, p)
	}
	for _, r := range roots {
		if p == r {
			return nil
		}
		if strings.HasPrefix(p, r+"/") {
			for _, part := range strings.Split(strings.TrimPrefix(p, r+"/"), "/") {
				if hiddenOrSensitive(part) {
					return fmt.Errorf("%w: %q is hidden or looks like a credential; bifrost does not read those", policy.ErrDenied, part)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s is outside your home and scratch folders (%s)", policy.ErrDenied, p, strings.Join(roots, ", "))
}

// resolveUserPath expands ~, checks the path, resolves symlinks on the cluster
// and checks the resolved path again (a link must not lead out of the roots or
// into a hidden folder).
func (s *Service) resolveUserPath(ctx context.Context, p string) (string, error) {
	_, roots, err := s.userRoots(ctx)
	if err != nil {
		return "", err
	}
	p = strings.TrimSpace(p)
	switch {
	case p == "" || p == "~":
		p = roots[0]
	case strings.HasPrefix(p, "~/"):
		p = roots[0] + "/" + strings.TrimPrefix(p, "~/")
	case !strings.HasPrefix(p, "/"):
		p = roots[0] + "/" + p
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		p = "/"
	}
	if err := checkUserPath(p, roots); err != nil {
		return "", err
	}
	rc, err := backend.Realpath(p)
	if err != nil {
		return "", err
	}
	out, err := s.run(ctx, rc)
	if errors.Is(err, backend.ErrUnreachable) {
		return "", err // a transport problem, not a missing file
	}
	if err != nil {
		return "", fmt.Errorf("%s does not exist or cannot be read", p)
	}
	real := strings.TrimSpace(string(out))
	if err := checkUserPath(real, roots); err != nil {
		return "", fmt.Errorf("%s is a link to %s: %w", p, real, err)
	}
	return real, nil
}

// ---- files_list ---------------------------------------------------------------

// DirEntry is one entry of a folder.
type DirEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // file | dir | link | other
	Bytes    int64  `json:"bytes"`
	Modified string `json:"modified"`
}

// DirListing is files_list's answer.
type DirListing struct {
	Path       string     `json:"path"`
	Entries    []DirEntry `json:"entries"`
	Total      int        `json:"total"`
	Hidden     int        `json:"left_out"` // hidden or credential-like entries not shown
	Offset     int        `json:"offset"`
	NextOffset int        `json:"next_offset,omitempty"`
	Notes      []string   `json:"notes"`
}

// FilesListInput selects a folder page.
type FilesListInput struct {
	Path    string
	Pattern string // glob on the name
	Offset  int
	Limit   int
}

// FilesList lists one folder under the caller's home or scratch.
func (s *Service) FilesList(ctx context.Context, in FilesListInput) (*DirListing, error) {
	if err := validGlob(in.Pattern); err != nil {
		return nil, err
	}
	dir, err := s.resolveUserPath(ctx, in.Path)
	if err != nil {
		return nil, err
	}
	c, err := backend.ListDir(dir)
	if err != nil {
		return nil, err
	}
	out, err := s.run(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	l := &DirListing{Path: dir, Entries: []DirEntry{}, Notes: []string{}}
	var all []DirEntry
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > backend.MaxDirEntries {
		lines = lines[:backend.MaxDirEntries]
		l.Notes = append(l.Notes, fmt.Sprintf("The folder has more than %d entries; only the first %d were read.", backend.MaxDirEntries, backend.MaxDirEntries))
		markTruncated(ctx)
	}
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) != 4 {
			continue
		}
		name := parts[3]
		if hiddenOrSensitive(name) {
			l.Hidden++
			continue
		}
		if in.Pattern != "" {
			if ok, _ := path.Match(in.Pattern, name); !ok {
				continue
			}
		}
		n, _ := strconv.ParseInt(parts[1], 10, 64)
		mt, _ := strconv.ParseFloat(parts[2], 64)
		typ := map[string]string{"f": "file", "d": "dir", "l": "link"}[parts[0]]
		if typ == "" {
			typ = "other"
		}
		all = append(all, DirEntry{Name: name, Type: typ, Bytes: n, Modified: time.Unix(int64(mt), 0).UTC().Format(time.RFC3339)})
	}
	sort.Slice(all, func(i, j int) bool {
		if (all[i].Type == "dir") != (all[j].Type == "dir") {
			return all[i].Type == "dir"
		}
		return all[i].Name < all[j].Name
	})
	l.Total = len(all)
	page, off, next := pageOf(all, in.Offset, in.Limit, 200, 1000)
	l.Entries, l.Offset, l.NextOffset = page, off, next
	if l.Entries == nil {
		l.Entries = []DirEntry{}
	}
	if l.Hidden > 0 {
		l.Notes = append(l.Notes, fmt.Sprintf("%d hidden or credential-like entries are not shown (bifrost never reads those).", l.Hidden))
	}
	markUntrusted(ctx) // names are written by people and programs
	return l, nil
}

// pageOf returns one page of xs and the next offset (0 = no more).
func pageOf[T any](xs []T, offset, limit, def, max int) ([]T, int, int) {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(xs) {
		return nil, offset, 0
	}
	end := offset + limit
	if end >= len(xs) {
		return xs[offset:], offset, 0
	}
	return xs[offset:end], offset, end
}

func validGlob(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > 120 || strings.ContainsAny(p, "\x00\n") {
		return errors.New("pattern must be under 120 characters on one line")
	}
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("pattern %q is not a valid glob", p)
	}
	return nil
}

// ---- files_read ---------------------------------------------------------------

// FileRead is files_read's answer.
type FileRead struct {
	Path  string            `json:"path"`
	Chunk *ChunkInfo        `json:"chunk,omitempty"`
	Text  *policy.Untrusted `json:"untrusted,omitempty"`
	Grep  *GrepResult       `json:"grep,omitempty"`
}

// FilesReadInput selects a chunk or a search.
type FilesReadInput struct {
	Path   string
	Offset int64
	Bytes  int
	Grep   string
}

// FilesRead reads one text file under the caller's home or scratch.
func (s *Service) FilesRead(ctx context.Context, in FilesReadInput) (*FileRead, error) {
	if strings.TrimSpace(in.Path) == "" {
		return nil, errors.New("path is required")
	}
	p, err := s.resolveUserPath(ctx, in.Path)
	if err != nil {
		return nil, err
	}
	_, roots, _ := s.userRoots(ctx)
	for _, r := range roots {
		if p == r {
			return nil, fmt.Errorf("%s is a folder; use files_list", p)
		}
	}
	r := &FileRead{Path: p}
	if in.Grep != "" {
		g, err := s.grepFile(ctx, p, in.Grep)
		if err != nil {
			return nil, err
		}
		r.Grep = g
		return r, nil
	}
	info, text, err := s.readChunk(ctx, p, in.Offset, in.Bytes)
	if errors.Is(err, errBinary) {
		return nil, fmt.Errorf("%s is not a text file", p)
	}
	if err != nil {
		return nil, err
	}
	r.Chunk, r.Text = info, policy.Wrap(text, 0)
	markUntrusted(ctx)
	return r, nil
}
