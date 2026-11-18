// Package bufpool provides size-classed byte slice pools so the receive path
// does not allocate a fresh buffer for every datagram.
package bufpool

import (
	"sync"
	"unsafe"
)

var classes = [...]int{256, 1024, 4096, 16384, 65536}

// pools hold a pointer to the first byte of each free buffer. Storing that
// pointer (instead of a *[]byte) lets Put return a buffer without allocating
// a slice header on the heap for every message.
var pools [len(classes)]sync.Pool

// Get returns a slice of length n. Its capacity is the smallest size class
// that fits n, so it can be returned with Put.
func Get(n int) []byte {
	for i, c := range classes {
		if n <= c {
			if p, ok := pools[i].Get().(*byte); ok {
				return unsafe.Slice(p, c)[:n]
			}
			return make([]byte, n, c)
		}
	}
	return make([]byte, n)
}

// Put returns b to its pool. Slices that were not created by Get are ignored.
func Put(b []byte) {
	c := cap(b)
	for i, cl := range classes {
		if c == cl {
			pools[i].Put(unsafe.SliceData(b[:cl]))
			return
		}
	}
}
