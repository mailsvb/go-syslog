# go-syslog

A small, fast UDP syslog collector for Linux written in Go.

- Listens on **UDP/514** (IPv4 and IPv6).
- Writes every message to `/var/log/go-syslog/<sender-ip>/<YYYY-MM-DD_HH>.log`. You get one file per sender and **UTC hour**.
- Can split selected senders (for example Docker hosts) by syslog tag into `/var/log/go-syslog/<sender-ip>/<tag>/<YYYY-MM-DD_HH>.log`, giving one directory per container. See [Docker containers](#docker-containers).
- Each line is `<receive time, RFC 3339 UTC, ms> <raw message>`, for example
  `2026-11-18T13:04:05.123Z <134>1 2026-11-18T13:04:05Z router app - - - link up`.
  Control characters inside a message are escaped rsyslog-style (`#012` for a newline), so every message stays on one line.
- Deletes files older than **7 days** automatically.
- Ships as an RPM for linux/x86_64 through a zypper repository on GitHub Pages.
- Optional second package **go-syslog-view**: a read-only MCP server over HTTP, so AI agents can list, count and search the collected logs. See [MCP server for AI agents](#mcp-server-for-ai-agents-go-syslog-view).

The receive and write path borrows the techniques that make rsyslog and syslog-ng fast and reliable:

- a large `SO_RCVBUF`
- `SO_REUSEPORT` reader sockets
- `recvmmsg` batching
- bounded queues that drop and count instead of blocking
- writer shards per sender
- buffered writes flushed on batch end (at most every 100 ms) and every 1 s
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
| `-flush-min-interval` | `100ms` | min time between flushes when a writer's queue runs empty (0 = flush every time) |
| `-write-buffer` | `65536` | buffer per open file |
| `-max-open-files` | `1024` | open file cache per writer |
| `-idle-timeout` | `2m` | close files not written for this long |
| `-fsync-on-close` | `false` | fsync files on rotation/eviction/shutdown |
| `-split-by-tag` | (off) | comma-separated IPs/CIDRs whose files are split by syslog tag into `<ip>/<tag>/` |
| `-stats-interval` | `1m` | log counters (0 = off) |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

Signals:

- `SIGHUP` (`systemctl reload go-syslog`): close and reopen all files.
- `SIGUSR1`: log the counters now.
- `SIGTERM`: drain the queues, flush and exit.

### Docker containers

Docker's `syslog` log driver sends from the Docker daemon, not from the containers. Every container on a host therefore arrives with the same source IP: `127.0.0.1` when go-syslog runs on the same host, otherwise the Docker host's address. The container is identified only by the syslog tag.

List those hosts in `-split-by-tag` to get one directory per container:

```bash
GO_SYSLOG_ARGS="-split-by-tag 127.0.0.1,10.0.0.20"    # /etc/sysconfig/go-syslog
```

Then configure the log driver per container:

```bash
docker run --log-driver syslog \
  --log-opt syslog-address=udp://127.0.0.1:514 \
  --log-opt syslog-format=rfc5424 \
  --log-opt tag='{{.Name}}' ...
```

Or configure it for all containers in `/etc/docker/daemon.json`. Restart dockerd afterwards; the setting applies to new containers only.

```json
{
  "log-driver": "syslog",
  "log-opts": {
    "syslog-address": "udp://127.0.0.1:514",
    "syslog-format": "rfc5424",
    "tag": "{{.Name}}"
  }
}
```

This writes `/var/log/go-syslog/127.0.0.1/<container-name>/<YYYY-MM-DD_HH>.log`.

- **Tag:** use `{{.Name}}`. Docker's default tag is the container ID, which changes every time the container is recreated.
- **Format:** `rfc5424` is the most robust. The default `unix` format and `rfc3164` also work.
- **Directory names:** characters other than letters, digits, `.`, `_` and `-` become `_`, so `{{.ImageName}}/{{.Name}}` becomes `nginx_web-1`. Names are cut at 64 bytes.
- **No tag:** messages without a usable tag go to `_untagged/`.
- **Other programs:** these rules apply to every message from a listed sender. If the same host also forwards its own system log, those messages are split by program name (`sshd/`, `cron/`, ...).

The retention cleanup covers the tag directories as well.

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

## MCP server for AI agents (go-syslog-view)

`go-syslog-view` is a separate program that reads the files in `/var/log/go-syslog` and serves them to AI agents as an [MCP](https://modelcontextprotocol.io) server over plain HTTP (streamable HTTP, stateless, JSON responses). It never writes to the log directory and does not touch the collector, so a slow query or a bug in it cannot cost the collector a message. It is built and released together with go-syslog, with the same version.

```bash
zypper install go-syslog-view
systemctl enable --now go-syslog-view
cat /etc/go-syslog-view/token      # bearer token, created on first install
```

The endpoint is `http://<host>:8514/mcp`. Clients must send `Authorization: Bearer <token>`. There is no TLS, so keep the port inside your LAN or VPN.

| Path | Purpose |
|---|---|
| `/usr/bin/go-syslog-view` | the server |
| `/usr/lib/systemd/system/go-syslog-view.service` | systemd unit; runs as user `gosyslog-view` with the supplementary group `gosyslog`, which gives read access to the logs |
| `/etc/sysconfig/go-syslog-view` | `GO_SYSLOG_VIEW_ARGS`, default `-token-file /etc/go-syslog-view/token` |
| `/etc/go-syslog-view/token` | random token (mode 0640, root:gosyslog-view); replace it with your own if you like and restart the service |

Flags (`go-syslog-view -h`):

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:8514` | HTTP listen address |
| `-dir` | `/var/log/go-syslog` | log directory of go-syslog |
| `-token-file` | (none) | file with the bearer token; without it there is no authentication and a warning is logged |
| `-max-scan-bytes` | `1073741824` | bytes one request may read before it returns a partial result with a cursor |
| `-max-scan-time` | `20s` | time one request may scan before it returns a partial result with a cursor |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

Tools (all read-only):

| Tool | What it does |
|---|---|
| `list_sources` | senders (`<ip>`) and tags of split senders (`<ip>/<tag>`) with file count, size, oldest hour and last write; also the server time |
| `log_stats` | lines per source (or source and hour) in a time range, per severity, first/last time and the most frequent apps (syslog tags) |
| `search_logs` | lines by source, time range, minimum severity, case-insensitive text and/or RE2 regex; newest first by default, paged with `next_cursor`, limited by `limit` and `max_bytes` |
| `get_context` | the lines before and after a line returned by `search_logs` (by its `ref`) |

Times accept RFC 3339 or a duration before now such as `30m`, `6h` or `2d`; the default range is the last 24 hours. Results are kept small on purpose (50 lines and about 32 KB per page by default, messages clipped at 1000 bytes), so an agent starts with `log_stats` and narrows down instead of reading whole files.

Connect Claude Code (or any MCP client that speaks streamable HTTP):

```bash
claude mcp add --transport http go-syslog http://loghost:8514/mcp \
  --header "Authorization: Bearer $(ssh loghost sudo cat /etc/go-syslog-view/token)"
```

Try it with curl (stateless, so no initialize call is needed):

```bash
curl -s http://loghost:8514/mcp -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"log_stats","arguments":{"from":"6h"}}}'
```

`GET /healthz` returns `ok` without authentication.

## Development

Requires Go 1.27+. The target platform is linux/amd64. Tests also run on macOS.

```bash
make test                   # go test -race ./...
make lint                   # gofmt, go vet, staticcheck
make build                  # dist/go-syslog and dist/go-syslog-view (linux/amd64, static, same version)
make rpm VERSION=1.2.3      # dist/go-syslog-1.2.3-1.x86_64.rpm and dist/go-syslog-view-1.2.3-1.x86_64.rpm (needs nfpm)
```

Try it locally on an unprivileged port:

```bash
go run ./cmd/go-syslog -listen 127.0.0.1:5514 -dir ./out
```

```bash
go run ./cmd/go-syslog-view -listen 127.0.0.1:8514 -dir ./out    # MCP at http://127.0.0.1:8514/mcp
```

```bash
logger -n 127.0.0.1 -P 5514 -d "hello"    # util-linux logger (Linux)
```

```bash
printf '<13>hello\n' | nc -u -w1 127.0.0.1 5514    # macOS / anywhere
```

## CI and releases

- **ci** (`.github/workflows/ci.yml`) runs on every push to `main` and on pull requests. It runs lint and race tests, builds both binaries and unsigned test RPMs (go-syslog and go-syslog-view), installs them in an openSUSE container, and uploads the binaries and RPMs as artifacts.
- **release** (`.github/workflows/release.yml`) runs manually from *Actions → release → Run workflow* with a version `X.Y.Z`. It tests and builds both RPMs with that version, adds them to the zypper repository on the `gh-pages` branch, keeps the **newest 5** RPMs of each package, rebuilds the repository metadata, force-pushes a single commit and creates the GitHub release `vX.Y.Z`.

One-time setup:

1. *Settings → Pages*: source **GitHub Actions**. The release workflow deploys the repository with `actions/deploy-pages`; the `gh-pages` branch only stores the published RPMs for the next release.
2. Optional, for signed packages: add the repository secret `PACKAGE_SIGNING_KEY` (output of `gpg --armor --export-secret-keys <KEYID>`, or the same base64 encoded), plus `PACKAGE_SIGNING_PASSPHRASE` only if the key is password protected. These are the same secret names as in mailsvb/go-fs, so the same package key can be used in both. Without them the RPMs and repository metadata are unsigned, and the generated `go-syslog.repo` sets `gpgcheck=0`.
