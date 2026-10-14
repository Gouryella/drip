package tuning

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestApplyRespectsRuntimeOverrides(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(oldProcs)
	oldGC := debug.SetGCPercent(123)
	defer debug.SetGCPercent(oldGC)
	oldLimit := debug.SetMemoryLimit(128 << 20)
	defer debug.SetMemoryLimit(oldLimit)
	t.Setenv("GOGC", "123")
	t.Setenv("GOMEMLIMIT", "128MiB")
	Apply(Config{GCPercent: 200, MemoryLimit: 1 << 30})
	if got := runtime.GOMAXPROCS(0); got != 2 {
		t.Fatalf("GOMAXPROCS = %d", got)
	}
	if got := debug.SetMemoryLimit(-1); got != 128<<20 {
		t.Fatalf("memory limit = %d", got)
	}
	if got := debug.SetGCPercent(123); got != 123 {
		t.Fatalf("GOGC = %d", got)
	}
}

func TestContainerMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  uint64
	}{
		{"max\n", 8 << 30}, {"9223372036854771712\n", 8 << 30}, {"134217728\n", 128 << 20}, {"invalid", 8 << 30},
	} {
		path := filepath.Join(t.TempDir(), "memory.max")
		if err := os.WriteFile(path, []byte(tc.value), 0600); err != nil {
			t.Fatal(err)
		}
		if got := constrainMemory(8<<30, path); got != tc.want {
			t.Errorf("limit %q => %d, want %d", tc.value, got, tc.want)
		}
	}
}
