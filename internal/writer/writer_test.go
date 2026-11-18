package writer

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-syslog/internal/stats"
)

var (
	ip1 = netip.MustParseAddr("192.0.2.1")
	ip2 = netip.MustParseAddr("192.0.2.2")
	ip3 = netip.MustParseAddr("2001:db8::3")
)

func TestFormatLine(t *testing.T) {
	ts := time.Date(2026, 11, 18, 13, 4, 5, 123456789, time.FixedZone("CET", 3600))
	tests := []struct{ in, want string }{
		{"<34>1 hello", "2026-11-18T12:04:05.123Z <34>1 hello\n"},
		{"<34>hello\n", "2026-11-18T12:04:05.123Z <34>hello\n"},
		{"<34>hello\r\n\x00", "2026-11-18T12:04:05.123Z <34>hello\n"},
		{"<34>line1\nline2", "2026-11-18T12:04:05.123Z <34>line1#012line2\n"},
		{"tab\tok", "2026-11-18T12:04:05.123Z tab\tok\n"},
		{"", "2026-11-18T12:04:05.123Z \n"},
	}
	for _, tt := range tests {
		if got := string(FormatLine(ts, []byte(tt.in))); got != tt.want {
			t.Errorf("FormatLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFilePathAndNormalize(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 59, 0, 0, time.UTC)
	tests := []struct{ ip, want string }{
		{"10.1.2.3", "/d/10.1.2.3/2026-01-02_03.log"},
		{"::ffff:10.1.2.3", "/d/10.1.2.3/2026-01-02_03.log"},
		{"2001:db8::1", "/d/2001:db8::1/2026-01-02_03.log"},
		{"fe80::1%eth0", "/d/fe80::1/2026-01-02_03.log"},
	}
	for _, tt := range tests {
		got := FilePath("/d", NormalizeAddr(netip.MustParseAddr(tt.ip)), ts)
		if got != tt.want {
			t.Errorf("FilePath(%s) = %s, want %s", tt.ip, got, tt.want)
		}
	}
}

func TestHourOf(t *testing.T) {
	a := time.Date(2026, 1, 1, 13, 59, 59, 999, time.UTC)
	b := time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC)
	if hourOf(b)-hourOf(a) != 1 {
		t.Fatalf("hour boundary not detected: %d %d", hourOf(a), hourOf(b))
	}
	if hourOf(time.Unix(-1, 0)) != -1 {
		t.Fatalf("negative times must floor")
	}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	o := Options{Dir: t.TempDir(), Stats: &stats.Stats{}}
	o.setDefaults()
	return o
}

func msg(addr netip.Addr, ts time.Time, s string) Message {
	return Message{Addr: addr, Time: ts, Data: []byte(s)}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShardHourRotation(t *testing.T) {
	o := testOptions(t)
	s := newShard(o, 0)
	t1 := time.Date(2026, 11, 18, 13, 59, 59, 0, time.UTC)
	t2 := t1.Add(time.Second)
	s.handle(msg(ip1, t1, "a"))
	s.handle(msg(ip1, t2, "b"))
	s.flushDirty()
	if len(s.files) != 2 {
		t.Fatalf("expected 2 open files, got %d", len(s.files))
	}
	if got := readFile(t, FilePath(o.Dir, ip1, t1)); got != "2026-11-18T13:59:59.000Z a\n" {
		t.Errorf("hour 13 file = %q", got)
	}
	if got := readFile(t, FilePath(o.Dir, ip1, t2)); got != "2026-11-18T14:00:00.000Z b\n" {
		t.Errorf("hour 14 file = %q", got)
	}
	s.closeAll()
	if st := o.Stats.Snapshot(); st.FilesOpened != 2 || st.FilesClosed != 2 || st.Written != 2 {
		t.Errorf("stats = %+v", st)
	}
}

func TestShardBuffersUntilFlush(t *testing.T) {
	o := testOptions(t)
	s := newShard(o, 0)
	ts := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	s.handle(msg(ip1, ts, "x"))
	if got := readFile(t, FilePath(o.Dir, ip1, ts)); got != "" {
		t.Fatalf("data written before flush: %q", got)
	}
	s.flushDirty()
	if got := readFile(t, FilePath(o.Dir, ip1, ts)); got == "" {
		t.Fatal("data not written after flush")
	}
	s.closeAll()
}

func TestShardLRUEvictionFlushes(t *testing.T) {
	o := testOptions(t)
	o.MaxOpenFiles = 2
	s := newShard(o, 0)
	ts := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	s.handle(msg(ip1, ts, "one"))
	s.handle(msg(ip2, ts, "two"))
	s.handle(msg(ip3, ts, "three")) // evicts ip1 (least recently used)
	if len(s.files) != 2 {
		t.Fatalf("open files = %d, want 2", len(s.files))
	}
	if _, ok := s.files[fileKey{ip1, hourOf(ts)}]; ok {
		t.Fatal("ip1 should have been evicted")
	}
	if got := readFile(t, FilePath(o.Dir, ip1, ts)); !strings.HasSuffix(got, " one\n") {
		t.Errorf("evicted file not flushed: %q", got)
	}
	// Writing to an evicted file reopens it in append mode.
	s.handle(msg(ip1, ts, "again"))
	s.closeAll()
	if got := readFile(t, FilePath(o.Dir, ip1, ts)); strings.Count(got, "\n") != 2 {
		t.Errorf("reopened file content = %q", got)
	}
}

func TestShardReapIdle(t *testing.T) {
	o := testOptions(t)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return now }
	o.IdleTimeout = time.Minute
	s := newShard(o, 0)
	s.handle(msg(ip1, now, "old"))
	now = now.Add(50 * time.Second)
	s.handle(msg(ip2, now, "new"))
	s.flushDirty()

	s.reap(now.Add(20 * time.Second)) // ip1 idle 70s, ip2 idle 20s
	if len(s.files) != 1 {
		t.Fatalf("open files after reap = %d, want 1", len(s.files))
	}
	if _, ok := s.files[fileKey{ip2, hourOf(now)}]; !ok {
		t.Fatal("ip2 should still be open")
	}
	s.closeAll()
}

func TestShardRecreatesDeletedDir(t *testing.T) {
	o := testOptions(t)
	s := newShard(o, 0)
	ts := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	s.handle(msg(ip1, ts, "a"))
	s.closeAll()
	if err := os.RemoveAll(filepath.Join(o.Dir, ip1.String())); err != nil {
		t.Fatal(err)
	}
	s.handle(msg(ip1, ts, "b"))
	s.closeAll()
	if got := readFile(t, FilePath(o.Dir, ip1, ts)); !strings.HasSuffix(got, " b\n") {
		t.Errorf("content = %q", got)
	}
}

func TestDispatchDropsWhenQueueFull(t *testing.T) {
	o := testOptions(t)
	o.QueuePerShard = 2
	d := &Dispatcher{shards: []*shard{newShard(o, 0)}, stats: o.Stats} // shard not running
	ts := time.Now()
	for i := 0; i < 5; i++ {
		d.Dispatch(msg(ip1, ts, "x"))
	}
	if got := o.Stats.DroppedQueueFull.Load(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
}

func TestDispatcherOrderAndShutdownFlush(t *testing.T) {
	o := testOptions(t)
	o.Shards = 4
	o.QueuePerShard = 100000
	d := NewDispatcher(o)
	ts := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
	addrs := []netip.Addr{ip1, ip2, ip3}
	const n = 5000
	for i := 0; i < n; i++ {
		for _, a := range addrs {
			d.Dispatch(msg(a, ts, fmt.Sprintf("seq=%d", i)))
		}
	}
	d.Close()
	for _, a := range addrs {
		lines := strings.Split(strings.TrimSuffix(readFile(t, FilePath(o.Dir, a, ts)), "\n"), "\n")
		if len(lines) != n {
			t.Fatalf("%s: %d lines, want %d", a, len(lines), n)
		}
		for i, l := range lines {
			if !strings.HasSuffix(l, fmt.Sprintf(" seq=%d", i)) {
				t.Fatalf("%s: line %d out of order: %q", a, i, l)
			}
		}
	}
	if st := o.Stats.Snapshot(); st.Written != 3*n || st.FilesOpened != st.FilesClosed {
		t.Errorf("stats = %+v", st)
	}
}

func TestDispatcherFlushInterval(t *testing.T) {
	o := testOptions(t)
	o.FlushInterval = 10 * time.Millisecond
	d := NewDispatcher(o)
	defer d.Close()
	ts := time.Now()
	d.Dispatch(msg(ip1, ts, "tick"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(FilePath(o.Dir, ip1, ts)); bytes.Contains(b, []byte("tick")) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("message not flushed to disk while the server keeps running")
}

func TestReopen(t *testing.T) {
	o := testOptions(t)
	d := NewDispatcher(o)
	ts := time.Now()
	d.Dispatch(msg(ip1, ts, "a"))
	d.Reopen()
	if st := o.Stats.Snapshot(); st.FilesOpened != st.FilesClosed {
		t.Errorf("files still open after Reopen: %+v", st)
	}
	d.Close()
	d.Reopen() // must not block after Close
}

func TestShardIndexSpread(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 256; i++ {
		a := netip.AddrFrom4([4]byte{10, 0, byte(i / 16), byte(i)})
		idx := shardIndex(a, 8)
		if idx != shardIndex(a, 8) {
			t.Fatal("not deterministic")
		}
		seen[idx] = true
	}
	if len(seen) != 8 {
		t.Fatalf("addresses spread over %d of 8 shards", len(seen))
	}
}

func BenchmarkShardHandle(b *testing.B) {
	o := Options{Dir: b.TempDir(), Stats: &stats.Stats{}}
	o.setDefaults()
	s := newShard(o, 0)
	ts := time.Now()
	payload := []byte("<134>1 2026-11-18T10:00:00.000Z host app 1234 ID47 - a typical syslog message of moderate length")
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.handle(Message{Addr: ip1, Time: ts, Data: payload})
		if i%256 == 255 {
			s.flushDirty()
		}
	}
	s.closeAll()
}
