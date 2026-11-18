package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mailsvb/go-syslog/internal/config"
)

func startTestServer(t *testing.T) (*Server, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Dir = t.TempDir()
	cfg.Readers = 2
	cfg.Writers = 2
	cfg.StatsInterval = 0
	srv, err := Start(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv, cfg
}

// send sends n numbered messages from source to dst, paced so that a small
// default kernel receive buffer (as on CI runners) does not overflow.
func send(t *testing.T, source string, dst *net.UDPAddr, start, n int) bool {
	t.Helper()
	c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP(source)}, dst)
	if err != nil {
		if source == "127.0.0.1" {
			t.Fatal(err)
		}
		t.Logf("cannot send from %s: %v", source, err)
		return false
	}
	defer c.Close()
	for i := start; i < start+n; i++ {
		fmt.Fprintf(c, "<134>1 2026-11-18T10:00:00Z host app - - - seq=%d\n", i)
		if i%200 == 199 {
			time.Sleep(time.Millisecond)
		}
	}
	return true
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
	dst := srv.Addr().(*net.UDPAddr)
	const n = 20000

	sources := []string{"127.0.0.1"}
	if send(t, "127.0.0.1", dst, 0, n) && send(t, "127.0.0.2", dst, 0, n) {
		sources = append(sources, "127.0.0.2") // Linux routes all of 127/8 to lo
	}
	want := uint64(n * len(sources))

	// Wait until everything that arrived has been processed.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := srv.Stats.Snapshot()
		if st.Received >= want {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	srv.Shutdown()

	st := srv.Stats.Snapshot()
	if st.Received < want*95/100 {
		t.Fatalf("received only %d of %d datagrams", st.Received, want)
	}
	if st.Written+st.DroppedQueueFull != st.Received || st.WriteErrors != 0 {
		t.Fatalf("inconsistent stats: %+v", st)
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
	dst := srv.Addr().(*net.UDPAddr)
	send(t, "127.0.0.1", dst, 0, 10)
	time.Sleep(100 * time.Millisecond)
	srv.Reopen()
	send(t, "127.0.0.1", dst, 10, 10)
	time.Sleep(100 * time.Millisecond)
	srv.Shutdown()
	if got := checkFile(t, filepath.Join(cfg.Dir, "127.0.0.1")); got != 20 {
		t.Fatalf("got %d lines, want 20", got)
	}
}
