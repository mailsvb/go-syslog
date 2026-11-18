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
3. **`recvmmsg` batching** via `golang.org/x/net/ipv4|ipv6.PacketConn.ReadBatch` (batch of 64, 64 KiB slots that are reused).
4. **Minimal hot path:** read the batch, take one `time.Now()` per batch (rsyslog `timeRequery` idea), copy each payload into a buffer from a `sync.Pool`, normalise the source IP (IPv4-mapped → IPv4), then a non-blocking send to the writer shard picked by `fnv(ip) % W`.
5. **Bounded queues with drop-and-count:** each writer shard has a buffered channel (default 65 536 msgs). When it is full the message is dropped and the `dropped_queue_full` counter goes up, like rsyslog's discard watermark. The reader never blocks, so overload is visible in the counters instead of hidden as kernel `RcvbufErrors`. The disk-assisted queue is out of scope and noted as a possible later addition.
6. **Sharded writers (W goroutines, default NumCPU) owned by IP hash:** each IP has exactly one writer, so per-host order is kept and file handles need no locks.
7. **Per-writer LRU file cache** (default 1024 open files per shard): `*os.File` + `bufio.Writer` (64 KiB). Flush happens when:
   - the buffer is full (automatic)
   - the shard's queue has been drained (rsyslog `flushOnTXEnd`)
   - a 1 s ticker fires (`flushInterval` / `flush_timeout`)
   - the file rotates, is evicted, the process shuts down, or SIGHUP arrives.
   There is no fsync per message. The config offers optional `fsync on close/rotate`.
8. **Hourly rotation by receive-time UTC hour:** the key is `(ip, hour)`. A new hour opens a new file. Files from the previous hour are closed by the reaper once idle for more than 2 min (syslog-ng `time_reap`).
9. **Retention janitor:** runs at start and then every 10 min. It parses the hour from the file name (more reliable than mtime), deletes files older than 7 days (configurable), and removes empty IP directories. It never touches files that are currently open.
10. **Stats:** atomic counters (received, written, bytes, dropped_queue_full, write_errors, open_files, files_deleted). They are logged every 60 s to the journal and dumped on SIGUSR1.
11. **Graceful shutdown (SIGTERM):** close the sockets, drain the shard channels, flush and close all files, then exit. This is the in-memory version of `saveOnShutdown`.

## Data flow

```
             SO_REUSEPORT sockets (32 MB SO_RCVBUF each)
 UDP/514 ──► reader 0 ─┐   recvmmsg batch of 64, one clock read per batch
             reader 1 ─┤
             reader N ─┘
                 │  Message{ip, time, pooled []byte}
                 ▼  fnv(ip) % W  (non-blocking; full queue → dropped_queue_full++)
          ┌─────────────┬─────────────┐
          │ shard 0 chan│ shard W chan│   bounded in-memory queues
          ▼             ▼             ▼
       writer 0  ...  writer W       one goroutine per shard, owns its files
          │  LRU cache: (ip, UTC hour) → *os.File + 64 KiB bufio.Writer
          │  flush: buffer full | queue drained | 1 s tick | rotate/evict/HUP/exit
          ▼
   <dir>/<ip>/<YYYY-MM-DD_HH>.log     "2026-11-18T13:04:05.123Z <raw message>"

   janitor (every 10 min): delete hour files older than the retention period, remove empty ip dirs
```

## Possible later additions
- A disk-assisted queue (rsyslog DA queue / syslog-ng disk-buffer) to absorb overloads that last a long time.
- TCP/TLS input (RFC 6587 / RFC 5425) with real backpressure.
