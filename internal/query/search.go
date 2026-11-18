package query

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Defaults and caps for search requests.
const (
	DefaultLimit     = 50
	MaxLimit         = 500
	DefaultMaxBytes  = 32 << 10
	MaxMaxBytes      = 256 << 10
	DefaultLineBytes = 1000
	DefaultWindow    = 24 * time.Hour
)

// SearchParams selects log lines. Zero values mean "no filter" or the default.
type SearchParams struct {
	Source      string    // "<ip>" or "<ip>/<tag>", empty = all
	From, To    time.Time // receive time; To defaults to now, From to To-24h
	Contains    string    // case-insensitive substring of the message
	Regex       string    // RE2 regular expression on the message
	MaxSeverity int       // only lines with severity <= this (0 = emerg); -1 = all
	Oldest      bool      // oldest first; default is newest first
	Limit       int       // lines per page
	MaxBytes    int       // approximate size of the returned messages
	LineBytes   int       // messages longer than this are clipped
	Cursor      string    // next_cursor of the previous page
}

// Line is one log line.
type Line struct {
	Time     string `json:"time" jsonschema:"receive time (UTC)"`
	Source   string `json:"source"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message" jsonschema:"raw syslog message as received"`
	Clipped  bool   `json:"clipped,omitempty" jsonschema:"message was cut to line_bytes"`
	Ref      string `json:"ref" jsonschema:"position of the line, for get_context"`
}

// SearchResult is one page of results.
type SearchResult struct {
	Lines        []Line `json:"lines"`
	From         string `json:"from"`
	To           string `json:"to"`
	Order        string `json:"order"`
	NextCursor   string `json:"next_cursor,omitempty" jsonschema:"pass as cursor (with the same filters) for the next page; absent when everything was searched"`
	Stopped      string `json:"stopped,omitempty" jsonschema:"why the page ended early: limit, max_bytes or scan_limit"`
	ScannedFiles int    `json:"scanned_files"`
	ScannedBytes int64  `json:"scanned_bytes"`
}

