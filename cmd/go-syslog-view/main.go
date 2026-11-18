// Command go-syslog-view gives read-only access to the files written by
// go-syslog. It serves an MCP server over plain HTTP for AI agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mailsvb/go-syslog/internal/mcpserver"
	"github.com/mailsvb/go-syslog/internal/query"
)

// version is set at build time with -ldflags "-X main.version=..."; the
// Makefile builds go-syslog and go-syslog-view with the same version.
var version = "dev"

type config struct {
	Listen       string
	Dir          string
	TokenFile    string
	MaxScanBytes int64
	MaxScanTime  time.Duration
	LogLevel     string
	ShowVersion  bool
}

func parseConfig(args []string, out io.Writer) (config, error) {
	c := config{
		Listen:       ":8514",
		Dir:          "/var/log/go-syslog",
		MaxScanBytes: query.DefaultLimits.MaxScanBytes,
		MaxScanTime:  query.DefaultLimits.MaxScanTime,
		LogLevel:     "info",
	}
	fs := flag.NewFlagSet("go-syslog-view", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&c.Listen, "listen", c.Listen, "HTTP listen address; the MCP endpoint is "+mcpserver.Path)
	fs.StringVar(&c.Dir, "dir", c.Dir, "log directory of go-syslog (read only)")
	fs.StringVar(&c.TokenFile, "token-file", c.TokenFile, "file with a bearer token that clients must send (empty: no authentication)")
	fs.Int64Var(&c.MaxScanBytes, "max-scan-bytes", c.MaxScanBytes, "bytes a single request may read before it returns a partial result")
	fs.DurationVar(&c.MaxScanTime, "max-scan-time", c.MaxScanTime, "time a single request may scan before it returns a partial result")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "log level: debug, info, warn, error")
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen must not be empty"))
	}
	if c.Dir == "" {
		errs = append(errs, errors.New("dir must not be empty"))
	}
	if c.MaxScanBytes <= 0 || c.MaxScanTime <= 0 {
		errs = append(errs, errors.New("max-scan-bytes and max-scan-time must be > 0"))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("unknown log-level %q", c.LogLevel))
	}
	return c, errors.Join(errs...)
}

// readToken reads the bearer token from path. Surrounding whitespace is
// removed; an empty token is an error.
func readToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return "", fmt.Errorf("%s: token must be one non-empty word", path)
	}
	return tok, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := parseConfig(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "go-syslog-view:", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Println("go-syslog-view", version)
		return 0
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log.Info("starting go-syslog-view", "version", version)

	token, err := readToken(cfg.TokenFile)
	if err != nil {
		log.Error("reading token failed", "err", err)
		return 1
	}
	store, err := query.Open(cfg.Dir, query.Limits{MaxScanBytes: cfg.MaxScanBytes, MaxScanTime: cfg.MaxScanTime})
	if err != nil {
		log.Error("opening log directory failed", "err", err)
		return 1
	}
	defer store.Close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen failed", "err", err)
		return 1
	}
	if token == "" {
		log.Warn("no -token-file set: anyone who can reach the port can read all logs")
	}
	log.Info("serving MCP over HTTP", "addr", ln.Addr().String(), "path", mcpserver.Path, "dir", cfg.Dir, "auth", token != "")

	srv := &http.Server{
		Handler:           mcpserver.Handler(mcpserver.New(store, version, log), token, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.MaxScanTime + 30*time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	select {
	case err := <-errc:
		log.Error("server failed", "err", err)
		return 1
	case sig := <-sigs:
		log.Info("shutting down", "signal", sig.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown failed", "err", err)
		return 1
	}
	return 0
}
