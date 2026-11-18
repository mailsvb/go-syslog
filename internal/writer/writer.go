// Package writer turns received messages into per-sender, per-hour log files.
//
// Messages are sharded by sender address. Each shard is a single goroutine that
// owns its open files, so per-sender ordering is preserved and no locks are
// needed on the write path. Writes go into a bufio.Writer per file and are
// flushed when the shard's queue runs empty, on a timer, on rotation/eviction
// and on shutdown, similar to rsyslog's omfile (asyncWriting, flushOnTXEnd,
// dynaFileCache) and syslog-ng's flush_lines/flush_timeout/time_reap.
package writer

import (
	"bufio"
	"container/list"
	"errors"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/mailsvb/go-syslog/internal/bufpool"
	"github.com/mailsvb/go-syslog/internal/stats"
)

// Message is one received datagram.
type Message struct {
	Addr netip.Addr // normalized sender address
	Time time.Time  // receive time
	Data []byte     // payload, owned by the writer after Dispatch (from bufpool)

	// Set by Dispatch for senders in Options.SplitByTag: the message goes to
	// <ip>/<tag>/ and Data[TagStart:TagEnd] is the raw tag (empty: untagged).
	Split            bool
	TagStart, TagEnd int32
}

// Options configures the writer shards.
type Options struct {
	Dir           string
	Shards        int
	QueuePerShard int
	WriteBufSize  int
	MaxOpenFiles  int // per shard
	FlushInterval time.Duration
	IdleTimeout   time.Duration
	FsyncOnClose  bool
	SplitByTag    []netip.Prefix // senders whose files are split by syslog tag
	DirMode       os.FileMode
	FileMode      os.FileMode
	Now           func() time.Time // for tests; defaults to time.Now
	Stats         *stats.Stats
	Logger        *slog.Logger
}