// cursor is a position in a file: for oldest-first the offset of the next
// line to read, for newest-first the end of the region still to read.
type cursor struct {
	File string `json:"f"`
	Off  int64  `json:"o"`
	Mode string `json:"m"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s, mode string) (cursor, logFile, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	f, ok := parseRel(c.File)
	if err != nil || !ok || c.Off < 0 || c.Mode != mode {
		return c, f, errors.New("invalid cursor (it only works with the same filters and order)")
	}
	return c, f, nil
}

// matcher tests the message part of a line.
type matcher struct {
	fold   []byte         // lower-case ASCII needle for case-insensitive search
	foldRe *regexp.Regexp // case-insensitive search for a non-ASCII needle
	re     *regexp.Regexp
	maxSev int
}

func newMatcher(p SearchParams) (*matcher, error) {
	m := &matcher{maxSev: p.MaxSeverity}
	if p.Contains != "" {
		if isASCII(p.Contains) {
			m.fold = []byte(strings.ToLower(p.Contains))
		} else {
			m.foldRe = regexp.MustCompile("(?i)" + regexp.QuoteMeta(p.Contains))
		}
	}
	if p.Regex != "" {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("regex: %w", err)
		}
		m.re = re
	}
	return m, nil
}

func (m *matcher) match(p parsedLine) bool {
	if m.maxSev >= 0 && (p.severity < 0 || p.severity > m.maxSev) {
		return false
	}
	if m.fold != nil && !containsFoldASCII(p.msg, m.fold) {
		return false
	}
	if m.foldRe != nil && !m.foldRe.Match(p.msg) {
		return false
	}
	return m.re == nil || m.re.Match(p.msg)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// containsFoldASCII reports whether h contains the lower-case ASCII needle n,
// ignoring ASCII case.
func containsFoldASCII(h, n []byte) bool {
	if len(n) == 0 {
		return true
	}
	first, firstUp := n[0], n[0]
	if first >= 'a' && first <= 'z' {
		firstUp = first - 'a' + 'A'
	}
	for i := 0; i+len(n) <= len(h); i++ {
		j := indexEither(h[i:len(h)-len(n)+1], first, firstUp)
		if j < 0 {
			return false
		}
		i += j
		if bytes.EqualFold(h[i:i+len(n)], n) {
			return true
		}
	}
	return false
}

func indexEither(b []byte, c1, c2 byte) int {
	if c1 == c2 {
		return bytes.IndexByte(b, c1)
	}
	i1, i2 := bytes.IndexByte(b, c1), bytes.IndexByte(b, c2)
	switch {
	case i1 < 0:
		return i2
	case i2 < 0:
		return i1
	}
	return min(i1, i2)
}

// tsKey formats t like the line timestamps, so they compare as strings.
func tsKey(t time.Time) []byte {
	return []byte(t.UTC().Format("2006-01-02T15:04:05.000Z"))
}

// budget tracks the scan limits of one request.
type budget struct {
	ctx      context.Context
	maxBytes int64
	deadline time.Time
	lines    int
}

// exceeded reports whether the request should stop scanning. It is called
// once per line before the line is used; the first line of a request always
// passes, so every page makes progress. The clock is read every 4096 lines.
func (b *budget) exceeded(scanned int64) (bool, error) {
	b.lines++
	if b.lines == 1 {
		return false, nil
	}
	if scanned > b.maxBytes {
		return true, nil
	}
	if b.lines&4095 == 0 {
		if err := b.ctx.Err(); err != nil {
			return false, err
		}
		if time.Now().After(b.deadline) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) normalizeWindow(from, to time.Time) (time.Time, time.Time) {
	if to.IsZero() {
		to = s.Now()
	}
	if from.IsZero() {
		from = to.Add(-DefaultWindow)
	}
	return from.UTC(), to.UTC()
}

// Search returns one page of lines that match p.
func (s *Store) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	sel, err := parseSelector(p.Source)
	if err != nil {
		return nil, err
	}
	from, to := s.normalizeWindow(p.From, p.To)
	if !from.Before(to) {
		return nil, errors.New("from must be before to")
	}
	if p.Limit <= 0 {
		p.Limit = DefaultLimit
	}
	p.Limit = min(p.Limit, MaxLimit)
	if p.MaxBytes <= 0 {
		p.MaxBytes = DefaultMaxBytes
	}
	p.MaxBytes = min(p.MaxBytes, MaxMaxBytes)
	if p.LineBytes <= 0 {
		p.LineBytes = DefaultLineBytes
	}
	m, err := newMatcher(p)
	if err != nil {
		return nil, err
	}
	mode := "newest"
	if p.Oldest {
		mode = "oldest"
	}

	files, err := s.files(sel, from, to)
	if err != nil {
		return nil, err
	}
	if !p.Oldest {
		slices.Reverse(files)
	}
	startOff := int64(-1) // -1: whole file
	if p.Cursor != "" {
		c, cf, err := decodeCursor(p.Cursor, mode)
		if err != nil {
			return nil, err
		}
		// Skip the files that come before the cursor file in this order. If
		// the cursor file is gone (retention), continue with the next one.
		i := 0
		for i < len(files) {
			cmp := files[i].less(cf)
			if !p.Oldest {
				cmp = -cmp
			}
			if cmp >= 0 {
				break
			}
			i++
		}
		files = files[i:]
		if len(files) > 0 && files[0].rel == cf.rel {
			startOff = c.Off
		}
	}

	res := &SearchResult{Lines: []Line{}, From: from.Format(time.RFC3339), To: to.Format(time.RFC3339), Order: mode}
	sc := &scan{
		s: s, p: p, m: m, res: res, mode: mode,
		fromKey: tsKey(from), toKey: tsKey(to),
		bud: budget{ctx: ctx, maxBytes: s.limits.MaxScanBytes, deadline: time.Now().Add(s.limits.MaxScanTime)},
	}
	for i, f := range files {
		off := int64(-1)
		if i == 0 {
			off = startOff
		}
		done, err := sc.file(f, off)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
	}
	return res, nil
}

// scan holds the state of one Search call.
type scan struct {
	s        *Store
	p        SearchParams
	m        *matcher
	res      *SearchResult
	mode     string
	fromKey  []byte
	toKey    []byte
	bud      budget
	outBytes int
}

// stop ends the page with a cursor at off in f.
func (sc *scan) stop(reason string, f logFile, off int64) (bool, error) {
	sc.res.Stopped = reason
	sc.res.NextCursor = cursor{File: f.rel, Off: off, Mode: sc.mode}.encode()
	return true, nil
}

// emit adds a matching line. It reports false if the line does not fit in
// the byte budget (and the page already has lines).
func (sc *scan) emit(f logFile, pl parsedLine, off int64) bool {
	msg, clipped := clip(pl.msg, sc.p.LineBytes)
	size := len(msg) + len(f.source) + 64
	if sc.outBytes+size > sc.p.MaxBytes && len(sc.res.Lines) > 0 {
		return false
	}
	sc.outBytes += size
	sc.res.Lines = append(sc.res.Lines, Line{
		Time:     string(pl.ts),
		Source:   f.source,
		Severity: severityName(pl.severity),
		Message:  msg,
		Clipped:  clipped,
		Ref:      f.rel + ":" + strconv.FormatInt(off, 10),
	})
	return true
}

// inWindow applies the time filter. Lines without a timestamp pass.
func (sc *scan) inWindow(pl parsedLine) bool {
	return pl.ts == nil || (bytes.Compare(pl.ts, sc.fromKey) >= 0 && bytes.Compare(pl.ts, sc.toKey) < 0)
}

// file scans one file. off is where to start (oldest-first) or the end of
// the region to scan (newest-first); -1 means the whole file. It reports
// whether the page is complete.
func (sc *scan) file(f logFile, off int64) (bool, error) {
	fh, err := sc.s.root.Open(f.rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // deleted by retention
		}
		return false, err
	}
	defer fh.Close()
	sc.res.ScannedFiles++
	if sc.p.Oldest {
		return sc.forward(f, fh, max(off, 0))
	}
	if off < 0 {
		info, err := fh.Stat()
		if err != nil {
			return false, err
		}
		off = info.Size()
	}
	return sc.backward(f, fh, off)
}

func (sc *scan) forward(f logFile, fh *os.File, off int64) (bool, error) {
	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	lr := newLineReader(fh, off)
	base := sc.res.ScannedBytes
	defer func() { sc.res.ScannedBytes = base + lr.read }()
	for {
		line, lineOff, err := lr.next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if stop, err := sc.bud.exceeded(base + lr.read); err != nil {
			return false, err
		} else if stop {
			return sc.stop("scan_limit", f, lineOff)
		}
		pl := parseLine(line)
		if !sc.inWindow(pl) || !sc.m.match(pl) {
			continue
		}
		if !sc.emit(f, pl, lineOff) {
			return sc.stop("max_bytes", f, lineOff)
		}
		if len(sc.res.Lines) >= sc.p.Limit {
			return sc.stop("limit", f, lr.off)
		}
	}
}

func (sc *scan) backward(f logFile, fh *os.File, bound int64) (bool, error) {
	rr := newReverseReader(fh, bound)
	base := sc.res.ScannedBytes
	defer func() { sc.res.ScannedBytes = base + rr.read }()
	for {
		line, lineOff, err := rr.prev()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		lineEnd := lineOff + int64(len(line)) + 1
		if stop, err := sc.bud.exceeded(base + rr.read); err != nil {
			return false, err
		} else if stop {
			return sc.stop("scan_limit", f, lineEnd)
		}
		pl := parseLine(line)
		if !sc.inWindow(pl) || !sc.m.match(pl) {
			continue
		}
		if !sc.emit(f, pl, lineOff) {
			return sc.stop("max_bytes", f, lineEnd)
		}
		if len(sc.res.Lines) >= sc.p.Limit {
			return sc.stop("limit", f, lineOff)
		}
	}
}
