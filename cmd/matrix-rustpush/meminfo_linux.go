//go:build linux

package main

import (
	"os"
	"path"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	cgroupRoot     = "/sys/fs/cgroup"
	procSelfCgroup = "/proc/self/cgroup"
)

// totalSystemMemory returns the memory ceiling to size the Go heap limit
// against: the smaller of host RAM and any cgroup memory limit, so it does the
// right thing inside a memory-capped container or systemd unit where host RAM
// is misleading. Returns 0 if it can't be determined.
func totalSystemMemory() uint64 {
	var host uint64
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err == nil {
		host = uint64(info.Totalram) * uint64(info.Unit)
	}
	selfCgroup, _ := os.ReadFile(procSelfCgroup)
	if limit := cgroupMemoryLimit(cgroupRoot, string(selfCgroup)); limit > 0 && (host == 0 || limit < host) {
		return limit
	}
	return host
}

// cgroupMemoryLimit returns the tightest memory limit that applies to this
// process, or 0 if none. It checks, under root:
//
//   - cgroup v2: memory.max of the process's own cgroup (from selfCgroup, the
//     contents of /proc/self/cgroup) and each ancestor, which covers a bare
//     host where systemd sets MemoryMax= on the unit or its slice, plus the
//     root memory.max seen inside a container's cgroup namespace;
//   - cgroup v1: memory.limit_in_bytes of the memory controller's own cgroup
//     and the controller root.
//
// "max" or a near-max sentinel means unlimited and is ignored.
func cgroupMemoryLimit(root, selfCgroup string) uint64 {
	v2Path, v1Path := parseProcSelfCgroup(selfCgroup)
	var candidates []string
	for p := v2Path; p != ""; p = parentCgroup(p) {
		candidates = append(candidates, path.Join(root, p, "memory.max"))
	}
	candidates = append(candidates, path.Join(root, "memory.max"))
	if v1Path != "" && v1Path != "/" {
		candidates = append(candidates, path.Join(root, "memory", v1Path, "memory.limit_in_bytes"))
	}
	candidates = append(candidates, path.Join(root, "memory", "memory.limit_in_bytes"))

	var tightest uint64
	for _, file := range candidates {
		if v := readCgroupLimit(file); v > 0 && (tightest == 0 || v < tightest) {
			tightest = v
		}
	}
	return tightest
}

// parseProcSelfCgroup extracts the cgroup v2 path ("0::<path>") and the v1
// memory controller path ("<n>:...memory...:<path>") from /proc/self/cgroup.
func parseProcSelfCgroup(content string) (v2Path, v1Path string) {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			v2Path = parts[2]
			continue
		}
		for _, ctrl := range strings.Split(parts[1], ",") {
			if ctrl == "memory" {
				v1Path = parts[2]
			}
		}
	}
	return v2Path, v1Path
}

// parentCgroup returns the parent of a cgroup path, or "" once past the root
// (the root itself is checked separately).
func parentCgroup(p string) string {
	p = path.Clean(p)
	if p == "/" || p == "." {
		return ""
	}
	parent := path.Dir(p)
	if parent == "/" {
		return ""
	}
	return parent
}

// readCgroupLimit parses a cgroup memory limit file. Returns 0 for missing
// files, "max", and v1's near-max "unlimited" sentinel.
func readCgroupLimit(file string) uint64 {
	const unlimitedThreshold = uint64(1) << 62
	b, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v >= unlimitedThreshold {
		return 0
	}
	return v
}
