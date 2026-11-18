// Package receiver reads syslog datagrams from UDP sockets.
//
// Like rsyslog imudp and syslog-ng, it opens several SO_REUSEPORT sockets with
// a large SO_RCVBUF and reads them with recvmmsg (via x/net ReadBatch). Each
// reader does as little work as possible: one clock read per batch, copy the
// payload into a pooled buffer, hand it to the writer without blocking.
package receiver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/mailsvb/go-syslog/internal/bufpool"
	"github.com/mailsvb/go-syslog/internal/stats"
	"github.com/mailsvb/go-syslog/internal/writer"
)

// maxDatagram is the largest possible UDP payload.
const maxDatagram = 65535

// Options configures the receiver.
type Options struct {
	Listen    string
	Readers   int
	RcvBuf    int
	BatchSize int
	Dispatch  func(writer.Message) bool
	Stats     *stats.Stats
	Logger    *slog.Logger
}

// Receiver owns the listening sockets.
type Receiver struct {
	o     Options
	conns []net.PacketConn
	wg    sync.WaitGroup
}

// batchReader is implemented by ipv4.PacketConn and ipv6.PacketConn.
type batchReader interface {
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
}

type v6reader struct{ c *ipv6.PacketConn }

func (r v6reader) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	return r.c.ReadBatch(ms, flags)
}

// Listen opens Options.Readers sockets on the same address. It does not start
// reading; call Start for that.
func Listen(ctx context.Context, o Options) (*Receiver, error) {
	if o.Readers <= 0 {
		o.Readers = 1
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 64
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Stats == nil {
		o.Stats = &stats.Stats{}
	}
	r := &Receiver{o: o}
	lc := net.ListenConfig{Control: controlFunc(o.RcvBuf)}
	addr := o.Listen
	for i := 0; i < o.Readers; i++ {
		c, err := lc.ListenPacket(ctx, "udp", addr)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("listen %s: %w", addr, err)
		}
		r.conns = append(r.conns, c)
		if i == 0 {
			// With port 0 the kernel picks a port; the other sockets must use the same one.
			host, _, _ := net.SplitHostPort(o.Listen)
			addr = net.JoinHostPort(host, strconv.Itoa(c.LocalAddr().(*net.UDPAddr).Port))
			if got, err := rcvBufSize(c); err == nil {
				lvl := slog.LevelInfo
				if got < o.RcvBuf {
					lvl = slog.LevelWarn
				}
				o.Logger.Log(ctx, lvl, "socket receive buffer",
					"requested", o.RcvBuf, "effective", got,
					"hint", "raise net.core.rmem_max if effective is lower than requested")
			}
		}
	}
	return r, nil
}

// Addr returns the bound local address.
func (r *Receiver) Addr() net.Addr {
	return r.conns[0].LocalAddr()
}

// Start launches one reader goroutine per socket.
func (r *Receiver) Start() {
	for _, c := range r.conns {
		r.wg.Add(1)
		go r.readLoop(c)
	}
}

// Close closes all sockets and waits for the readers to stop.
func (r *Receiver) Close() {
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.wg.Wait()
}

func newBatchReader(c net.PacketConn) batchReader {
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && ua.IP.To4() != nil {
		return ipv4.NewPacketConn(c)
	}
	return v6reader{ipv6.NewPacketConn(c)}
}

func (r *Receiver) readLoop(c net.PacketConn) {
	defer r.wg.Done()
	// A dedicated OS thread for each reader keeps the socket drained even when
	// the Go scheduler is busy with writer goroutines.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	br := newBatchReader(c)
	ms := make([]ipv4.Message, r.o.BatchSize)
	for i := range ms {
		ms[i].Buffers = [][]byte{make([]byte, maxDatagram)}
	}
	var lastErrLog time.Time
	for {
		n, err := br.ReadBatch(ms, 0)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.o.Stats.ReadErrors.Add(1)
			if time.Since(lastErrLog) > 10*time.Second {
				lastErrLog = time.Now()
				r.o.Logger.Error("read failed", "err", err)
			}
			continue
		}
		now := time.Now() // one clock read per batch (rsyslog timeRequery)
		r.o.Stats.Received.Add(uint64(n))
		for i := 0; i < n; i++ {
			addr, ok := addrOf(ms[i].Addr)
			if !ok {
				continue
			}
			buf := bufpool.Get(ms[i].N)
			copy(buf, ms[i].Buffers[0][:ms[i].N])
			r.o.Dispatch(writer.Message{Addr: addr, Time: now, Data: buf})
		}
	}
}

func addrOf(a net.Addr) (netip.Addr, bool) {
	ua, ok := a.(*net.UDPAddr)
	if !ok || ua == nil {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.Addr{}, false
	}
	return writer.NormalizeAddr(ip), true
}
