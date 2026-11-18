// Package stats provides lock-free counters similar to rsyslog impstats.
package stats

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Stats holds process wide counters. All fields are safe for concurrent use.
type Stats struct {
	Received         atomic.Uint64 // datagrams read from the sockets
	DroppedQueueFull atomic.Uint64 // datagrams dropped because a writer queue was full
	Written          atomic.Uint64 // messages handed to a file buffer
	BytesWritten     atomic.Uint64 // bytes handed to a file buffer
	WriteErrors      atomic.Uint64 // failed opens/writes/flushes
	ReadErrors       atomic.Uint64 // socket read errors
	FilesOpened      atomic.Uint64
	FilesClosed      atomic.Uint64
	FilesDeleted     atomic.Uint64 // removed by the retention janitor
}

// Snapshot is a point-in-time copy of Stats.
type Snapshot struct {
	Received, DroppedQueueFull, Written, BytesWritten, WriteErrors, ReadErrors uint64
	FilesOpened, FilesClosed, FilesDeleted                                     uint64
}

// Snapshot returns the current counter values.
func (s *Stats) Snapshot() Snapshot {
	return Snapshot{
		Received:         s.Received.Load(),
		DroppedQueueFull: s.DroppedQueueFull.Load(),
		Written:          s.Written.Load(),
		BytesWritten:     s.BytesWritten.Load(),
		WriteErrors:      s.WriteErrors.Load(),
		ReadErrors:       s.ReadErrors.Load(),
		FilesOpened:      s.FilesOpened.Load(),
		FilesClosed:      s.FilesClosed.Load(),
		FilesDeleted:     s.FilesDeleted.Load(),
	}
}

// Log writes the snapshot as one structured log line.
func (s Snapshot) Log(l *slog.Logger, msg string) {
	l.Info(msg,
		"received", s.Received,
		"dropped_queue_full", s.DroppedQueueFull,
		"written", s.Written,
		"bytes_written", s.BytesWritten,
		"write_errors", s.WriteErrors,
		"read_errors", s.ReadErrors,
		"open_files", s.FilesOpened-s.FilesClosed,
		"files_deleted", s.FilesDeleted,
	)
}

// RunLogger logs the counters every interval until ctx is done.
func (s *Stats) RunLogger(ctx context.Context, l *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Snapshot().Log(l, "stats")
		}
	}
}