func (o *Options) setDefaults() {
	if o.Shards <= 0 {
		o.Shards = 1
	}
	if o.QueuePerShard <= 0 {
		o.QueuePerShard = 1024
	}
	if o.WriteBufSize <= 0 {
		o.WriteBufSize = 64 << 10
	}
	if o.MaxOpenFiles <= 0 {
		o.MaxOpenFiles = 1024
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 2 * time.Minute
	}
	if o.DirMode == 0 {
		o.DirMode = 0o750
	}
	if o.FileMode == 0 {
		o.FileMode = 0o640
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Stats == nil {
		o.Stats = &stats.Stats{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// maxBatch bounds how many queued messages are written before buffers are
// flushed, so data keeps reaching the disk under sustained load.
const maxBatch = 4096

// errLogEvery limits how often a shard logs write errors (e.g. disk full).
const errLogEvery = 10 * time.Second

// maxInterned bounds the per-shard cache of tag strings.
const maxInterned = 4096

// Dispatcher routes messages to writer shards by sender address (and tag).
type Dispatcher struct {
	shards []*shard
	split  []netip.Prefix
	stats  *stats.Stats
}

// NewDispatcher creates and starts the writer shards.
func NewDispatcher(o Options) *Dispatcher {
	o.setDefaults()
	d := &Dispatcher{split: o.SplitByTag, stats: o.Stats}
	for i := 0; i < o.Shards; i++ {
		s := newShard(o, i)
		d.shards = append(d.shards, s)
		go s.run()
	}
	return d
}

const (
	fnvOffset = 2166136261
	fnvPrime  = 16777619
)

// hashAddr hashes the address with FNV-1a.
func hashAddr(a netip.Addr) uint32 {
	h := uint32(fnvOffset)
	for _, c := range a.As16() {
		h = (h ^ uint32(c)) * fnvPrime
	}
	return h
}

// shardIndex picks the shard for a sender that is not split by tag.
func shardIndex(a netip.Addr, n int) int {
	if n == 1 {
		return 0
	}
	return int(hashAddr(a) % uint32(n))
}

// splits reports whether files of sender a are split by tag.
func (d *Dispatcher) splits(a netip.Addr) bool {
	for _, p := range d.split {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Dispatch queues m for writing. It never blocks: if the shard queue is full
// the message is dropped and counted (rsyslog "discard mark" behaviour).
// For split senders it finds the tag and picks the shard by (ip, tag), so the
// sources behind one IP are spread over the writers.
// Dispatch must not be called after Close.
func (d *Dispatcher) Dispatch(m Message) bool {
	var idx int
	if len(d.split) > 0 && d.splits(m.Addr) {
		start, end := parseTag(m.Data)
		m.Split, m.TagStart, m.TagEnd = true, int32(start), int32(end)
		if n := len(d.shards); n > 1 {
			idx = int(hashTag(hashAddr(m.Addr), m.Data[start:end]) % uint32(n))
		}
	} else {
		idx = shardIndex(m.Addr, len(d.shards))
	}
	s := d.shards[idx]
	select {
	case s.ch <- m:
		return true
	default:
		d.stats.DroppedQueueFull.Add(1)
		bufpool.Put(m.Data)
		return false
	}
}

// Reopen flushes and closes all open files; they are reopened on the next
// message. Used for SIGHUP.
func (d *Dispatcher) Reopen() {
	for _, s := range d.shards {
		done := make(chan struct{})
		select {
		case s.ctl <- done:
			<-done
		case <-s.done:
		}
	}
}

// Close drains all queues, flushes and closes every file and waits for the
// shards to stop. No Dispatch calls may happen concurrently or afterwards.
func (d *Dispatcher) Close() {
	for _, s := range d.shards {
		close(s.ch)
	}
	for _, s := range d.shards {
		<-s.done
	}
}

type shard struct {
	o     Options
	id    int
	ch    chan Message
	ctl   chan chan struct{}
	done  chan struct{}
	files map[fileKey]*openFile
	tags  map[string]string // raw tag -> sanitized tag (interned)
	lru   *list.List        // front = most recently written
	dirty []*openFile

	scratch    []byte
	lastTime   time.Time
	lastStamp  []byte
	lastErrLog time.Time
}

func newShard(o Options, id int) *shard {
	return &shard{
		o:     o,
		id:    id,
		ch:    make(chan Message, o.QueuePerShard),
		ctl:   make(chan chan struct{}),
		done:  make(chan struct{}),
		files: make(map[fileKey]*openFile),
		tags:  make(map[string]string),
		lru:   list.New(),
	}
}

func (s *shard) run() {
	defer close(s.done)
	t := time.NewTicker(s.o.FlushInterval)
	defer t.Stop()
	for {
		select {
		case m, ok := <-s.ch:
			if !ok {
				s.closeAll()
				return
			}
			s.handle(m)
		drain:
			for i := 1; i < maxBatch; i++ {
				select {
				case m, ok = <-s.ch:
					if !ok {
						s.closeAll()
						return
					}
					s.handle(m)
				default:
					break drain
				}
			}
			s.flushDirty() // queue drained: end of transaction
		case <-t.C:
			s.flushDirty()
			s.reap(s.o.Now())
		case reply := <-s.ctl:
			s.closeAll()
			close(reply)
		}
	}
}

// handle writes one message into the buffer of its (sender, hour) file.
func (s *shard) handle(m Message) {
	defer bufpool.Put(m.Data)
	key := fileKey{addr: m.Addr, tag: s.tagOf(m), hour: hourOf(m.Time)}
	of := s.files[key]
	if of == nil {
		var err error
		of, err = s.open(key, m.Time)
		if err != nil {
			s.o.Stats.WriteErrors.Add(1)
			s.logErr("open failed", FilePath(s.o.Dir, key.addr, key.tag, m.Time), err)
			return
		}
	} else {
		s.lru.MoveToFront(of.elem)
	}

	n, err := s.writeLine(of.w, m)
	if err != nil {
		s.o.Stats.WriteErrors.Add(1)
		s.logErr("write failed", of.path, err)
		s.close(of)
		return
	}
	of.lastWrite = s.o.Now()
	if !of.dirty {
		of.dirty = true
		s.dirty = append(s.dirty, of)
	}
	s.o.Stats.Written.Add(1)
	s.o.Stats.BytesWritten.Add(uint64(n))
}

// tagOf returns the directory name for the tag of m ("" if m is not split).
// Known tags are looked up without allocating; a new tag is sanitized once.
func (s *shard) tagOf(m Message) string {
	if !m.Split {
		return ""
	}
	raw := m.Data[m.TagStart:m.TagEnd]
	if tag, ok := s.tags[string(raw)]; ok {
		return tag
	}
	if len(s.tags) >= maxInterned {
		clear(s.tags) // only a cache; bounds memory when names churn
	}
	tag := sanitizeTag(raw)
	s.tags[string(raw)] = tag
	return tag
}

func (s *shard) writeLine(w *bufio.Writer, m Message) (int, error) {
	if !m.Time.Equal(s.lastTime) || s.lastStamp == nil {
		s.lastTime = m.Time
		s.lastStamp = appendTimestamp(s.lastStamp[:0], m.Time)
	}
	msg := trimTrailing(m.Data)
	n := len(s.lastStamp) + 2
	if _, err := w.Write(s.lastStamp); err != nil {
		return 0, err
	}
	if err := w.WriteByte(' '); err != nil {
		return 0, err
	}
	if needsEscape(msg) {
		s.scratch = appendEscaped(s.scratch[:0], msg)
		msg = s.scratch
	}
	if _, err := w.Write(msg); err != nil {
		return 0, err
	}
	if err := w.WriteByte('\n'); err != nil {
		return 0, err
	}
	return n + len(msg), nil
}

func (s *shard) open(key fileKey, t time.Time) (*openFile, error) {
	for s.lru.Len() >= s.o.MaxOpenFiles {
		s.close(s.lru.Back().Value.(*openFile))
	}
	path := FilePath(s.o.Dir, key.addr, key.tag, t)
	const flags = os.O_WRONLY | os.O_APPEND | os.O_CREATE
	f, err := os.OpenFile(path, flags, s.o.FileMode)
	if errors.Is(err, fs.ErrNotExist) {
		// Directory missing (new sender, or removed by the janitor).
		if err = os.MkdirAll(filepath.Dir(path), s.o.DirMode); err == nil {
			f, err = os.OpenFile(path, flags, s.o.FileMode)
		}
	}
	if err != nil {
		return nil, err
	}
	of := &openFile{key: key, path: path, f: f, w: bufio.NewWriterSize(f, s.o.WriteBufSize)}
	of.elem = s.lru.PushFront(of)
	s.files[key] = of
	s.o.Stats.FilesOpened.Add(1)
	return of, nil
}

func (s *shard) flushDirty() {
	for i, of := range s.dirty {
		s.dirty[i] = nil
		if !of.dirty {
			continue // closed (and flushed) in the meantime
		}
		of.dirty = false
		if err := of.w.Flush(); err != nil {
			s.o.Stats.WriteErrors.Add(1)
			s.logErr("flush failed", of.path, err)
		}
	}
	s.dirty = s.dirty[:0]
}

// close flushes and closes one file and removes it from the cache.
func (s *shard) close(of *openFile) {
	if err := of.w.Flush(); err != nil {
		s.o.Stats.WriteErrors.Add(1)
		s.logErr("flush failed", of.path, err)
	}
	of.dirty = false
	if s.o.FsyncOnClose {
		if err := of.f.Sync(); err != nil {
			s.logErr("fsync failed", of.path, err)
		}
	}
	if err := of.f.Close(); err != nil {
		s.o.Stats.WriteErrors.Add(1)
		s.logErr("close failed", of.path, err)
	}
	s.lru.Remove(of.elem)
	delete(s.files, of.key)
	s.o.Stats.FilesClosed.Add(1)
}

// reap closes files that have not been written for IdleTimeout. This also
// closes the previous hour's files shortly after the hour changes.
func (s *shard) reap(now time.Time) {
	for e := s.lru.Back(); e != nil; {
		of := e.Value.(*openFile)
		if now.Sub(of.lastWrite) < s.o.IdleTimeout {
			return // everything further to the front was written more recently
		}
		prev := e.Prev()
		s.close(of)
		e = prev
	}
}

func (s *shard) closeAll() {
	for e := s.lru.Back(); e != nil; {
		prev := e.Prev()
		s.close(e.Value.(*openFile))
		e = prev
	}
	s.dirty = s.dirty[:0]
}

func (s *shard) logErr(msg, path string, err error) {
	now := time.Now()
	if now.Sub(s.lastErrLog) < errLogEvery {
		return
	}
	s.lastErrLog = now
	s.o.Logger.Error(msg, "shard", s.id, "path", path, "err", err)
}
