package receiver

import "net/netip"

// maxDatagram is the largest possible UDP payload.
const maxDatagram = 65535

// batch holds the datagrams of one receive call. The slots are reused for
// every call; the reader copies each payload out before the next call.
type batch struct {
	bufs  [][]byte     // one maxDatagram slot per datagram
	addrs []netip.Addr // normalized sender; invalid if it could not be parsed
	lens  []int        // payload length
}

func newBatch(size int) batch {
	b := batch{
		bufs:  make([][]byte, size),
		addrs: make([]netip.Addr, size),
		lens:  make([]int, size),
	}
	for i := range b.bufs {
		b.bufs[i] = make([]byte, maxDatagram)
	}
	return b
}

// packetReader fills a batch with up to len(bufs) datagrams per call,
// blocking until at least one is available.
type packetReader interface {
	read() (int, error)
	batch() *batch
}
