package query

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

// maxLineBytes bounds how much of one line is kept in memory. go-syslog lines
// are at most about 4 x 64 KiB (an escaped datagram), so this is never hit by
// files the collector wrote; the rest of a longer line is skipped.
const maxLineBytes = 1 << 20

// tsLen is the length of the receive timestamp at the start of each line,
// "2006-01-02T15:04:05.000Z".
const tsLen = len("2006-01-02T15:04:05.000Z")

// lineReader reads complete lines and their offsets. A last line without a
// newline is not returned: the collector may still be writing it (bufio
// flushes can end in the middle of a line).
type lineReader struct {
	r    *bufio.Reader
	off  int64 // offset of the next line
	buf  []byte
	read int64 // bytes consumed, for scan limits
}

func newLineReader(r io.Reader, off int64) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, 64<<10), off: off}
}

// next returns the next line without its newline and the line's offset. The
// slice is valid until the next call. It returns io.EOF at the end.
func (lr *lineReader) next() ([]byte, int64, error) {
	lr.buf = lr.buf[:0]
	start := lr.off
	for {
		chunk, err := lr.r.ReadSlice('\n')
		lr.off += int64(len(chunk))
		lr.read += int64(len(chunk))
		if room := maxLineBytes - len(lr.buf); room > 0 {
			lr.buf = append(lr.buf, chunk[:min(len(chunk), room)]...)
		}
		switch {
		case err == nil:
			line := lr.buf
			if n := len(line); n > 0 && line[n-1] == '\n' {
				line = line[:n-1]
			}
			return line, start, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, start, err // io.EOF also for an unterminated last line
		}
	}
}

// parsedLine is a log line split into its parts.
type parsedLine struct {
	ts       []byte // receive timestamp, nil if the line has none
	msg      []byte // raw message (everything after the timestamp)
	severity int    // 0-7, -1 if the message has no PRI
}

func parseLine(line []byte) parsedLine {
	p := parsedLine{msg: line, severity: -1}
	if len(line) > tsLen && line[tsLen] == ' ' && line[tsLen-1] == 'Z' && line[10] == 'T' {
		p.ts, p.msg = line[:tsLen], line[tsLen+1:]
	}
	p.severity = severityOf(p.msg)
	return p
}

// severityOf returns the severity from the "<PRI>" at the start of msg.
func severityOf(msg []byte) int {
	if len(msg) < 3 || msg[0] != '<' {
		return -1
	}
	pri := 0
	i := 1
	for ; i < len(msg) && i <= 3 && msg[i] >= '0' && msg[i] <= '9'; i++ {
		pri = pri*10 + int(msg[i]-'0')
	}
	if i == 1 || i >= len(msg) || msg[i] != '>' || pri > 191 {
		return -1
	}
	return pri % 8
}

// SeverityNames are the RFC 5424 severity keywords, indexed by severity.
var SeverityNames = [8]string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

var severityAliases = map[string]int{
	"emergency": 0, "panic": 0, "critical": 2, "error": 3, "warn": 4, "informational": 6,
}

// ParseSeverity returns the severity for a keyword ("err", "warning", ...),
// a common alias ("error", "warn", ...) or a digit 0-7. Empty returns -1.
func ParseSeverity(s string) (int, error) {
	if s == "" {
		return -1, nil
	}
	if len(s) == 1 && s[0] >= '0' && s[0] <= '7' {
		return int(s[0] - '0'), nil
	}
	for i, n := range SeverityNames {
		if s == n {
			return i, nil
		}
	}
	if v, ok := severityAliases[s]; ok {
		return v, nil
	}
	return -1, errors.New(`severity must be one of emerg, alert, crit, err, warning, notice, info, debug`)
}

func severityName(sev int) string {
	if sev < 0 {
		return ""
	}
	return SeverityNames[sev]
}

// clip returns at most n bytes of b as valid UTF-8 and whether it cut b.
func clip(b []byte, n int) (string, bool) {
	cut := len(b) > n
	if cut {
		b = b[:n]
		// Drop a rune that the cut split, instead of turning it into U+FFFD.
		if i := lastRuneStart(b); i >= 0 && !utf8.FullRune(b[i:]) {
			b = b[:i]
		}
	}
	if utf8.Valid(b) {
		return string(b), cut
	}
	return string(bytes.ToValidUTF8(b, []byte("�"))), cut
}

// lastRuneStart returns the index of the last rune start byte within the
// last utf8.UTFMax bytes of b, or -1.
func lastRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return -1
}
