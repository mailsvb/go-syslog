package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-syslog/internal/config"
)

func startTestServer(t *testing.T, opts ...func(*config.Config)) (*Server, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Dir = t.TempDir()
	cfg.Readers = 2
	cfg.Writers = 2
	cfg.RcvBuf = 256 << 10 // like an untuned CI runner; send() must cope with it
	cfg.StatsInterval = 0
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := Start(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv, cfg
}

// sendWindow is how many datagrams may be in flight before send waits for
// the server to read them. It is far below what even a default ~208 KB
// socket buffer holds, so the kernel never drops datagrams in tests, however
// slow the machine is (CI runners with -race are 5-20x slower).
const sendWindow = 100

// send sends n numbered messages (seq=start..start+n-1) from source to the
// server. After every sendWindow datagrams it waits until the server has
// received all of them; the test fails if one does not arrive.
func send(t *testing.T, srv *Server, source string, start, n int) bool {
	t.Helper()
	return sendf(t, srv, source, "<134>1 2026-11-18T10:00:00Z host app - - - seq=%d\n", start, n)
}

// sendf is send with a message format that contains one %d for the sequence.
// Each call uses its own socket, like each Docker container logger does.
func sendf(t *testing.T, srv *Server, source, format string, start, n int) bool {
	t.Helper()
	c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP(source)}, srv.Addr().(*net.UDPAddr))
	if err != nil {
		if source == "127.0.0.1" {
			t.Fatal(err)
		}
		t.Logf("cannot send from %s: %v", source, err)
		return false
	}
	defer c.Close()
	base := srv.Stats.Received.Load()
	for sent := 0; sent < n; {
		for end := min(sent+sendWindow, n); sent < end; sent++ {
			if _, err := fmt.Fprintf(c, format, start+sent); err != nil {
				t.Fatal(err)
			}
		}
		waitReceived(t, srv, base+uint64(sent))
	}
	return true
}

func waitReceived(t *testing.T, srv *Server, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for srv.Stats.Received.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("datagrams lost: server received %d, sent %d", srv.Stats.Received.Load(), want)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// checkFile verifies that the sequence numbers in all hour files of dir are
// strictly increasing and returns the number of lines.
func checkFile(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no log files in %s (%v)", dir, err)
	}
	lines, last := 0, -1
	for _, f := range files { // Glob returns sorted names = chronological order
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			l := sc.Text()
			if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", l[:24]); err != nil {
				t.Fatalf("bad timestamp prefix in %q", l)
			}
			i := strings.LastIndex(l, "seq=")
			seq, err := strconv.Atoi(l[i+4:])
			if err != nil {
				t.Fatalf("bad line %q", l)
			}
			if seq <= last {
				t.Fatalf("%s: seq %d after %d: per-sender order broken", f, seq, last)
			}
			last = seq
			lines++
		}
		fh.Close()
	}
	return lines
}

func TestEndToEnd(t *testing.T) {
	srv, cfg := startTestServer(t)
	const n = 20000

	sources := []string{"127.0.0.1"}
	if send(t, srv, "127.0.0.1", 0, n) && send(t, srv, "127.0.0.2", 0, n) {
		sources = append(sources, "127.0.0.2") // Linux routes all of 127/8 to lo
	}
	want := uint64(n * len(sources))
	srv.Shutdown() // drains the writer queues

	st := srv.Stats.Snapshot()
	if st.Received != want || st.Written != want || st.DroppedQueueFull != 0 || st.WriteErrors != 0 {
		t.Fatalf("want %d received and written without drops or errors, got %+v", want, st)
	}
	var total uint64
	for _, src := range sources {
		total += uint64(checkFile(t, filepath.Join(cfg.Dir, src)))
	}
	if total != st.Written {
		t.Fatalf("lines on disk = %d, written counter = %d", total, st.Written)
	}
	if st.FilesOpened != st.FilesClosed {
		t.Fatalf("files left open: %+v", st)
	}
}

func TestReopenWhileRunning(t *testing.T) {
	srv, cfg := startTestServer(t)
	send(t, srv, "127.0.0.1", 0, 10)
	srv.Reopen()
	send(t, srv, "127.0.0.1", 10, 10)
	srv.Shutdown()
	if got := checkFile(t, filepath.Join(cfg.Dir, "127.0.0.1")); got != 20 {
		t.Fatalf("got %d lines, want 20", got)
	}
}

func TestEndToEndSplitByTag(t *testing.T) {
	srv, cfg := startTestServer(t, func(c *config.Config) {
		c.SplitByTag = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	})
	const n = 5000
	// Docker log driver formats: rfc5424, the default "unix" and rfc3164.
	sendf(t, srv, "127.0.0.1", "<30>1 2026-11-18T10:00:00.000000Z dockerhost web-1 42 web-1 - seq=%d", 0, n)
	sendf(t, srv, "127.0.0.1", "<30>Nov 18 10:00:00 db-1[43]: seq=%d", 0, n)
	sendf(t, srv, "127.0.0.1", "<30>Nov 18 10:00:00 dockerhost app/cache.1[44]: seq=%d", 0, n)
	srv.Shutdown()

	st := srv.Stats.Snapshot()
	if st.Written != 3*n || st.DroppedQueueFull != 0 || st.WriteErrors != 0 {
		t.Fatalf("stats = %+v", st)
	}
	host := filepath.Join(cfg.Dir, "127.0.0.1")
	for _, tag := range []string{"web-1", "db-1", "app_cache.1"} {
		if got := checkFile(t, filepath.Join(host, tag)); got != n {
			t.Errorf("%s: %d lines, want %d", tag, got, n)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(host, "*.log")); len(files) != 0 {
		t.Errorf("split sender also wrote unsplit files: %v", files)
	}
}
