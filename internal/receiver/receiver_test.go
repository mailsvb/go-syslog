package receiver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/mailsvb/go-syslog/internal/writer"
)

type chanSink chan writer.Message

func (c chanSink) Add(m writer.Message) { c <- m }
func (c chanSink) Flush()               {}

// TestReceive checks addresses and payloads for IPv4, IPv6 and IPv4 on a
// dual-stack socket (v4-mapped, must be reported as plain IPv4).
func TestReceive(t *testing.T) {
	for _, tc := range []struct{ listen, src, want string }{
		{"127.0.0.1:0", "127.0.0.1", "127.0.0.1"},
		{"[::1]:0", "::1", "::1"},
		{":0", "127.0.0.1", "127.0.0.1"},
	} {
		t.Run(tc.listen+"<-"+tc.src, func(t *testing.T) {
			got := make(chan writer.Message, 1000)
			r, err := Listen(context.Background(), Options{
				Listen:    tc.listen,
				Readers:   1,
				RcvBuf:    1 << 20,
				BatchSize: 8, // smaller than the burst: several full batches
				NewSink:   func() Sink { return chanSink(got) },
				Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if err != nil {
				t.Skipf("cannot listen on %s: %v", tc.listen, err)
			}
			defer r.Close()
			r.Start()

			dst := &net.UDPAddr{IP: net.ParseIP(tc.src), Port: r.Addr().(*net.UDPAddr).Port}
			c, err := net.DialUDP("udp", nil, dst)
			if err != nil {
				t.Skipf("cannot send from %s: %v", tc.src, err)
			}
			defer c.Close()
			const n = 50
			for i := 0; i < n; i++ {
				if _, err := fmt.Fprintf(c, "msg %d %s", i, make([]byte, i*100)); err != nil {
					t.Fatal(err)
				}
			}
			want := netip.MustParseAddr(tc.want)
			for i := 0; i < n; i++ {
				select {
				case m := <-got:
					if m.Addr != want {
						t.Fatalf("addr = %v, want %v", m.Addr, want)
					}
					if exp := fmt.Sprintf("msg %d %s", i, make([]byte, i*100)); string(m.Data) != exp {
						t.Fatalf("datagram %d: got %d bytes %.10q, want %d bytes", i, len(m.Data), m.Data, len(exp))
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("received %d of %d datagrams", i, n)
				}
			}
		})
	}
}
