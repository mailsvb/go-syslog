//go:build linux

package receiver

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/mailsvb/go-syslog/internal/writer"
)

// mmsgReader calls recvmmsg directly. Unlike x/net's ReadBatch it reads the
// sender address straight from the sockaddr into a netip.Addr, so receiving
// does not allocate (x/net allocates a net.IP and a *net.UDPAddr per datagram).
type mmsgReader struct {
	b     batch
	rc    syscall.RawConn
	hs    []unix.Mmsghdr
	iovs  []unix.Iovec
	names []unix.RawSockaddrInet6 // large enough for IPv4 and IPv6 sockaddrs

	// Result of the last recv callback; recvFn is bound once so read does
	// not allocate a closure per call.
	n      int
	errno  syscall.Errno
	recvFn func(fd uintptr) bool
}

func newPacketReader(c net.PacketConn, size int) (packetReader, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil, fmt.Errorf("%T has no raw connection", c)
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil, err
	}
	r := &mmsgReader{
		b:     newBatch(size),
		rc:    rc,
		hs:    make([]unix.Mmsghdr, size),
		iovs:  make([]unix.Iovec, size),
		names: make([]unix.RawSockaddrInet6, size),
	}
	for i := range r.hs {
		r.iovs[i].Base = &r.b.bufs[i][0]
		r.iovs[i].SetLen(len(r.b.bufs[i]))
		h := &r.hs[i].Hdr
		h.Name = (*byte)(unsafe.Pointer(&r.names[i]))
		h.Iov = &r.iovs[i]
		h.SetIovlen(1)
	}
	r.recvFn = r.recv
	return r, nil
}

func (r *mmsgReader) batch() *batch { return &r.b }

// recv runs inside RawConn.Read. Returning false on EAGAIN makes the
// runtime poller wait until the socket is readable and call it again.
func (r *mmsgReader) recv(fd uintptr) bool {
	for i := range r.hs {
		// The kernel overwrites these on return.
		r.hs[i].Hdr.Namelen = unix.SizeofSockaddrInet6
		r.hs[i].Hdr.Flags = 0
	}
	n, _, errno := unix.Syscall6(unix.SYS_RECVMMSG, fd,
		uintptr(unsafe.Pointer(&r.hs[0])), uintptr(len(r.hs)), 0, 0, 0)
	if errno == unix.EAGAIN || errno == unix.EWOULDBLOCK {
		return false
	}
	r.n, r.errno = int(n), errno
	return true
}

func (r *mmsgReader) read() (int, error) {
	if err := r.rc.Read(r.recvFn); err != nil {
		return 0, err // net.ErrClosed after Close
	}
	if r.errno != 0 {
		return 0, fmt.Errorf("recvmmsg: %w", r.errno)
	}
	for i := 0; i < r.n; i++ {
		r.b.lens[i] = int(r.hs[i].Len)
		r.b.addrs[i] = sockaddrAddr(&r.names[i], r.hs[i].Hdr.Namelen)
	}
	return r.n, nil
}

// sockaddrAddr returns the normalized address of a sockaddr_in or
// sockaddr_in6 of length n, or the zero Addr if it is neither.
func sockaddrAddr(sa *unix.RawSockaddrInet6, n uint32) netip.Addr {
	switch {
	case sa.Family == unix.AF_INET && n >= unix.SizeofSockaddrInet4:
		sa4 := (*unix.RawSockaddrInet4)(unsafe.Pointer(sa))
		return netip.AddrFrom4(sa4.Addr)
	case sa.Family == unix.AF_INET6 && n >= unix.SizeofSockaddrInet6:
		return writer.NormalizeAddr(netip.AddrFrom16(sa.Addr))
	}
	return netip.Addr{}
}
