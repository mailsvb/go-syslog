// Command go-syslog is a UDP syslog collector that writes one file per sender
// IP and UTC hour and deletes files older than the retention period.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mailsvb/go-syslog/internal/config"
	"github.com/mailsvb/go-syslog/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := config.Parse(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "go-syslog:", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Println("go-syslog", version)
		return 0
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log.Info("starting go-syslog", "version", version)

	srv, err := server.Start(context.Background(), cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		return 1
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1)
	for sig := range sigs {
		switch sig {
		case syscall.SIGHUP:
			log.Info("SIGHUP: reopening log files")
			srv.Reopen()
		case syscall.SIGUSR1:
			srv.Stats.Snapshot().Log(log, "stats")
		default:
			log.Info("shutting down", "signal", sig.String())
			signal.Stop(sigs)
			srv.Shutdown()
			return 0
		}
	}
	return 0
}
