package query

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// MaxContextLines caps before and after in Context.
const MaxContextLines = 200

// ContextLine is a line around the requested one.
type ContextLine struct {
	Line
	Target bool `json:"target,omitempty" jsonschema:"the line the ref points to"`
}

// ContextResult holds the lines around a ref.
type ContextResult struct {
	Source      string        `json:"source"`
	Hour        string        `json:"hour"`
	Lines       []ContextLine `json:"lines"`
	AtFileStart bool          `json:"at_file_start,omitempty" jsonschema:"no earlier lines in this hour file; earlier ones are in the previous hour"`
	AtFileEnd   bool          `json:"at_file_end,omitempty" jsonschema:"no later lines in this hour file (yet)"`
}

// ErrBadRef is returned for a ref that does not point to a line.
var ErrBadRef = errors.New("invalid ref: use the ref of a line returned by search_logs")

// Context returns up to before lines before and after lines after the line
// that ref ("<file>:<offset>", from Line.Ref) points to, in file order.
func (s *Store) Context(ctx context.Context, ref string, before, after, lineBytes int) (*ContextResult, error) {
	i := strings.LastIndexByte(ref, ':')
	if i < 0 {
		return nil, ErrBadRef
	}
	f, ok := parseRel(ref[:i])
	off, err := strconv.ParseInt(ref[i+1:], 10, 64)
	if !ok || err != nil || off < 0 {
		return nil, ErrBadRef
	}
	before = min(max(before, 0), MaxContextLines)
	after = min(max(after, 0), MaxContextLines)
	if lineBytes <= 0 {
		lineBytes = DefaultLineBytes
	}

	fh, err := s.root.Open(f.rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("the file of this ref no longer exists (retention)")
		}
		return nil, err
	}
	defer fh.Close()
	if off > 0 {
		var b [1]byte
		if _, err := fh.ReadAt(b[:], off-1); err != nil || b[0] != '\n' {
			return nil, ErrBadRef
		}
	}
	res := &ContextResult{Source: f.source, Hour: f.hour.Format("2006-01-02T15:04:05Z")}
	mk := func(line []byte, lineOff int64, target bool) ContextLine {
		pl := parseLine(line)
		msg, clipped := clip(pl.msg, lineBytes)
		return ContextLine{Line: Line{
			Time: string(pl.ts), Source: f.source, Severity: severityName(pl.severity),
			Message: msg, Clipped: clipped, Ref: f.rel + ":" + strconv.FormatInt(lineOff, 10),
		}, Target: target}
	}

	rr := newReverseReader(fh, off)
	for len(res.Lines) < before {
		line, lineOff, err := rr.prev()
		if errors.Is(err, io.EOF) {
			res.AtFileStart = true
			break
		}
		if err != nil {
			return nil, err
		}
		res.Lines = append(res.Lines, mk(line, lineOff, false))
	}
	if off == 0 {
		res.AtFileStart = true
	}
	slices.Reverse(res.Lines)

	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	lr := newLineReader(fh, off)
	for n := 0; n <= after; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, lineOff, err := lr.next()
		if errors.Is(err, io.EOF) {
			if n == 0 {
				return nil, ErrBadRef
			}
			res.AtFileEnd = true
			break
		}
		if err != nil {
			return nil, err
		}
		res.Lines = append(res.Lines, mk(line, lineOff, n == 0))
	}
	return res, nil
}
