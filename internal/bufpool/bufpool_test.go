package bufpool

import "testing"

func TestGetPut(t *testing.T) {
	for _, n := range []int{0, 1, 256, 257, 4000, 65535, 65536, 70000} {
		b := Get(n)
		if len(b) != n {
			t.Fatalf("Get(%d) len = %d", n, len(b))
		}
		Put(b)
	}
	b := Get(100)
	if cap(b) != 256 {
		t.Fatalf("Get(100) cap = %d, want 256", cap(b))
	}
}
