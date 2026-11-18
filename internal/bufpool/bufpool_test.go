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

func TestGetPutNoAllocs(t *testing.T) {
	Put(Get(1000)) // warm the pool
	if n := testing.AllocsPerRun(1000, func() { Put(Get(1000)) }); n != 0 {
		t.Fatalf("Get+Put allocates %.1f times", n)
	}
	foreign := make([]byte, 10)
	if n := testing.AllocsPerRun(1000, func() { Put(foreign) }); n != 0 {
		t.Fatalf("Put of a foreign slice allocates %.1f times", n)
	}
}

func TestReuseKeepsCapacity(t *testing.T) {
	b := Get(5000)
	b[0] = 'x'
	Put(b)
	c := Get(10)
	if cap(Get(16384)) != 16384 || cap(c) != 256 {
		t.Fatal("wrong size class after reuse")
	}
}
