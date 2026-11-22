package reviewlaunch

import (
	"os"
	"strings"
)

// YamaScope reports the kernel's ptrace_scope as text, or "unknown" when the
// file is absent or unreadable. A receipt records it: the supervisor is also
// protected by being non-dumpable, but the scope is part of what a reader
// needs to judge the isolation.
func YamaScope() string {
	raw, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	if err != nil {
		return "unknown"
	}
	if v := strings.TrimSpace(string(raw)); v != "" {
		return v
	}
	return "unknown"
}
