//go:build linux

package hostmem

// procMeminfoPath is the Linux kernel's memory summary. MemTotal there is
// usable physical RAM (total minus firmware/kernel reserved), which is the
// right basis for a "do not oversubscribe the host" ceiling. It lives in the
// linux-tagged file because it is the only consumer; the parser it feeds is
// build-tag-free so it stays testable on any developer machine.
const procMeminfoPath = "/proc/meminfo"

// TotalBytes reports the host's usable physical memory in bytes, read from
// /proc/meminfo's MemTotal.
//
// MemTotal rather than sysinfo(2).totalram is deliberate: both report usable
// RAM, but /proc/meminfo is readable in every container and namespace layout
// mgit-sandboxd runs in and is stable across kernel versions. Note this is the
// HOST's memory as the kernel sees it — under a cgroup memory limit the daemon
// would over-estimate its own budget, which is why an operator confined that
// way should size the ceiling explicitly with --max-memory-mb.
// Refs: FR-17.26, MGIT-98
func TotalBytes() (uint64, error) {
	return totalBytesFromMeminfo(procMeminfoPath)
}
