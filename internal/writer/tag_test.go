package writer

import (
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseTag(t *testing.T) {
	long := strings.Repeat("x", 600)
	tests := []struct{ name, in, want string }{
		// Docker log driver formats (syslog-format option)
		{"docker unix", "<30>Nov 18 13:04:05 web-1[4242]: GET / 200", "web-1"},
		{"docker unix padded day", "<30>Nov  8 13:04:05 web-1[4242]: hi", "web-1"},
		{"docker rfc3164", "<30>Nov 18 13:04:05 dockerhost web-1[4242]: hi", "web-1"},
		{"docker rfc5424", "<30>1 2026-11-18T13:04:05Z dockerhost web-1 4242 web-1 - hi", "web-1"},
		{"docker rfc5424micro", "<30>1 2026-11-18T13:04:05.123456Z dockerhost app/web.1 4242 app/web.1 - hi", "app/web.1"},
		// generic RFC 5424
		{"5424 nil app", "<134>1 2026-11-18T13:04:05Z host - - - - hi", ""},
		{"5424 nil host", "<134>1 2026-11-18T13:04:05Z - app - - - hi", "app"},
		{"5424 version 2", "<134>2 ts host app - - - hi", "app"},
		{"5424 unterminated app", "<134>1 ts host app", ""},
		{"5424 truncated", "<134>1 ts", ""},
		// generic RFC 3164
		{"3164 host tag colon", "<13>Nov 18 13:04:05 router sshd: Accepted", "sshd"},
		{"3164 tag colon no host", "<13>Nov 18 13:04:05 sshd: Accepted", "sshd"},
		{"3164 ipv6 host", "<13>Nov 18 13:04:05 fe80::1 kernel: x", "kernel"},
		{"3164 host tag space", "<13>Nov 18 13:04:05 router tag msg", "tag"},
		{"3164 no stamp tag", "<13>cron[12]: job", "cron"},
		{"3164 no stamp text", "<13>hello world", ""},
		{"3164 iso stamp", "<13>2026-11-18T13:04:05Z host app: x", ""},
		{"colon inside host", "<13>Nov 18 13:04:05 a:b c d", "c"},
		// malformed
		{"empty", "", ""},
		{"no pri", "Nov 18 13:04:05 web-1[1]: x", ""},
		{"bad pri", "<1234>Nov 18 13:04:05 web-1[1]: x", ""},
		{"pri only", "<13>", ""},
		{"empty tag", "<13>Nov 18 13:04:05 [1]: x", ""},
		{"beyond scan limit", "<13>Nov 18 13:04:05 " + long + "[1]: x", ""},
	}
	for _, tt := range tests {
		start, end := parseTag([]byte(tt.in))
		if got := tt.in[start:end]; got != tt.want {
			t.Errorf("%s: parseTag(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}

func TestSanitizeTag(t *testing.T) {
	tests := []struct{ in, want string }{
		{"web-1", "web-1"},
		{"my_app.v2", "my_app.v2"},
		{"app/web.1", "app_web.1"},
		{"../../etc", ".._.._etc"},
		{"..", Untagged},
		{".", Untagged},
		{"-", Untagged},
		{"", Untagged},
		{"a\x00b\nc", "a_b_c"},
		{"ünï", "__n__"}, // UTF-8 bytes are replaced one by one
		{strings.Repeat("y", 100), strings.Repeat("y", MaxTagLen)},
	}
	for _, tt := range tests {
		if got := sanitizeTag([]byte(tt.in)); got != tt.want {
			t.Errorf("sanitizeTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHashTagMatchesSanitized(t *testing.T) {
	// Raw tags that land in the same directory must land on the same shard.
	same := [][2]string{
		{"a/b", "a_b"},
		{"..", "_untagged"},
		{"", "-"},
		{strings.Repeat("z", 70), strings.Repeat("z", 80)},
	}
	for _, p := range same {
		if hashTag(1, []byte(p[0])) != hashTag(1, []byte(p[1])) {
			t.Errorf("hashTag(%q) != hashTag(%q)", p[0], p[1])
		}
	}
	if hashTag(1, []byte("web-1")) == hashTag(1, []byte("web-2")) {
		t.Error("different tags hash equal")
	}
}

func splitDispatcher(t testing.TB, shards int, prefixes ...string) (*Dispatcher, Options) {
	o := Options{Dir: t.TempDir(), Shards: shards, QueuePerShard: 1 << 16}
	for _, p := range prefixes {
		o.SplitByTag = append(o.SplitByTag, netip.MustParsePrefix(p))
	}
	o.setDefaults()
	return NewDispatcher(o), o
}

func TestDispatchSplitByTag(t *testing.T) {
	d, o := splitDispatcher(t, 4, "192.0.2.0/30")
	ts := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	d.Dispatch(msg(ip1, ts, "<30>1 2026-11-18T10:00:00Z dockerhost web-1 1 web-1 - a"))
	d.Dispatch(msg(ip1, ts, "<30>Nov 18 10:00:00 db-1[2]: b"))
	d.Dispatch(msg(ip1, ts, "<30>Nov 18 10:00:00 ../x[2]: c"))
	d.Dispatch(msg(ip1, ts, "no header at all"))
	d.Dispatch(msg(ip3, ts, "<30>Nov 18 10:00:00 web-1[2]: not split"))
	d.Close()

	for tag, want := range map[string]string{
		"web-1":  " a\n",
		"db-1":   " b\n",
		".._x":   " c\n",
		Untagged: " no header at all\n",
	} {
		if got := readFile(t, FilePath(o.Dir, ip1, tag, ts)); !strings.HasSuffix(got, want) {
			t.Errorf("tag %s: content %q, want suffix %q", tag, got, want)
		}
	}
	if got := readFile(t, FilePath(o.Dir, ip3, "", ts)); !strings.HasSuffix(got, " not split\n") {
		t.Errorf("non-split sender: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(o.Dir, ip1.String()))
	if len(entries) != 4 {
		t.Errorf("split sender dir has %d entries, want 4 tag dirs", len(entries))
	}
}

func TestSplitInternBounded(t *testing.T) {
	o := testOptions(t)
	o.MaxOpenFiles = 8
	s := newShard(o, 0)
	ts := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for i := 0; i < maxInterned+10; i++ {
		data := []byte("<30>Nov 18 10:00:00 c" + strconv.Itoa(i) + "[1]: m")
		start, end := parseTag(data)
		s.handle(Message{Addr: ip1, Time: ts, Data: data, Split: true, TagStart: int32(start), TagEnd: int32(end)})
	}
	if len(s.tags) > maxInterned {
		t.Fatalf("intern cache grew to %d", len(s.tags))
	}
	s.closeAll()
}

func TestDispatchSplitNoAllocs(t *testing.T) {
	d, _ := splitDispatcher(t, 1, "192.0.2.1/32")
	d.Close() // stop the goroutine; drive the shard by hand below
	s := newShard(d.shards[0].o, 0)
	d.shards[0] = s
	ts := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	m := msg(ip1, ts, "<30>1 2026-11-18T10:00:00Z dockerhost web-1 1 web-1 - hello")
	step := func() {
		d.Dispatch(m)
		s.handle(<-s.ch)
	}
	step() // opens the file and interns the tag
	if n := testing.AllocsPerRun(200, step); n != 0 {
		t.Errorf("split dispatch+handle allocates %.1f times per message", n)
	}
	s.closeAll()
}

func BenchmarkParseTag(b *testing.B) {
	for _, bc := range []struct{ name, in string }{
		{"rfc5424", "<30>1 2026-11-18T13:04:05.123456Z dockerhost compose-web-1 4242 compose-web-1 - GET / 200"},
		{"unix", "<30>Nov 18 13:04:05 compose-web-1[4242]: GET / 200"},
		{"rfc3164", "<30>Nov 18 13:04:05 dockerhost compose-web-1[4242]: GET / 200"},
	} {
		in := []byte(bc.in)
		b.Run(bc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				parseTag(in)
			}
		})
	}
}

func BenchmarkDispatch(b *testing.B) {
	payload := []byte("<30>1 2026-11-18T13:04:05.123456Z dockerhost compose-web-1 4242 compose-web-1 - GET / 200")
	for _, bc := range []struct {
		name  string
		split []string
	}{
		{"off", nil},
		{"on-other-ip", []string{"10.0.0.0/8", "127.0.0.1/32"}},
		{"on-split-ip", []string{"10.0.0.0/8", "192.0.2.1/32"}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			d, _ := splitDispatcher(b, 4, bc.split...)
			d.Close()
			for i := range d.shards { // fresh, non-running shards with a big queue
				d.shards[i] = newShard(d.shards[i].o, i)
			}
			m := Message{Addr: ip1, Data: payload}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				d.Dispatch(m)
				if i%1024 == 1023 {
					for _, s := range d.shards {
						for len(s.ch) > 0 {
							<-s.ch
						}
					}
				}
			}
		})
	}
}
