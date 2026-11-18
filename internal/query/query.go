// Package query reads the log tree written by go-syslog:
//
//	<dir>/<ip>/<YYYY-MM-DD_HH>.log
//	<dir>/<ip>/<tag>/<YYYY-MM-DD_HH>.log
//
// It is read-only and meant for tools that run next to the collector, such as
// the go-syslog-view MCP server. All file access goes through an os.Root, so
// names that come from a client can never reach files outside the log
// directory.
package query

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/mailsvb/go-syslog/internal/writer"
)

// Limits bound the work a single request may do. A request that hits a
// limit returns what it found so far together with a cursor to continue.
type Limits struct {
	MaxScanBytes int64         // bytes read per request
	MaxScanTime  time.Duration // wall time per request
}

// DefaultLimits are used when the Store is created with zero Limits.
var DefaultLimits = Limits{MaxScanBytes: 1 << 30, MaxScanTime: 20 * time.Second}

// Store gives read-only access to a go-syslog log directory.
type Store struct {
	root   *os.Root
	limits Limits
	now    func() time.Time
}

// Open opens the log directory dir.
func Open(dir string, limits Limits) (*Store, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	if limits.MaxScanBytes <= 0 {
		limits.MaxScanBytes = DefaultLimits.MaxScanBytes
	}
	if limits.MaxScanTime <= 0 {
		limits.MaxScanTime = DefaultLimits.MaxScanTime
	}
	return &Store{root: r, limits: limits, now: time.Now}, nil
}

// Close releases the directory handle.
func (s *Store) Close() error { return s.root.Close() }

// Now returns the current time of the store (UTC).
func (s *Store) Now() time.Time { return s.now().UTC() }

// SetClock replaces the clock; for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// ErrBadSource is returned for a source selector that is not "<ip>" or "<ip>/<tag>".
var ErrBadSource = errors.New(`source must be "<ip>" or "<ip>/<tag>" as returned by list_sources`)

// selector restricts a request to one sender, or one tag of a sender.
type selector struct {
	ip  string // canonical address, "" = all senders
	tag string // "" = the sender's own files and all of its tags
}

func parseSelector(s string) (selector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return selector{}, nil
	}
	ipPart, tag, hasTag := strings.Cut(s, "/")
	a, err := netip.ParseAddr(ipPart)
	if err != nil || a.Zone() != "" {
		return selector{}, ErrBadSource
	}
	a = a.Unmap()
	if hasTag && !writer.ValidTagDir(tag) {
		return selector{}, ErrBadSource
	}
	return selector{ip: a.String(), tag: tag}, nil
}

// logFile is one hour file.
type logFile struct {
	rel    string    // path relative to the root, with '/' separators
	source string    // "<ip>" or "<ip>/<tag>"
	hour   time.Time // start of the hour (UTC)
	size   int64
	mtime  time.Time
}

// less orders files by hour, then by source.
func (f logFile) less(g logFile) int {
	if c := f.hour.Compare(g.hour); c != 0 {
		return c
	}
	return strings.Compare(f.source, g.source)
}

// parseHourName returns the hour of a log file name, or false.
func parseHourName(name string) (time.Time, bool) {
	base, ok := strings.CutSuffix(name, writer.FileExt)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(writer.HourLayout, base, time.UTC)
	return t, err == nil
}

// parseRel turns a relative file path into a logFile (without size and
// mtime), checking that it has the shape of a file written by go-syslog.
func parseRel(rel string) (logFile, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return logFile{}, false
	}
	a, err := netip.ParseAddr(parts[0])
	if err != nil || a.String() != parts[0] {
		return logFile{}, false
	}
	source := parts[0]
	if len(parts) == 3 {
		if !writer.ValidTagDir(parts[1]) {
			return logFile{}, false
		}
		source += "/" + parts[1]
	}
	h, ok := parseHourName(parts[len(parts)-1])
	if !ok {
		return logFile{}, false
	}
	return logFile{rel: rel, source: source, hour: h}, true
}

