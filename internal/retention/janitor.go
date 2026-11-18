// Package retention deletes log files older than the retention period.
package retention

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mailsvb/go-syslog/internal/stats"
	"github.com/mailsvb/go-syslog/internal/writer"
)

// Janitor removes <dir>/<ip>/<YYYY-MM-DD_HH>.log files whose hour ended more
// than Retention ago and removes sender directories that became empty.
// The age comes from the file name, not from mtime, so touching or copying
// files does not change when they expire.
type Janitor struct {
	Dir       string
	Retention time.Duration
	Interval  time.Duration
	Now       func() time.Time
	Stats     *stats.Stats
	Logger    *slog.Logger
}

// Run cleans up once immediately and then every Interval until ctx is done.
func (j *Janitor) Run(ctx context.Context) {
	j.runOnceLogged()
	t := time.NewTicker(j.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.runOnceLogged()
		}
	}
}

func (j *Janitor) runOnceLogged() {
	n, err := j.RunOnce()
	if err != nil {
		j.logger().Error("retention cleanup failed", "dir", j.Dir, "err", err)
	}
	if n > 0 {
		j.logger().Info("retention cleanup", "deleted_files", n)
	}
}

func (j *Janitor) logger() *slog.Logger {
	if j.Logger == nil {
		return slog.Default()
	}
	return j.Logger
}

// RunOnce performs one cleanup pass and returns the number of deleted files.
func (j *Janitor) RunOnce() (int, error) {
	now := time.Now
	if j.Now != nil {
		now = j.Now
	}
	cutoff := now().Add(-j.Retention)

	hosts, err := os.ReadDir(j.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	deleted := 0
	var errs []error
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		hostDir := filepath.Join(j.Dir, h.Name())
		files, err := os.ReadDir(hostDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		remaining := len(files)
		for _, f := range files {
			if !Expired(f.Name(), cutoff) || f.IsDir() {
				continue
			}
			if err := os.Remove(filepath.Join(hostDir, f.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			remaining--
			deleted++
			if j.Stats != nil {
				j.Stats.FilesDeleted.Add(1)
			}
		}
		if remaining == 0 {
			// Fails harmlessly if a writer created a new file in the meantime.
			_ = os.Remove(hostDir)
		}
	}
	return deleted, errors.Join(errs...)
}

// Expired reports whether name is a log file whose hour ended before cutoff.
// Names that do not match the log file pattern are never expired.
func Expired(name string, cutoff time.Time) bool {
	base, ok := strings.CutSuffix(name, writer.FileExt)
	if !ok {
		return false
	}
	start, err := time.ParseInLocation(writer.HourLayout, base, time.UTC)
	if err != nil {
		return false
	}
	return !start.Add(time.Hour).After(cutoff)
}
