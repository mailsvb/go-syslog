# go-syslog

A small, fast UDP syslog collector for Linux written in Go.

- Listens on **UDP/514** (IPv4 and IPv6).
- Writes every message to `/var/log/go-syslog/<sender-ip>/<YYYY-MM-DD_HH>.log`. You get one file per sender and **UTC hour**.
- Each line is `<receive time, RFC 3339 UTC, ms> <raw message>`, for example
  `2026-11-18T13:04:05.123Z <134>1 2026-11-18T13:04:05Z router app - - - link up`.
  Control characters inside a message are escaped rsyslog-style (`#012` for a newline), so every message stays on one line.
- Deletes files older than **7 days** automatically.
- Ships as an RPM for linux/x86_64 through a zypper repository on GitHub Pages.

The receive and write path borrows the techniques that make rsyslog and syslog-ng fast and reliable:

- a large `SO_RCVBUF`
- `SO_REUSEPORT` reader sockets
- `recvmmsg` batching
- bounded queues that drop and count instead of blocking
- writer shards per sender
- buffered writes flushed on batch end and every 1 s
- an LRU open-file cache

See [docs/design.md](docs/design.md) for the analysis.

## Install (openSUSE / SLES)

```bash
zypper addrepo https://mailsvb.github.io/go-syslog/go-syslog.repo
zypper refresh
zypper install go-syslog
systemctl enable --now go-syslog
```

The package installs:

| Path | Purpose |
|---|---|
| `/usr/bin/go-syslog` | the daemon |
| `/usr/lib/systemd/system/go-syslog.service` | systemd unit; runs as user `gosyslog` with `CAP_NET_BIND_SERVICE` |
| `/etc/sysconfig/go-syslog` | `GO_SYSLOG_ARGS` for extra flags |
| `/usr/lib/sysctl.d/90-go-syslog.conf` | raises `net.core.rmem_max` to 32 MB |
| `/var/log/go-syslog/` | log directory |

If rsyslog or syslog-ng already listens on UDP/514 on the same host, disable that input or pick another port with `-listen`.

## Configuration

All settings are command line flags (`go-syslog -h`):

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:514` | UDP listen address |
| `-dir` | `/var/log/go-syslog` | base directory |
| `-retention` | `168h` | delete files older than this |
| `-cleanup-interval` | `10m` | how often retention runs |
| `-readers` | min(CPUs, 4) | `SO_REUSEPORT` sockets / reader threads |
| `-writers` | min(CPUs, 16) | writer shards (sender IPs are hashed to shards) |
| `-rcvbuf` | `33554432` | requested socket receive buffer |
| `-batch-size` | `64` | datagrams per `recvmmsg` |
| `-queue-size` | `65536` | in-memory queue (messages), split across writers |
| `-flush-interval` | `1s` | longest time data stays buffered |
| `-write-buffer` | `65536` | buffer per open file |
| `-max-open-files` | `1024` | open file cache per writer |
| `-idle-timeout` | `2m` | close files not written for this long |
| `-fsync-on-close` | `false` | fsync files on rotation/eviction/shutdown |
| `-stats-interval` | `1m` | log counters (0 = off) |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

Signals:

- `SIGHUP` (`systemctl reload go-syslog`): close and reopen all files.
- `SIGUSR1`: log the counters now.
- `SIGTERM`: drain the queues, flush and exit.

### Reading the stats

The service logs a `stats` line every minute with these counters:

- `received`
- `dropped_queue_full`
- `written`
- `bytes_written`
- `write_errors`
- `read_errors`
- `open_files`
- `files_deleted`

If `dropped_queue_full` grows, the disk can't keep up: raise `-queue-size` or `-writers`. Drops inside the kernel show up as `RcvbufErrors` in `netstat -su` (or `/proc/net/snmp`). When those grow, check that the log line `socket receive buffer` reports the requested size, and raise `-readers`.

## Development

Requires Go 1.27+. The target platform is linux/amd64. Tests also run on macOS.

```bash
make test                   # go test -race ./...
make lint                   # gofmt, go vet, staticcheck
make build                  # dist/go-syslog (linux/amd64, static)
make rpm VERSION=1.2.3      # dist/go-syslog-1.2.3-1.x86_64.rpm (needs nfpm)
```

Try it locally on an unprivileged port:

```bash
go run ./cmd/go-syslog -listen 127.0.0.1:5514 -dir ./out
```

```bash
logger -n 127.0.0.1 -P 5514 -d "hello"    # util-linux logger (Linux)
```

```bash
printf '<13>hello\n' | nc -u -w1 127.0.0.1 5514    # macOS / anywhere
```

## CI and releases

- **ci** (`.github/workflows/ci.yml`) runs on every push to `main` and on pull requests. It runs lint and race tests, builds the binary and an unsigned test RPM, installs that RPM in an openSUSE container, and uploads the binary and RPM as artifacts.
- **release** (`.github/workflows/release.yml`) runs manually from *Actions → release → Run workflow* with a version `X.Y.Z`. It tests and builds the RPM, adds it to the zypper repository on the `gh-pages` branch, keeps the **newest 5** RPMs, rebuilds the repository metadata, force-pushes a single commit and creates the GitHub release `vX.Y.Z`.

One-time setup:

1. *Settings → Pages*: source **GitHub Actions**. The release workflow deploys the repository with `actions/deploy-pages`; the `gh-pages` branch only stores the published RPMs for the next release.
2. Optional, for signed packages: add the repository secret `PACKAGE_SIGNING_KEY` (output of `gpg --armor --export-secret-keys <KEYID>`, or the same base64 encoded), plus `PACKAGE_SIGNING_PASSPHRASE` only if the key is password protected. These are the same secret names as in mailsvb/go-fs, so the same package key can be used in both. Without them the RPMs and repository metadata are unsigned, and the generated `go-syslog.repo` sets `gpgcheck=0`.
