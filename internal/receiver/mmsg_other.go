//go:build !linux

package receiver

import (
	"net"
	"net/netip"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/mailsvb/go-syslog/internal/writer"
)

// xnetReader uses x/net's ReadBatch. It is used on systems other than Linux
// (development machines), where it reads one datagram per call anyway.
type xnetReader struct {
	b  batch
	br interface {
		ReadBatch(ms []ipv4.Message, flags int) (int, error)
	}
	ms []ipv4.Message
}

type v6reader struct{ c *ipv6.PacketConn }

func (r v6reader) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	return r.c.ReadBatch(ms, flags)
}

func newPacketReader(c net.PacketConn, size int) (packetReader, error) {
	r := &xnetReader{b: newBatch(size), ms: make([]ipv4.Message, size)}
	for i := range r.ms {
		r.ms[i].Buffers = [][]byte{r.b.bufs[i]}
	}
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && ua.IP.To4() != nil {
		r.br = ipv4.NewPacketConn(c)
	} else {
		r.br = v6reader{ipv6.NewPacketConn(c)}
	}
	return r, nil
}

func (r *xnetReader) batch() *batch { return &r.b }

func (r *xnetReader) read() (int, error) {
	n, err := r.br.ReadBatch(r.ms, 0)
	if err != nil {
		return 0, err
	}
	for i := 0; i < n; i++ {
		r.b.lens[i] = r.ms[i].N
		r.b.addrs[i] = netip.Addr{}
		if ua, ok := r.ms[i].Addr.(*net.UDPAddr); ok && ua != nil {
			if ip, ok := netip.AddrFromSlice(ua.IP); ok {
				r.b.addrs[i] = writer.NormalizeAddr(ip)
			}
		}
	}
	return n, nil
}
