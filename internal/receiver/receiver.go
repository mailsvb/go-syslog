// Package receiver reads syslog datagrams from UDP sockets.
//
// Like rsyslog imudp and syslog-ng, it opens several SO_REUSEPORT sockets with
// a large SO_RCVBUF and reads them with recvmmsg (mmsg_linux.go). Each
// reader does as little work as possible: one clock read per batch, copy the
// payload into a pooled buffer, hand the batch to the writers without blocking.
package receiver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/mailsvb/go-syslog/internal/bufpool"
	"github.com/mailsvb/go-syslog/internal/stats"
	"github.com/mailsvb/go-syslog/internal/writer"
)

// Options configures the receiver.
type Options struct {
	Listen    string
	Readers   int
	RcvBuf    int
	BatchSize int
	// NewSink is called once per reader goroutine. The reader Adds every
	// datagram of a receive call and then calls Flush.
	NewSink func() Sink
	Stats   *stats.Stats
	Logger  *slog.Logger
}

// Sink takes the messages of one reader. writer.Batcher implements it.
type Sink interface {
	Add(writer.Message)
	Flush()
}

// Receiver owns the listening sockets.
type Receiver struct {
	o     Options
	conns []net.PacketConn
	wg    sync.WaitGroup
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

func (r *Receiver) readLoop(c net.PacketConn) {
	defer r.wg.Done()
	// A dedicated OS thread for each reader keeps the socket drained even when
	// the Go scheduler is busy with writer goroutines.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	pr, err := newPacketReader(c, r.o.BatchSize)
	if err != nil {
		r.o.Logger.Error("cannot read socket", "err", err)
		return
	}
	b := pr.batch()
	sink := r.o.NewSink()
	var lastErrLog time.Time
	for {
		n, err := pr.read()
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
			addr := b.addrs[i]
			if !addr.IsValid() {
				continue
			}
			buf := bufpool.Get(b.lens[i])
			copy(buf, b.bufs[i])
			sink.Add(writer.Message{Addr: addr, Time: now, Data: buf})
		}
		sink.Flush()
	}
}
