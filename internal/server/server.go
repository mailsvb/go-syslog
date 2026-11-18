// Package server wires receiver, writer, retention janitor and stats together.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/mailsvb/go-syslog/internal/config"
	"github.com/mailsvb/go-syslog/internal/receiver"
	"github.com/mailsvb/go-syslog/internal/retention"
	"github.com/mailsvb/go-syslog/internal/stats"
	"github.com/mailsvb/go-syslog/internal/writer"
)

// Server is a running go-syslog instance.
type Server struct {
	cfg    config.Config
	log    *slog.Logger
	Stats  *stats.Stats
	disp   *writer.Dispatcher
	recv   *receiver.Receiver
	cancel context.CancelFunc
	bg     sync.WaitGroup
}

// Start opens the sockets and starts all goroutines.
func Start(ctx context.Context, cfg config.Config, log *slog.Logger) (*Server, error) {
	st := &stats.Stats{}
	disp := writer.NewDispatcher(writer.Options{
		Dir:              cfg.Dir,
		Shards:           cfg.Writers,
		QueuePerShard:    cfg.QueuePerWriter(),
		WriteBufSize:     cfg.WriteBufSize,
		MaxOpenFiles:     cfg.MaxOpenFiles,
		FlushInterval:    cfg.FlushInterval,
		FlushMinInterval: cfg.FlushMinInterval,
		IdleTimeout:      cfg.IdleTimeout,
		FsyncOnClose:     cfg.FsyncOnClose,
		SplitByTag:       cfg.SplitByTag,
		Stats:            st,
		Logger:           log,
	})
	recv, err := receiver.Listen(ctx, receiver.Options{
		Listen:    cfg.Listen,
		Readers:   cfg.Readers,
		RcvBuf:    cfg.RcvBuf,
		BatchSize: cfg.BatchSize,
		NewSink:   func() receiver.Sink { return disp.NewBatcher() },
		Stats:     st,
		Logger:    log,
	})
	if err != nil {
		disp.Close()
		return nil, err
	}

	bgCtx, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, log: log, Stats: st, disp: disp, recv: recv, cancel: cancel}
	j := &retention.Janitor{
		Dir:       cfg.Dir,
		Retention: cfg.Retention,
		Interval:  cfg.CleanupInterval,
		Stats:     st,
		Logger:    log,
	}
	s.bg.Add(2)
	go func() { defer s.bg.Done(); j.Run(bgCtx) }()
	go func() { defer s.bg.Done(); st.RunLogger(bgCtx, log, cfg.StatsInterval) }()

	recv.Start()
	log.Info("listening", "addr", recv.Addr().String(), "readers", cfg.Readers,
		"writers", cfg.Writers, "dir", cfg.Dir, "retention", cfg.Retention.String(),
		"split_by_tag", fmt.Sprint(cfg.SplitByTag))
	return s, nil
}

// Addr returns the bound UDP address.
func (s *Server) Addr() net.Addr { return s.recv.Addr() }

// Reopen closes all open log files; they are reopened on the next message.
func (s *Server) Reopen() { s.disp.Reopen() }

// Shutdown stops receiving, writes everything still queued and closes all files.
func (s *Server) Shutdown() {
	s.recv.Close()
	s.disp.Close()
	s.cancel()
	s.bg.Wait()
	s.Stats.Snapshot().Log(s.log, "final stats")
}
