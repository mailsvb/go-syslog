package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-syslog/internal/writer"
)

var now = time.Date(2026, 11, 18, 13, 30, 0, 0, time.UTC)

// fixture writes log files and returns the directory.
type fixture struct {
	t   *testing.T
	dir string
}

func newFixture(t *testing.T) *fixture {
	return &fixture{t: t, dir: t.TempDir()}
}

// add appends a line for msg received at ts to the file of source.
func (fx *fixture) add(source string, ts time.Time, msg string) {
	fx.t.Helper()
	ip, tag, _ := strings.Cut(source, "/")
	p := filepath.Join(fx.dir, ip, tag, ts.UTC().Format(writer.HourLayout)+writer.FileExt)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(writer.FormatLine(ts, []byte(msg))); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) raw(rel, data string) {
	fx.t.Helper()
	p := filepath.Join(fx.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) store(l Limits) *Store {
	fx.t.Helper()
	s, err := Open(fx.dir, l)
	if err != nil {
		fx.t.Fatal(err)
	}
	s.SetClock(func() time.Time { return now })
	fx.t.Cleanup(func() { s.Close() })
	return s
}

// standard builds 3 hours x 2 sources plus a tag source, 40 lines each.
// Every 10th line is an error, every line has a "seq=<n>" marker that is
// unique per source.
func standard(t *testing.T) *fixture {
	fx := newFixture(t)
	for h := 0; h < 3; h++ {
		for i := 0; i < 40; i++ {
			ts := now.Add(time.Duration(h-2)*time.Hour - 25*time.Minute + time.Duration(i)*time.Second)
			n := h*40 + i
			sev := 6
			if i%10 == 0 {
				sev = 3
			}
			fx.add("10.0.0.1", ts, fmt.Sprintf("<%d>1 2026-11-18T00:00:00Z host sshd 1 - - seq=%d hello", 8+sev, n))
			fx.add("10.0.0.2", ts.Add(time.Millisecond), fmt.Sprintf("<%d>Nov 18 00:00:00 host cron[12]: seq=%d HELLO", 8+sev, n))
			fx.add("127.0.0.1/web", ts.Add(2*time.Millisecond), fmt.Sprintf("<%d>web: seq=%d Grüße", 8+sev, n))
		}
	}
	return fx
}

func collect(t *testing.T, s *Store, p SearchParams) []Line {
	t.Helper()
	var all []Line
	for page := 0; ; page++ {
		if page > 10000 {
			t.Fatal("too many pages")
		}
		r, err := s.Search(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, r.Lines...)
		if r.NextCursor == "" {
			return all
		}
		p.Cursor = r.NextCursor
	}
}

func TestSources(t *testing.T) {
	fx := standard(t)
	fx.raw("not-an-ip/2026-11-18_10.log", "x\n")
	fx.raw("10.0.0.1/readme.txt", "x\n")
	fx.raw("10.0.0.1/bad tag/2026-11-18_10.log", "x\n")
	s := fx.store(Limits{})
	src, err := s.Sources("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range src {
		names = append(names, x.Source)
		if x.Files != 3 {
			t.Errorf("%s: files = %d, want 3", x.Source, x.Files)
		}
	}
	slices.Sort(names)
	if want := []string{"10.0.0.1", "10.0.0.2", "127.0.0.1/web"}; !slices.Equal(names, want) {
		t.Fatalf("sources = %v, want %v", names, want)
	}
	if _, err := s.Sources("../etc"); err == nil {
		t.Fatal("expected error for bad source")
	}
}

func TestSearchPagingCoversEverything(t *testing.T) {
	fx := standard(t)
	s := fx.store(Limits{})
	for _, oldest := range []bool{false, true} {
		lines := collect(t, s, SearchParams{MaxSeverity: -1, Oldest: oldest, Limit: 7})
		if len(lines) != 360 {
			t.Fatalf("oldest=%v: got %d lines, want 360", oldest, len(lines))
		}
		seen := map[string]bool{}
		for i, l := range lines {
			if seen[l.Ref] {
				t.Fatalf("duplicate %s", l.Ref)
			}
			seen[l.Ref] = true
			if i == 0 {
				continue
			}
			prev := lines[i-1]
			// Files are visited hour by hour; within an hour, per source.
			if prev.Time[:13] != l.Time[:13] {
				if oldest == (prev.Time > l.Time) {
					t.Fatalf("oldest=%v: hour order broken at %d: %s then %s", oldest, i, prev.Time, l.Time)
				}
			} else if prev.Source == l.Source && oldest == (prev.Time > l.Time) {
				t.Fatalf("oldest=%v: line order broken at %d: %s then %s", oldest, i, prev.Time, l.Time)
			}
		}
	}
}

func TestSearchNewestFirstStartsWithLatest(t *testing.T) {
	fx := standard(t)
	s := fx.store(Limits{})
	r, err := s.Search(context.Background(), SearchParams{Source: "10.0.0.1", MaxSeverity: -1, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Lines) != 3 || !strings.Contains(r.Lines[0].Message, "seq=119 ") || !strings.Contains(r.Lines[2].Message, "seq=117 ") {
		t.Fatalf("unexpected lines %+v", r.Lines)
	}
	if r.Stopped != "limit" || r.NextCursor == "" {
		t.Fatalf("stopped=%q cursor=%q", r.Stopped, r.NextCursor)
	}
	if _, err := s.Search(context.Background(), SearchParams{MaxSeverity: -1, Oldest: true, Cursor: r.NextCursor}); err == nil {
		t.Fatal("cursor of the other order must be rejected")
	}
}

func TestSearchFilters(t *testing.T) {
	fx := standard(t)
	s := fx.store(Limits{})
	cases := []struct {
		name string
		p    SearchParams
		want int
	}{
		{"contains ascii fold", SearchParams{Contains: "hello", MaxSeverity: -1}, 240},
		{"contains non-ascii fold", SearchParams{Contains: "GRÜSSE", MaxSeverity: -1}, 0},
		{"contains non-ascii", SearchParams{Contains: "grüße", MaxSeverity: -1}, 120},
		{"regex", SearchParams{Regex: `seq=1[0-9] `, MaxSeverity: -1}, 30},
		{"severity", SearchParams{MaxSeverity: 3}, 36},
		{"source", SearchParams{Source: "10.0.0.2", MaxSeverity: -1}, 120},
		{"tag", SearchParams{Source: "127.0.0.1/web", MaxSeverity: -1}, 120},
		{"ip includes tags", SearchParams{Source: "127.0.0.1", MaxSeverity: -1}, 120},
		{"window", SearchParams{From: now.Add(-80 * time.Minute), To: now.Add(-60 * time.Minute), MaxSeverity: -1}, 0},
		{"window one hour", SearchParams{From: now.Add(-86 * time.Minute), To: now.Add(-84 * time.Minute), MaxSeverity: -1}, 120},
		{"combined", SearchParams{Source: "10.0.0.1", Contains: "SSHD", MaxSeverity: 3, Regex: "seq=4"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.p.Limit = 500
			if got := len(collect(t, s, tc.p)); got != tc.want {
				t.Fatalf("got %d lines, want %d", got, tc.want)
			}
		})
	}
	if _, err := s.Search(context.Background(), SearchParams{Regex: "("}); err == nil {
		t.Fatal("bad regex must fail")
	}
}

func TestSearchMaxBytesAndClip(t *testing.T) {
	fx := newFixture(t)
	for i := 0; i < 20; i++ {
		fx.add("10.0.0.9", now.Add(-time.Minute+time.Duration(i)*time.Second), fmt.Sprintf("<14>%d %s", i, strings.Repeat("x", 2000)))
	}
	s := fx.store(Limits{})
	r, err := s.Search(context.Background(), SearchParams{MaxSeverity: -1, MaxBytes: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Lines) != 3 || r.Stopped != "max_bytes" || !r.Lines[0].Clipped || len(r.Lines[0].Message) != DefaultLineBytes {
		t.Fatalf("lines=%d stopped=%q clipped=%v len=%d", len(r.Lines), r.Stopped, r.Lines[0].Clipped, len(r.Lines[0].Message))
	}
	if got := len(collect(t, s, SearchParams{MaxSeverity: -1, MaxBytes: 4000})); got != 20 {
		t.Fatalf("paged %d lines, want 20", got)
	}
}

func TestSearchScanLimitMakesProgress(t *testing.T) {
	fx := standard(t)
	s := fx.store(Limits{MaxScanBytes: 2000})
	for _, oldest := range []bool{false, true} {
		if got := len(collect(t, s, SearchParams{MaxSeverity: 3, Oldest: oldest})); got != 36 {
			t.Fatalf("oldest=%v: got %d, want 36", oldest, got)
		}
	}
}

func TestSearchIgnoresUnterminatedLine(t *testing.T) {
	fx := newFixture(t)
	fx.add("10.0.0.3", now.Add(-time.Minute), "<14>complete")
	fx.raw("10.0.0.3/"+now.Format(writer.HourLayout)+".log", "2026-11-18T13:29:59.000Z <14>partial")
	s := fx.store(Limits{})
	for _, oldest := range []bool{false, true} {
		lines := collect(t, s, SearchParams{MaxSeverity: -1, Oldest: oldest})
		if len(lines) != 1 || lines[0].Message != "<14>complete" {
			t.Fatalf("oldest=%v: %+v", oldest, lines)
		}
	}
}

func TestReverseReaderLongFile(t *testing.T) {
	fx := newFixture(t)
	var want []string
	for i := 0; i < 3000; i++ { // ~150 KB, several reverse chunks
		msg := fmt.Sprintf("<14>line %d %s", i, strings.Repeat("y", i%97))
		fx.add("10.0.0.4", now.Add(-20*time.Minute+time.Duration(i)*time.Millisecond), msg)
		want = append(want, msg)
	}
	s := fx.store(Limits{})
	lines := collect(t, s, SearchParams{MaxSeverity: -1, Limit: 500})
	if len(lines) != len(want) {
		t.Fatalf("got %d, want %d", len(lines), len(want))
	}
	for i, l := range lines {
		if l.Message != want[len(want)-1-i] {
			t.Fatalf("line %d = %q, want %q", i, l.Message, want[len(want)-1-i])
		}
	}
}

func TestContext(t *testing.T) {
	fx := standard(t)
	s := fx.store(Limits{})
	r, err := s.Search(context.Background(), SearchParams{Source: "10.0.0.1", Regex: `seq=85 `, MaxSeverity: -1})
	if err != nil || len(r.Lines) != 1 {
		t.Fatalf("search: %v %+v", err, r)
	}
	c, err := s.Context(context.Background(), r.Lines[0].Ref, 3, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Lines) != 6 || !c.Lines[3].Target || !strings.Contains(c.Lines[0].Message, "seq=82 ") || !strings.Contains(c.Lines[5].Message, "seq=87 ") {
		t.Fatalf("context = %+v", c.Lines)
	}
	// seq=80 is the first line of its hour file.
	r, _ = s.Search(context.Background(), SearchParams{Source: "10.0.0.1", Regex: `seq=80 `, MaxSeverity: -1})
	c, err = s.Context(context.Background(), r.Lines[0].Ref, 5, 0, 0)
	if err != nil || len(c.Lines) != 1 || !c.AtFileStart {
		t.Fatalf("at start: %v %+v", err, c)
	}
	for _, bad := range []string{"", "x", "../../etc/passwd:0", "10.0.0.1/../../x/2026-11-18_13.log:0", r.Lines[0].Ref[:len(r.Lines[0].Ref)-1] + "7", "10.0.0.1/2026-11-18_13.log:999999"} {
		if _, err := s.Context(context.Background(), bad, 1, 1, 0); err == nil {
			t.Errorf("ref %q: expected error", bad)
		}
	}
}

func TestStats(t *testing.T) {
	fx := standard(t)
	s := fx.store(Limits{})
	r, err := s.Stats(context.Background(), StatsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 3 || r.Partial {
		t.Fatalf("rows = %+v", r)
	}
	for _, row := range r.Rows {
		if row.Lines != 120 || row.Severity["err"] != 12 || row.Severity["info"] != 108 {
			t.Errorf("%s: lines=%d severity=%v", row.Source, row.Lines, row.Severity)
		}
		want := map[string]string{"10.0.0.1": "sshd", "10.0.0.2": "cron", "127.0.0.1/web": "web"}[row.Source]
		if len(row.TopApps) != 1 || row.TopApps[0].App != want || row.TopApps[0].Lines != 120 {
			t.Errorf("%s: top apps %+v", row.Source, row.TopApps)
		}
	}
	r, err = s.Stats(context.Background(), StatsParams{Source: "10.0.0.2", ByHour: true})
	if err != nil || len(r.Rows) != 3 || r.Rows[0].Hour > r.Rows[1].Hour || r.Rows[0].Lines != 40 {
		t.Fatalf("by hour: %v %+v", err, r)
	}

	small := fx.store(Limits{MaxScanBytes: 1})
	r, err = small.Stats(context.Background(), StatsParams{ByHour: true})
	if err != nil || !r.Partial || len(r.Rows) != 0 {
		t.Fatalf("partial: %v %+v", err, r)
	}
}

func TestParseTime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"":                          {},
		"now":                       now,
		"2h":                        now.Add(-2 * time.Hour),
		"-30m":                      now.Add(-30 * time.Minute),
		"1d12h":                     now.Add(-36 * time.Hour),
		"2026-11-18T10:00:00+01:00": time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
	} {
		got, err := ParseTime(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "1x", "d", "1.5d"} {
		if _, err := ParseTime(bad, now); err == nil {
			t.Errorf("ParseTime(%q): expected error", bad)
		}
	}
}

func TestParseSeverity(t *testing.T) {
	for in, want := range map[string]int{"": -1, "err": 3, "error": 3, "warn": 4, "7": 7, "emerg": 0} {
		if got, err := ParseSeverity(in); err != nil || got != want {
			t.Errorf("ParseSeverity(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParseSeverity("loud"); err == nil {
		t.Error("expected error")
	}
}

func TestClip(t *testing.T) {
	s, cut := clip([]byte("aé"), 2) // é is 2 bytes; cut inside it
	if s != "a" || !cut {
		t.Fatalf("clip = %q %v", s, cut)
	}
	s, cut = clip([]byte{'a', 0xff}, 10)
	if s != "a�" || cut {
		t.Fatalf("clip invalid = %q %v", s, cut)
	}
}
