# go-syslog design notes

These notes look at why rsyslog and syslog-ng are fast and reliable, and how each idea is used in go-syslog.

## Techniques used by rsyslog and syslog-ng

| Technique | rsyslog | syslog-ng | Why it matters |
|---|---|---|---|
| **Big kernel receive buffer** | `imudp rcvbufSize` (up to 1 GB, `SO_RCVBUFFORCE` as root) | `so_rcvbuf()` | UDP has no backpressure. The kernel socket buffer is the only thing that absorbs bursts while user space is busy. Default (~208 KB) overflows in milliseconds. Needs `net.core.rmem_max` raised. |
| **Batched syscalls on receive** | `recvmmsg()` with `batchSize` (default 32) | event loop reads up to `fetch_limit` messages per wakeup | Many datagrams per syscall means fewer context switches. |
| **Parallel receivers** | `imudp threads=N` + `SO_REUSEPORT` | several sources with `so_reuseport(yes)` | The kernel spreads datagrams over sockets by hashing the 4-tuple, so one sender always lands on the same socket and its message order is kept. |
| **The receive thread does almost nothing** | input thread only builds the msg object and enqueues it; it can run with high scheduling priority | the source only reads and parses | The socket must be drained faster than it fills. Disk I/O never happens on the receive path. |
| **Decoupling queue between input and output** | main queue + per-action queues (LinkedList/FixedArray), high/low/discard watermarks, optional disk-assisted spill | `log_fifo_size` per destination, optional disk-buffer | Slow disk or a slow destination never blocks receiving. Overload is handled on purpose (discard and count) instead of silent kernel drops. |
| **Batch processing / dequeue** | workers dequeue `dequeueBatchSize` messages at a time, one lock per batch | `flush_lines` | Locking and syscall cost is spread over many messages. |
| **Buffered file writes with timed flush** | `omfile ioBufferSize`, `asyncWriting` + `flushInterval` (1 s), `flushOnTXEnd` | `flush_lines` + `flush_timeout`, `writev()` | Each write() carries many KB instead of one line. Data reaches disk within about 1 s. No fsync per message: the page cache does the rest. |
| **Open-file cache for dynamic file names** | `dynaFileCacheSize` (LRU of fds), `closeTimeout` | file handle cache, `time_reap()` closes idle files | open/close per message would cost more than the write itself. Idle handles are reaped so fds don't leak. |
| **Cheap time and no per-message lookups** | `timeRequery` (clock read every N msgs), DNS cache or disabled DNS | cached time per poll iteration | Avoids syscalls and blocking DNS on the hot path. |
| **Memory discipline** | ref-counted msg objects, no copies between stages | ref-counted LogMessage, compact NV table | Less allocation and GC/heap churn under load. |
| **Observability and clean shutdown** | `impstats` counters, `queue.saveOnShutdown` | `syslog-ng-ctl stats` (processed/dropped/queued) | You can see drops, and nothing buffered is lost on a normal restart. |

