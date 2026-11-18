// Package config holds the runtime configuration of go-syslog.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"time"
)

// Config is the complete runtime configuration.
type Config struct {
	Listen          string        // UDP listen address, e.g. ":514"
	Dir             string        // base directory for <ip>/<hour>.log files
	Retention       time.Duration // files older than this are deleted
	CleanupInterval time.Duration // how often the retention janitor runs
	Readers         int           // SO_REUSEPORT sockets / receiver goroutines
	Writers         int           // writer shards
	RcvBuf          int           // requested SO_RCVBUF in bytes
	BatchSize       int           // datagrams per recvmmsg call
	QueueSize       int           // total in-memory queue capacity (messages), split across writers
	FlushInterval   time.Duration // max time buffered data stays in user space
	WriteBufSize    int           // bufio size per open file
	MaxOpenFiles    int           // open file cache size per writer shard
	IdleTimeout     time.Duration // close files not written for this long
	FsyncOnClose    bool          // fsync files when they are closed
	StatsInterval   time.Duration // periodic stats logging, 0 disables
	LogLevel        string        // debug, info, warn, error
	ShowVersion     bool
}

// Default returns the default configuration.
func Default() Config {
	return Config{
		Listen:          ":514",
		Dir:             "/var/log/go-syslog",
		Retention:       7 * 24 * time.Hour,
		CleanupInterval: 10 * time.Minute,
		Readers:         min(runtime.NumCPU(), 4),
		Writers:         min(runtime.NumCPU(), 16),
		RcvBuf:          32 << 20,
		BatchSize:       64,
		QueueSize:       65536,
		FlushInterval:   time.Second,
		WriteBufSize:    64 << 10,
		MaxOpenFiles:    1024,
		IdleTimeout:     2 * time.Minute,
		StatsInterval:   time.Minute,
		LogLevel:        "info",
	}
}

// Parse parses command line arguments (without the program name).
func Parse(args []string, out io.Writer) (Config, error) {
	c := Default()
	fs := flag.NewFlagSet("go-syslog", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&c.Listen, "listen", c.Listen, "UDP listen address")
	fs.StringVar(&c.Dir, "dir", c.Dir, "base directory for log files (<dir>/<ip>/<YYYY-MM-DD_HH>.log)")
	fs.DurationVar(&c.Retention, "retention", c.Retention, "delete log files older than this")
	fs.DurationVar(&c.CleanupInterval, "cleanup-interval", c.CleanupInterval, "how often to run the retention cleanup")
	fs.IntVar(&c.Readers, "readers", c.Readers, "number of receiving sockets (SO_REUSEPORT)")
	fs.IntVar(&c.Writers, "writers", c.Writers, "number of writer shards")
	fs.IntVar(&c.RcvBuf, "rcvbuf", c.RcvBuf, "requested socket receive buffer in bytes (capped by net.core.rmem_max)")
	fs.IntVar(&c.BatchSize, "batch-size", c.BatchSize, "datagrams read per recvmmsg call")
	fs.IntVar(&c.QueueSize, "queue-size", c.QueueSize, "total in-memory queue capacity in messages")
	fs.DurationVar(&c.FlushInterval, "flush-interval", c.FlushInterval, "max time data stays buffered before it is written")
	fs.IntVar(&c.WriteBufSize, "write-buffer", c.WriteBufSize, "write buffer size per open file in bytes")
	fs.IntVar(&c.MaxOpenFiles, "max-open-files", c.MaxOpenFiles, "open file cache size per writer shard")
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", c.IdleTimeout, "close files that were not written for this long")
	fs.BoolVar(&c.FsyncOnClose, "fsync-on-close", c.FsyncOnClose, "fsync files when they are closed (rotation, eviction, shutdown)")
	fs.DurationVar(&c.StatsInterval, "stats-interval", c.StatsInterval, "log statistics at this interval (0 disables)")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "log level: debug, info, warn, error")
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return c, c.Validate()
}

// Validate checks the configuration for invalid values.
func (c Config) Validate() error {
	var errs []error
	positive := func(name string, v int) {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be > 0", name))
		}
	}
	positiveDur := func(name string, v time.Duration) {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("%s must be > 0", name))
		}
	}
	if c.Listen == "" {
		errs = append(errs, errors.New("listen must not be empty"))
	}
	if c.Dir == "" {
		errs = append(errs, errors.New("dir must not be empty"))
	}
	positive("readers", c.Readers)
	positive("writers", c.Writers)
	positive("rcvbuf", c.RcvBuf)
	positive("batch-size", c.BatchSize)
	positive("queue-size", c.QueueSize)
	positive("write-buffer", c.WriteBufSize)
	positive("max-open-files", c.MaxOpenFiles)
	positiveDur("retention", c.Retention)
	positiveDur("cleanup-interval", c.CleanupInterval)
	positiveDur("flush-interval", c.FlushInterval)
	positiveDur("idle-timeout", c.IdleTimeout)
	if c.StatsInterval < 0 {
		errs = append(errs, errors.New("stats-interval must be >= 0"))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("unknown log-level %q", c.LogLevel))
	}
	return errors.Join(errs...)
}

// QueuePerWriter returns the channel capacity of a single writer shard.
func (c Config) QueuePerWriter() int {
	return max(c.QueueSize/c.Writers, 1024)
}
