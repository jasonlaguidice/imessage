//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCgroupFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCgroupMemoryLimit(t *testing.T) {
	const gib = uint64(1) << 30

	t.Run("v2 container root", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, "memory.max", "2147483648")
		if got := cgroupMemoryLimit(root, "0::/\n"); got != 2*gib {
			t.Errorf("got %d, want %d", got, 2*gib)
		}
	})

	t.Run("v2 unlimited", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, "memory.max", "max")
		if got := cgroupMemoryLimit(root, "0::/\n"); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})

	t.Run("v2 systemd unit and slice, tightest wins", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, "system.slice/memory.max", "8589934592")
		writeCgroupFile(t, root, "system.slice/bridge.service/memory.max", "3221225472")
		self := "0::/system.slice/bridge.service\n"
		if got := cgroupMemoryLimit(root, self); got != 3*gib {
			t.Errorf("got %d, want %d", got, 3*gib)
		}
	})

	t.Run("v2 unit unlimited, slice limited", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, "system.slice/memory.max", "4294967296")
		writeCgroupFile(t, root, "system.slice/bridge.service/memory.max", "max")
		if got := cgroupMemoryLimit(root, "0::/system.slice/bridge.service"); got != 4*gib {
			t.Errorf("got %d, want %d", got, 4*gib)
		}
	})

	t.Run("v1 root and own cgroup", func(t *testing.T) {
		root := t.TempDir()
		writeCgroupFile(t, root, "memory/memory.limit_in_bytes", "9223372036854771712") // unlimited sentinel
		writeCgroupFile(t, root, "memory/docker/abc/memory.limit_in_bytes", "1073741824")
		self := "12:cpu,cpuacct:/docker/abc\n11:memory:/docker/abc\n"
		if got := cgroupMemoryLimit(root, self); got != gib {
			t.Errorf("got %d, want %d", got, gib)
		}
	})

	t.Run("no cgroup files", func(t *testing.T) {
		if got := cgroupMemoryLimit(t.TempDir(), ""); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
}

func TestParseProcSelfCgroup(t *testing.T) {
	v2, v1 := parseProcSelfCgroup("0::/user.slice/session-1.scope\n")
	if v2 != "/user.slice/session-1.scope" || v1 != "" {
		t.Errorf("v2-only: got (%q, %q)", v2, v1)
	}
	v2, v1 = parseProcSelfCgroup("4:memory:/docker/x\n1:name=systemd:/docker/x\n0::/docker/x\n")
	if v2 != "/docker/x" || v1 != "/docker/x" {
		t.Errorf("hybrid: got (%q, %q)", v2, v1)
	}
}

func TestTotalSystemMemoryDetectsSomething(t *testing.T) {
	if totalSystemMemory() == 0 {
		t.Error("expected non-zero memory on linux")
	}
}
