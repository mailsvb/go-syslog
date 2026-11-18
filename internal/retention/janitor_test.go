package retention

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mailsvb/go-syslog/internal/stats"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestExpired(t *testing.T) {
	cutoff := time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		want bool
	}{
		{"2026-11-11_11.log", true},  // ended 12:00 < cutoff
		{"2026-11-11_12.log", false}, // ends 13:00, still within retention
		{"2026-11-01_00.log", true},
		{"2026-11-18_00.log", false},
		{"notes.txt", false},
		{"2026-11-01_00.log.gz", false},
		{"garbage.log", false},
	}
	for _, tt := range tests {
		if got := Expired(tt.name, cutoff); got != tt.want {
			t.Errorf("Expired(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRunOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC)
	old1 := filepath.Join(dir, "10.0.0.1", "2026-11-10_08.log")
	old2 := filepath.Join(dir, "10.0.0.2", "2026-11-11_11.log")
	keep := filepath.Join(dir, "10.0.0.1", "2026-11-18_12.log")
	edge := filepath.Join(dir, "10.0.0.3", "2026-11-11_12.log")
	other := filepath.Join(dir, "10.0.0.4", "README")
	for _, p := range []string{old1, old2, keep, edge, other} {
		touch(t, p)
	}

	st := &stats.Stats{}
	j := &Janitor{Dir: dir, Retention: 7 * 24 * time.Hour, Now: func() time.Time { return now }, Stats: st}
	n, err := j.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || st.FilesDeleted.Load() != 2 {
		t.Fatalf("deleted %d (stats %d), want 2", n, st.FilesDeleted.Load())
	}
	if exists(old1) || exists(old2) {
		t.Error("expired files still exist")
	}
	if !exists(keep) || !exists(edge) || !exists(other) {
		t.Error("files within retention or unknown files were deleted")
	}
	if exists(filepath.Join(dir, "10.0.0.2")) {
		t.Error("empty host dir not removed")
	}
	if !exists(filepath.Join(dir, "10.0.0.1")) {
		t.Error("non-empty host dir removed")
	}
}

func TestRunOnceMissingDir(t *testing.T) {
	j := &Janitor{Dir: filepath.Join(t.TempDir(), "nope"), Retention: time.Hour}
	if n, err := j.RunOnce(); n != 0 || err != nil {
		t.Fatalf("got %d, %v", n, err)
	}
}
