package main

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
)

// memLimitFraction is the share of detected system memory the Go heap may use
// as a soft cap (GOMEMLIMIT). The rest is headroom for memory the Go GC does
// not govern: the Rust/uniffi FFI (CloudKit attachment downloads, image and
// video buffers), the ffmpeg subprocess, and the OS itself.
const memLimitFraction = 0.80

// capMemoryLimitFromSystem sets a soft Go heap limit (GOMEMLIMIT) sized to the
// host's memory when the operator hasn't set one explicitly. Ported from
// upstream corten-matrix 38b0deaf.
//
// On a small, swap-less host an aggressive backfill can drive the resident set
// past physical memory and get the process OOM-killed in a restart loop. A
// GOMEMLIMIT makes the GC work harder as the heap nears the cap instead of
// letting it grow unbounded between collections. It is a soft, heap-only
// limit, so it complements rather than replaces more RAM or swap.
func capMemoryLimitFromSystem() {
	if gomemlimitExplicit() {
		fmt.Fprintf(os.Stderr, "[memlimit] GOMEMLIMIT set by environment (%q), leaving as-is\n", os.Getenv("GOMEMLIMIT"))
		return
	}
	total := totalSystemMemory()
	if total == 0 {
		// Unsupported platform or read failure: don't guess.
		fmt.Fprintln(os.Stderr, "[memlimit] could not detect system memory; not setting GOMEMLIMIT")
		return
	}
	limit := memLimitFor(total)
	debug.SetMemoryLimit(limit)
	fmt.Fprintf(os.Stderr, "[memlimit] system memory %d MiB → GOMEMLIMIT %d MiB (%.0f%%); override with the GOMEMLIMIT env var\n",
		total/(1<<20), limit/(1<<20), memLimitFraction*100)
}

// gomemlimitExplicit reports whether the operator already chose a limit. The
// env var is checked directly so GOMEMLIMIT=off is honoured too; the runtime
// check catches a limit applied some other way before startup.
func gomemlimitExplicit() bool {
	if _, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		return true
	}
	// SetMemoryLimit(-1) reads the current limit without changing it; the
	// unset default is math.MaxInt64.
	return debug.SetMemoryLimit(-1) != math.MaxInt64
}

// memLimitFor returns the soft heap limit for a memory ceiling of total bytes.
func memLimitFor(total uint64) int64 {
	limit := float64(total) * memLimitFraction
	if limit >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(limit)
}