### What go-syslog adopts
1. **`SO_RCVBUF` 32 MB** (configurable) on each socket, read back with `getsockopt`, and a warning logged if the kernel capped it. The RPM ships `/usr/lib/sysctl.d/90-go-syslog.conf` with `net.core.rmem_max=33554432`, so no `CAP_NET_ADMIN` is needed.
2. **N receiver goroutines (default = min(NumCPU,4)), each with its own `SO_REUSEPORT` socket**, each locked to an OS thread (`runtime.LockOSThread`).
3. **`recvmmsg` batching** (batch of 64, 64 KiB slots that are reused). On Linux the reader calls `recvmmsg` itself and reads the sender straight from the `sockaddr` into a `netip.Addr`; `x/net`'s `ReadBatch` allocated a `net.IP` and a `*net.UDPAddr` per datagram. Other systems (development machines) still use `ReadBatch`.
4. **Minimal hot path:** read the batch, take one `time.Now()` per batch (rsyslog `timeRequery` idea), copy each payload into a buffer from a `sync.Pool` (the pool stores the buffer's base pointer, so returning a buffer does not allocate), normalise the source IP (IPv4-mapped → IPv4), pick the writer shard by `fnv(ip) % W` and collect the message in a per-shard batch. After the `recvmmsg` call every shard gets its batch with **one** non-blocking channel send (rsyslog `dequeueBatchSize`, the other way round), so channel locking and goroutine wake-ups are paid per batch, not per message. Receiving and dispatching do not allocate.
5. **Bounded queues with drop-and-count:** each writer shard has a queue (default 65 536 msgs in total). The limit is counted in messages: a reader reserves room before it sends a batch, and a batch that does not fit is dropped as a whole and counted in `dropped_queue_full`, like rsyslog's discard watermark. The reader never blocks, so overload is visible in the counters instead of hidden as kernel `RcvbufErrors`. The disk-assisted queue is out of scope and noted as a possible later addition.
6. **Sharded writers (W goroutines, default NumCPU) owned by IP hash:** each IP has exactly one writer, so per-host order is kept and file handles need no locks.
7. **Per-writer LRU file cache** (default 1024 open files per shard): `*os.File` + `bufio.Writer` (64 KiB). The shard remembers the last written file, so a run of messages for the same file skips the map lookup. Written-message counters are kept per shard and added to the shared atomics once per batch, so the shards do not contend on one cache line. Flush happens when:
   - the buffer is full (automatic)
   - the shard's queue has been drained (rsyslog `flushOnTXEnd`), at most every `-flush-min-interval` (100 ms). Without the limit, moderate traffic drains the queue after nearly every message and costs one `write(2)` per message; with it, a skipped flush is done by a timer when the interval is up. In a loopback test with 64 senders at 20 000 msgs/s, file writes dropped from 1 per message to 0.04 per message.
   - a 1 s ticker fires (`flushInterval` / `flush_timeout`)
   - the file rotates, is evicted, the process shuts down, or SIGHUP arrives.
   There is no fsync per message. The config offers optional `fsync on close/rotate`.
8. **Hourly rotation by receive-time UTC hour:** the key is `(ip, hour)`. A new hour opens a new file. Files from the previous hour are closed by the reaper once idle for more than 2 min (syslog-ng `time_reap`).
9. **Retention janitor:** runs at start and then every 10 min. It parses the hour from the file name (more reliable than mtime), deletes files older than 7 days (configurable), and removes empty IP directories. It never touches files that are currently open.
10. **Stats:** atomic counters (received, written, bytes, dropped_queue_full, write_errors, open_files, files_deleted). They are logged every 60 s to the journal and dumped on SIGUSR1.
11. **Graceful shutdown (SIGTERM):** close the sockets, drain the shard channels, flush and close all files, then exit. This is the in-memory version of `saveOnShutdown`.

## Splitting senders by tag (Docker)

A Docker host sends the logs of all its containers from one IP. The only thing that tells them apart is the syslog TAG (RFC 3164) or APP-NAME (RFC 5424). For the senders listed in `-split-by-tag`, the file key becomes `(ip, tag, hour)` and the files go to `<ip>/<tag>/<hour>.log`. The design keeps the hot path allocation-free:

- **Where:** `Dispatcher.Dispatch` on the reader thread. The tag has to be known before the shard is picked. Otherwise all containers of a busy host would share one writer goroutine.
- **Matching the sender:** if the flag is not set, a single length check skips everything. Otherwise a linear `netip.Prefix.Contains` runs over the few configured prefixes.
- **Parsing:** `parseTag` reads only the header (at most 512 bytes) and returns offsets into the datagram. It doesn't allocate or copy. It handles RFC 5424, and RFC 3164 with or without HOSTNAME. Together these cover all Docker `syslog-format` values. Cost: 12–18 ns.
- **Sharding:** `fnv(ip)` continues over the *sanitized* tag bytes. Raw tags that map to the same directory therefore land on the same shard, so one file is only ever written by one goroutine and order within each file is kept.
- **Writer:** each shard keeps a map from raw tag to sanitized directory name. Lookups through `m[string(b)]` do not allocate. A new tag is sanitized and allocated once. The map is cleared when it reaches 4096 entries, which bounds memory when container names churn.
- **Safety:** the tag comes from the network and becomes a path component. Only `[A-Za-z0-9._-]` is kept, names are cut to 64 bytes, and `""`, `.`, `..` and `-` map to `_untagged`.
- **Retention:** the janitor also cleans `<ip>/<tag>/` and removes tag directories that became empty.

Measured on an Apple M-series machine:
- Dispatch for a split sender costs about 40 ns more than for a normal one.
- A configured but non-matching flag costs about 8 ns.
- Both paths still make 0 allocations per message.

## Data flow

```
             SO_REUSEPORT sockets (32 MB SO_RCVBUF each)
 UDP/514 ──► reader 0 ─┐   recvmmsg batch of 64, one clock read per batch
             reader 1 ─┤
             reader N ─┘
                 │  Message{ip, time, pooled []byte}
                 ▼  fnv(ip) % W, or fnv(ip, tag) % W for -split-by-tag senders
                 │  one batch per shard per recvmmsg call (non-blocking; full queue → dropped_queue_full += len)
          ┌─────────────┬─────────────┐
          │ shard 0 chan│ shard W chan│   bounded in-memory queues
          ▼             ▼             ▼
       writer 0  ...  writer W       one goroutine per shard, owns its files
          │  LRU cache: (ip, [tag,] UTC hour) → *os.File + 64 KiB bufio.Writer
          │  flush: buffer full | queue drained (max every 100 ms) | 1 s tick | rotate/evict/HUP/exit
          ▼
   <dir>/<ip>/[<tag>/]<YYYY-MM-DD_HH>.log     "2026-11-18T13:04:05.123Z <raw message>"

   janitor (every 10 min): delete hour files older than the retention period, remove empty ip dirs
```

## Measuring

`BenchmarkEndToEnd` (`internal/server/bench_test.go`) sends datagrams over loopback through the whole server (sockets, readers, writer shards, files) and keeps at most 8192 in flight, so it measures how fast the server drains and writes, not how many datagrams the kernel drops. Run it on Linux:

```bash
go test -run x -bench EndToEnd -benchmem -cpuprofile cpu.out ./internal/server
```

On loopback the kernel's delivery work is billed to the senders, so ns/op mostly measures the sending side. The useful number is the server's share of the CPU profile (`receiver.(*Receiver).readLoop` + `writer.(*shard).run`) divided by the message count.

Server CPU per message, Linux arm64 VM with 2 vCPUs, 2M messages of 112 bytes, mean of 3 runs:

| | 1 sender IP | 64 sender IPs | allocations/msg |
|---|---|---|---|
| before the hot-path work | 440 ns | 410 ns | 2 |
| writer quick wins | 430 ns | 395 ns | 2 |
| own `recvmmsg` | 325 ns | 370 ns | 0 |
| batched hand-off | 310 ns | 345 ns | 0 |
| `-flush-min-interval 100ms` | 265 ns | 305 ns | 0 |

About two thirds of the remaining reader time is the `recvmmsg` system call itself (the kernel copying the datagrams); the user-space part of the reader is about 60 ns per message. A VM with 2 vCPUs understates the gain of batched hand-off, which saves goroutine wake-ups and channel lock contention that grow with the number of readers and writers.

## Possible later additions

- Count kernel drops per socket with `SO_RXQ_OVFL` and report them in the stats.
- Steer flows to readers on the CPU that received them (`SO_INCOMING_CPU`, `SO_ATTACH_REUSEPORT_CBPF`).
- A disk-assisted queue (rsyslog DA queue / syslog-ng disk-buffer) to absorb overloads that last a long time.
- TCP/TLS input (RFC 6587 / RFC 5425) with real backpressure.