// readDir lists a directory below the root. A missing directory is empty.
func (s *Store) readDir(name string) ([]fs.DirEntry, error) {
	f, err := s.root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// files returns the hour files of the selected sources whose hour overlaps
// [from, to), sorted by hour and source. A zero from or to is unbounded.
func (s *Store) files(sel selector, from, to time.Time) ([]logFile, error) {
	var ips []string
	if sel.ip != "" {
		ips = []string{sel.ip}
	} else {
		entries, err := s.readDir(".")
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if a, err := netip.ParseAddr(e.Name()); err == nil && a.String() == e.Name() {
				ips = append(ips, e.Name())
			}
		}
	}
	var out []logFile
	add := func(dir, source string) error {
		entries, err := s.readDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			h, ok := parseHourName(e.Name())
			if !ok {
				continue
			}
			if !from.IsZero() && !h.Add(time.Hour).After(from) {
				continue
			}
			if !to.IsZero() && !h.Before(to) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue // deleted by retention in the meantime
			}
			out = append(out, logFile{
				rel: path.Join(dir, e.Name()), source: source, hour: h,
				size: info.Size(), mtime: info.ModTime(),
			})
		}
		return nil
	}
	for _, ip := range ips {
		if sel.tag != "" {
			if err := add(path.Join(ip, sel.tag), ip+"/"+sel.tag); err != nil {
				return nil, err
			}
			continue
		}
		if err := add(ip, ip); err != nil {
			return nil, err
		}
		entries, err := s.readDir(ip)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() && writer.ValidTagDir(e.Name()) {
				if err := add(path.Join(ip, e.Name()), ip+"/"+e.Name()); err != nil {
					return nil, err
				}
			}
		}
	}
	slices.SortFunc(out, logFile.less)
	return out, nil
}

// Source describes one sender (or one tag of a split sender).
type Source struct {
	Source    string `json:"source" jsonschema:"selector for the other tools: <ip> or <ip>/<tag>"`
	IP        string `json:"ip"`
	Tag       string `json:"tag,omitempty"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	FirstHour string `json:"first_hour" jsonschema:"start of the oldest hour file (UTC)"`
	LastWrite string `json:"last_write" jsonschema:"modification time of the newest file (UTC)"`
}

// Sources lists all senders and tags, newest activity first.
func (s *Store) Sources(sourceFilter string) ([]Source, error) {
	sel, err := parseSelector(sourceFilter)
	if err != nil {
		return nil, err
	}
	files, err := s.files(sel, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	bySource := map[string]*Source{}
	last := map[string]time.Time{}
	first := map[string]time.Time{}
	for _, f := range files {
		src, ok := bySource[f.source]
		if !ok {
			ip, tag, _ := strings.Cut(f.source, "/")
			src = &Source{Source: f.source, IP: ip, Tag: tag}
			bySource[f.source] = src
			first[f.source] = f.hour
		}
		src.Files++
		src.Bytes += f.size
		if f.hour.Before(first[f.source]) {
			first[f.source] = f.hour
		}
		if f.mtime.After(last[f.source]) {
			last[f.source] = f.mtime
		}
	}
	out := make([]Source, 0, len(bySource))
	for k, src := range bySource {
		src.FirstHour = first[k].Format(time.RFC3339)
		src.LastWrite = last[k].UTC().Format(time.RFC3339)
		out = append(out, *src)
	}
	slices.SortFunc(out, func(a, b Source) int {
		if c := last[b.Source].Compare(last[a.Source]); c != 0 {
			return c
		}
		return strings.Compare(a.Source, b.Source)
	})
	return out, nil
}

// ParseTime parses a time argument: RFC 3339 ("2026-11-18T13:04:05Z",
// fractional seconds and offsets allowed), "now", or a duration before now
// such as "15m", "2h", "3d" or "1d12h" (a leading "-" is accepted too).
// An empty string returns the zero time.
func ParseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return time.Time{}, nil
	case strings.EqualFold(s, "now"):
		return now.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	d, err := parseAgo(strings.TrimPrefix(s, "-"))
	if err != nil {
		return time.Time{}, fmt.Errorf("time %q: use RFC 3339 (2026-11-18T13:04:05Z), \"now\" or a duration ago like 30m, 2h, 3d", s)
	}
	return now.Add(-d).UTC(), nil
}

// parseAgo parses a Go duration that may also use a "d" (24h) unit.
func parseAgo(s string) (time.Duration, error) {
	var days time.Duration
	if i := strings.IndexByte(s, 'd'); i > 0 {
		var n int
		if _, err := fmt.Sscanf(s[:i], "%d", &n); err != nil || fmt.Sprint(n) != s[:i] || n < 0 {
			return 0, errors.New("bad days")
		}
		days = time.Duration(n) * 24 * time.Hour
		s = s[i+1:]
		if s == "" {
			return days, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, errors.New("bad duration")
	}
	return days + d, nil
}
