package receiver

import (
	"net/netip"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func sockaddr4(a [4]byte) unix.RawSockaddrInet6 {
	var sa unix.RawSockaddrInet6
	sa4 := (*unix.RawSockaddrInet4)(unsafe.Pointer(&sa))
	sa4.Family, sa4.Addr = unix.AF_INET, a
	return sa
}

func sockaddr6(s string) unix.RawSockaddrInet6 {
	return unix.RawSockaddrInet6{Family: unix.AF_INET6, Addr: netip.MustParseAddr(s).As16(), Scope_id: 2}
}

func TestSockaddrAddr(t *testing.T) {
	const n4, n6 = unix.SizeofSockaddrInet4, unix.SizeofSockaddrInet6
	for _, tc := range []struct {
		name string
		sa   unix.RawSockaddrInet6
		n    uint32
		want string // "" = invalid
	}{
		{"v4", sockaddr4([4]byte{192, 0, 2, 1}), n4, "192.0.2.1"},
		{"v6", sockaddr6("2001:db8::1"), n6, "2001:db8::1"},
		{"v4-mapped", sockaddr6("::ffff:10.1.2.3"), n6, "10.1.2.3"},
		{"short v4", sockaddr4([4]byte{1, 2, 3, 4}), 2, ""},
		{"short v6", sockaddr6("::1"), n4, ""},
		{"unix family", unix.RawSockaddrInet6{Family: unix.AF_UNIX}, n6, ""},
	} {
		got := sockaddrAddr(&tc.sa, tc.n)
		if tc.want == "" {
			if got.IsValid() {
				t.Errorf("%s: got %v, want invalid", tc.name, got)
			}
			continue
		}
		if got != netip.MustParseAddr(tc.want) {
			t.Errorf("%s: got %v, want %s", tc.name, got, tc.want)
		}
	}
}
