package writer

import (
	"bufio"
	"container/list"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

const (
	// HourLayout is the time layout of the log file names (UTC).
	HourLayout = "2006-01-02_15"
	// FileExt is the extension of the log files.
	FileExt = ".log"
)

// fileKey identifies one hourly file of one sender (and tag).
type fileKey struct {
	addr netip.Addr
	tag  string // sanitized tag for split senders, "" otherwise
	hour int64  // hours since the Unix epoch, UTC
}

type openFile struct {
	key       fileKey
	path      string
	f         *os.File
	w         *bufio.Writer
	lastWrite time.Time
	dirty     bool
	elem      *list.Element
}

// hourOf returns the number of whole hours since the Unix epoch.
func hourOf(t time.Time) int64 {
	s := t.Unix()
	h := s / 3600
	if s < 0 && s%3600 != 0 {
		h--
	}
	return h
}

// NormalizeAddr turns IPv4-mapped IPv6 addresses into plain IPv4 and strips
// IPv6 zones so the same sender always maps to the same directory.
func NormalizeAddr(a netip.Addr) netip.Addr {
	return a.Unmap().WithZone("")
}

// FilePath returns <dir>/<ip>/<YYYY-MM-DD_HH>.log for the given hour, or
// <dir>/<ip>/<tag>/<YYYY-MM-DD_HH>.log if tag is not empty. tag must already
// be sanitized.
func FilePath(dir string, addr netip.Addr, tag string, t time.Time) string {
	name := t.UTC().Format(HourLayout) + FileExt
	if tag == "" {
		return filepath.Join(dir, addr.String(), name)
	}
	return filepath.Join(dir, addr.String(), tag, name)
}
