// Package bufpool provides size-classed byte slice pools so the receive path
// does not allocate a fresh buffer for every datagram.
package bufpool

import "sync"

var classes = [...]int{256, 1024, 4096, 16384, 65536}

var pools [len(classes)]sync.Pool

// Get returns a slice of length n. Its capacity is the smallest size class
// that fits n, so it can be returned with Put.
func Get(n int) []byte {
	for i, c := range classes {
		if n <= c {
			if v, ok := pools[i].Get().(*[]byte); ok {
				return (*v)[:n]
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
			b = b[:0]
			pools[i].Put(&b)
			return
		}
	}
}
