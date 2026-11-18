package query

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/mailsvb/go-syslog/internal/writer"
)

// maxAppsPerRow bounds the distinct tags counted per row; the rest are
// counted as "_other".
const maxAppsPerRow = 1000

// StatsParams selects what to count.
type StatsParams struct {
	Source  string
	From    time.Time // default To-24h
	To      time.Time // default now
	ByHour  bool      // one row per source and hour instead of per source
	TopApps int       // number of most frequent tags per row (default 5)
}

// AppCount is the number of lines of one syslog tag / app name.
type AppCount struct {
	App   string `json:"app"`
	Lines int64  `json:"lines"`
}

// StatsRow holds the counts for one source (and hour).
type StatsRow struct {
	Source   string           `json:"source"`
	Hour     string           `json:"hour,omitempty"`
	Lines    int64            `json:"lines"`
	Bytes    int64            `json:"bytes"`
	Severity map[string]int64 `json:"severity" jsonschema:"lines per severity; none = message without PRI"`
	First    string           `json:"first,omitempty"`
	Last     string           `json:"last,omitempty"`
	TopApps  []AppCount       `json:"top_apps,omitempty" jsonschema:"most frequent syslog tags / app names"`

	apps map[string]*int64
}

// StatsResult is the result of Stats.
type StatsResult struct {
	From         string     `json:"from"`
	To           string     `json:"to"`
	Rows         []StatsRow `json:"rows"`
	Partial      bool       `json:"partial,omitempty" jsonschema:"the scan limit was hit; only hours from oldest_scanned on are counted"`
	OldestHour   string     `json:"oldest_scanned,omitempty"`
	ScannedFiles int        `json:"scanned_files"`
	ScannedBytes int64      `json:"scanned_bytes"`
}

// Stats counts lines per source (or source and hour), severity and tag.
// Files are read newest first, so a partial result covers the latest hours.
func (s *Store) Stats(ctx context.Context, p StatsParams) (*StatsResult, error) {
	sel, err := parseSelector(p.Source)
	if err != nil {
		return nil, err
	}
	from, to := s.normalizeWindow(p.From, p.To)
	if !from.Before(to) {
		return nil, errors.New("from must be before to")
	}
	if p.TopApps <= 0 {
		p.TopApps = 5
	}
	files, err := s.files(sel, from, to)
	if err != nil {
		return nil, err
	}
	slices.Reverse(files)

	res := &StatsResult{From: from.Format(time.RFC3339), To: to.Format(time.RFC3339)}
	rows := map[[2]string]*StatsRow{}
	fromKey, toKey := tsKey(from), tsKey(to)
	bud := budget{ctx: ctx, maxBytes: s.limits.MaxScanBytes, deadline: time.Now().Add(s.limits.MaxScanTime)}

files:
	for _, f := range files {
		key := [2]string{f.source, f.hour.Format(time.RFC3339)}
		row := rows[key]
		if row == nil {
			row = &StatsRow{Source: key[0], Hour: key[1], Severity: map[string]int64{}, apps: map[string]*int64{}}
			rows[key] = row
		}
		fh, err := s.root.Open(f.rel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		res.ScannedFiles++
		lr := newLineReader(fh, 0)
		base := res.ScannedBytes
		for {
			line, _, err := lr.next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fh.Close()
				return nil, err
			}
			if stop, err := bud.exceeded(base + lr.read); err != nil {
				fh.Close()
				return nil, err
			} else if stop {
				// Drop the half-counted hour so every reported hour is complete.
				fh.Close()
				res.ScannedBytes = base + lr.read
				res.Partial = true
				delete(rows, key)
				break files
			}
			pl := parseLine(line)
			if pl.ts != nil && (bytes.Compare(pl.ts, fromKey) < 0 || bytes.Compare(pl.ts, toKey) >= 0) {
				continue
			}
			row.add(pl, len(line)+1)
		}
		fh.Close()
		res.ScannedBytes = base + lr.read
		res.OldestHour = f.hour.Format(time.RFC3339)
	}

	if !p.ByHour {
		rows = mergeHours(rows)
	}
	for _, row := range rows {
		if row.Lines == 0 {
			continue
		}
		row.finish(p.TopApps)
		res.Rows = append(res.Rows, *row)
	}
	slices.SortFunc(res.Rows, func(a, b StatsRow) int {
		if p.ByHour {
			return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.Hour, b.Hour))
		}
		return cmp.Or(cmp.Compare(b.Lines, a.Lines), cmp.Compare(a.Source, b.Source))
	})
	if res.Rows == nil {
		res.Rows = []StatsRow{}
	}
	if !res.Partial {
		res.OldestHour = ""
	}
	return res, nil
}

func (r *StatsRow) add(pl parsedLine, size int) {
	r.Lines++
	r.Bytes += int64(size)
	if pl.severity < 0 {
		r.Severity["none"]++
	} else {
		r.Severity[SeverityNames[pl.severity]]++
	}
	if pl.ts != nil {
		if r.First == "" || string(pl.ts) < r.First {
			r.First = string(pl.ts)
		}
		if string(pl.ts) > r.Last {
			r.Last = string(pl.ts)
		}
	}
	app := writer.ParseTag(pl.msg)
	if len(app) == 0 {
		app = noApp
	} else if len(app) > writer.MaxTagLen {
		app = app[:writer.MaxTagLen]
	}
	if c := r.apps[string(app)]; c != nil {
		*c++
		return
	}
	name := string(app)
	if len(r.apps) >= maxAppsPerRow {
		name = "_other"
		if c := r.apps[name]; c != nil {
			*c++
			return
		}
	}
	n := int64(1)
	r.apps[name] = &n
}

// noApp counts messages without a tag.
var noApp = []byte("_none")

// mergeHours folds the per-hour rows into one row per source.
func mergeHours(rows map[[2]string]*StatsRow) map[[2]string]*StatsRow {
	out := map[[2]string]*StatsRow{}
	for k, r := range rows {
		key := [2]string{k[0], ""}
		m := out[key]
		if m == nil {
			m = &StatsRow{Source: k[0], Severity: map[string]int64{}, apps: map[string]*int64{}}
			out[key] = m
		}
		m.Lines += r.Lines
		m.Bytes += r.Bytes
		for sev, n := range r.Severity {
			m.Severity[sev] += n
		}
		if r.First != "" && (m.First == "" || r.First < m.First) {
			m.First = r.First
		}
		m.Last = max(m.Last, r.Last)
		for app, n := range r.apps {
			if c := m.apps[app]; c != nil {
				*c += *n
			} else if len(m.apps) < maxAppsPerRow {
				v := *n
				m.apps[app] = &v
			} else {
				if m.apps["_other"] == nil {
					m.apps["_other"] = new(int64)
				}
				*m.apps["_other"] += *n
			}
		}
	}
	return out
}

func (r *StatsRow) finish(top int) {
	for app, n := range r.apps {
		r.TopApps = append(r.TopApps, AppCount{App: app, Lines: *n})
	}
	slices.SortFunc(r.TopApps, func(a, b AppCount) int {
		return cmp.Or(cmp.Compare(b.Lines, a.Lines), cmp.Compare(a.App, b.App))
	})
	if len(r.TopApps) > top {
		r.TopApps = r.TopApps[:top]
	}
	r.apps = nil
}
