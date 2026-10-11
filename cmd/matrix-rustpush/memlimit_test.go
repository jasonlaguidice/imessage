package main

import (
	"math"
	"runtime/debug"
	"testing"
)

func TestMemLimitFor(t *testing.T) {
	if got := memLimitFor(10 << 30); got != 8<<30 {
		t.Errorf("memLimitFor(10GiB) = %d, want %d", got, int64(8<<30))
	}
	if got := memLimitFor(math.MaxUint64); got != math.MaxInt64 {
		t.Errorf("memLimitFor(max) = %d, want MaxInt64", got)
	}
}

func TestCapMemoryLimitRespectsEnv(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	debug.SetMemoryLimit(math.MaxInt64)
	t.Setenv("GOMEMLIMIT", "off")
	capMemoryLimitFromSystem()
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Errorf("GOMEMLIMIT=off was overridden: limit=%d", got)
	}
}
