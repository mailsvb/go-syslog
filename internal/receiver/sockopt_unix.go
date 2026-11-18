//go:build unix

package receiver

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// controlFunc sets SO_REUSEPORT and the largest SO_RCVBUF the kernel accepts
// (up to rcvbuf). Linux silently caps the value at net.core.rmem_max; other
// systems return ENOBUFS, so the value is halved until it is accepted.
func controlFunc(rcvbuf int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var opErr error
		err := c.Control(func(fd uintptr) {
			if opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); opErr != nil {
				return
			}
			for size := rcvbuf; size >= 64<<10; size /= 2 {
				if e := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, size); e == nil || !errors.Is(e, unix.ENOBUFS) {
					return
				}
			}
		})
		if err != nil {
			return err
		}
		return opErr
	}
}

// rcvBufSize returns the effective receive buffer of the socket. Linux reports
// twice the usable size (it accounts for bookkeeping overhead), so it is halved.
func rcvBufSize(c net.PacketConn) (int, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return 0, errors.ErrUnsupported
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var size int
	var opErr error
	err = rc.Control(func(fd uintptr) {
		size, opErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
	})
	if err != nil {
		return 0, err
	}
	if opErr != nil {
		return 0, opErr
	}
	if isLinux {
		size /= 2
	}
	return size, nil
}
