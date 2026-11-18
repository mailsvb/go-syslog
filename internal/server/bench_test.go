package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/mailsvb/go-syslog/internal/config"
)

// benchWindow bounds the datagrams in flight (sent but not yet written or
// dropped by the server), so the benchmark measures how fast the server
// drains its sockets and writes the files, not how many datagrams the kernel
// throws away. It must stay well below what the socket buffers can hold.
const benchWindow = 8192

// benchSocketsPerIP gives every source IP several source ports, so the
// SO_REUSEPORT hash spreads the load over all readers.
const benchSocketsPerIP = 4

// BenchmarkEndToEnd sends b.N datagrams over loopback through the complete
// server (sockets, readers, writer shards, files) and reports the cost per
// message. Run it on Linux, the target platform; on other systems only
// 127.0.0.1 is usable and recvmmsg is emulated.
//
//	go test -run x -bench EndToEnd -benchmem ./internal/server
//
// allocs/op includes the sending side.
func BenchmarkEndToEnd(b *testing.B) {
	for _, ips := range []int{1, 64} {
		b.Run(fmt.Sprintf("ips=%d", ips), func(b *testing.B) { benchEndToEnd(b, ips) })
	}
}

func benchEndToEnd(b *testing.B, ips int) {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Dir = b.TempDir()
	cfg.StatsInterval = 0
	srv, err := Start(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		b.Fatal(err)
	}
	defer srv.Shutdown()

	var conns []*ipv4.PacketConn
	for i := 0; i < ips; i++ {
		src := net.IPv4(127, 0, 0, byte(1+i))
		for j := 0; j < benchSocketsPerIP; j++ {
			c, err := net.DialUDP("udp4", &net.UDPAddr{IP: src}, srv.Addr().(*net.UDPAddr))
			if err != nil {
				if i == 0 {
					b.Fatal(err)
				}
				b.Skipf("cannot send from %s (only Linux routes all of 127/8): %v", src, err)
			}
			defer c.Close()
			conns = append(conns, ipv4.NewPacketConn(c))
		}
	}

	payload := []byte("<134>1 2026-11-18T10:00:00.000Z host.example.com app 1234 ID47 - a typical syslog message of moderate length\n")
	done := func() uint64 {
		st := srv.Stats
		return st.Written.Load() + st.DroppedQueueFull.Load()
	}
	base := done()

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()

	// One goroutine per socket group, each sending its share in sendmmsg batches.
	const senders = 4
	var sent atomic.Uint64
	var wg sync.WaitGroup
	for g := 0; g < senders; g++ {
		share := b.N / senders
		if g == 0 {
			share += b.N % senders
		}
		wg.Add(1)
		go func(g, share int) {
			defer wg.Done()
			ms := make([]ipv4.Message, 16)
			for i := range ms {
				ms[i].Buffers = [][]byte{payload}
			}
			for k := g; share > 0; k += senders {
				for sent.Load()-(done()-base) > benchWindow {
					time.Sleep(20 * time.Microsecond)
				}
				n := min(share, 16)
				c := conns[k%len(conns)]
				w, err := c.WriteBatch(ms[:n], 0)
				if err != nil {
					b.Error(err)
					return
				}
				sent.Add(uint64(w))
				share -= w
			}
		}(g, share)
	}
	wg.Wait()

	// Wait for the server to catch up. Datagrams the kernel dropped never
	// arrive; report them instead of hanging.
	var lost uint64
	last, lastChange := done(), time.Now()
	for done()-base < uint64(b.N) {
		if d := done(); d != last {
			last, lastChange = d, time.Now()
		} else if time.Since(lastChange) > time.Second {
			lost = uint64(b.N) - (d - base)
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	elapsed := time.Since(start)
	b.StopTimer()

	if lost > 0 {
		b.ReportMetric(float64(lost), "lost")
	}
	b.ReportMetric(float64(srv.Stats.DroppedQueueFull.Load()), "queue-drops")
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "msgs/s")
}
