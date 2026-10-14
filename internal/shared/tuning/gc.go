package tuning

import (
	"math"
	"os"
	"runtime/debug"
)

type Mode int

const (
	ModeClient Mode = iota
	ModeServer
)

type Config struct {
	GCPercent   int
	MemoryLimit int64
}

func safeUint64ToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func DefaultClientConfig() Config {
	total := safeUint64ToInt64(getSystemTotalMemory())
	limit := total / 4
	if limit <= 0 {
		limit = 64 * 1024 * 1024
	}
	return Config{
		GCPercent:   100,
		MemoryLimit: limit,
	}
}

func DefaultServerConfig() Config {
	total := safeUint64ToInt64(getSystemTotalMemory())
	limit := total / 4 * 3
	if limit <= 0 {
		limit = 128 * 1024 * 1024
	}
	return Config{
		GCPercent:   200,
		MemoryLimit: limit,
	}
}

func Apply(cfg Config) {
	// Leave GOMAXPROCS to Go's container-aware default or the operator.
	if cfg.GCPercent > 0 && os.Getenv("GOGC") == "" {
		debug.SetGCPercent(cfg.GCPercent)
	}
	if cfg.MemoryLimit > 0 && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(cfg.MemoryLimit)
	}
}

func ApplyMode(mode Mode) {
	switch mode {
	case ModeClient:
		Apply(DefaultClientConfig())
	case ModeServer:
		Apply(DefaultServerConfig())
	}
}
