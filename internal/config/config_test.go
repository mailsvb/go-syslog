package config

import (
	"io"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":514" || c.Dir != "/var/log/go-syslog" || c.Retention != 7*24*time.Hour {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestParseFlags(t *testing.T) {
	c, err := Parse([]string{"-listen", "127.0.0.1:5514", "-dir", "/tmp/x", "-retention", "48h", "-writers", "2", "-queue-size", "10"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:5514" || c.Dir != "/tmp/x" || c.Retention != 48*time.Hour || c.Writers != 2 {
		t.Fatalf("flags not applied: %+v", c)
	}
	if c.QueuePerWriter() != 1024 {
		t.Fatalf("QueuePerWriter = %d, want minimum 1024", c.QueuePerWriter())
	}
}

func TestParseInvalid(t *testing.T) {
	for _, args := range [][]string{
		{"-readers", "0"},
		{"-retention", "0s"},
		{"-log-level", "loud"},
		{"-dir", ""},
		{"extra"},
	} {
		if _, err := Parse(args, io.Discard); err == nil {
			t.Errorf("Parse(%v) succeeded, want error", args)
		}
	}
}
