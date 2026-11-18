package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfig(t *testing.T) {
	c, err := parseConfig(nil, io.Discard)
	if err != nil || c.Listen != ":8514" || c.Dir != "/var/log/go-syslog" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = parseConfig([]string{"-listen", "127.0.0.1:9000", "-dir", "/tmp/x", "-token-file", "/etc/t"}, io.Discard)
	if err != nil || c.Listen != "127.0.0.1:9000" || c.Dir != "/tmp/x" || c.TokenFile != "/etc/t" {
		t.Fatalf("flags: %+v %v", c, err)
	}
	for _, bad := range [][]string{{"-log-level", "loud"}, {"-max-scan-bytes", "0"}, {"-listen", ""}, {"extra"}} {
		if _, err := parseConfig(bad, io.Discard); err == nil {
			t.Errorf("%v: expected error", bad)
		}
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tok, err := readToken(""); err != nil || tok != "" {
		t.Fatalf("empty path: %q %v", tok, err)
	}
	if tok, err := readToken(write("ok", "  abc123\n")); err != nil || tok != "abc123" {
		t.Fatalf("ok: %q %v", tok, err)
	}
	for _, data := range []string{"\n", "two words"} {
		if _, err := readToken(write("bad", data)); err == nil {
			t.Errorf("%q: expected error", data)
		}
	}
	if _, err := readToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file: expected error")
	}
}
