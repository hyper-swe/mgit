package mcp

import (
	"fmt"
	"strings"

	"github.com/hyper-swe/mgit/internal/model"
)

// maxArgLen bounds any single string argument. MCP inputs are untrusted even
// from a "trusted" agent (security mindset), so an oversized payload is
// rejected before it reaches a service or the store. Refs: MGIT-50
const maxArgLen = 8192

// validateTaskID enforces the MGIT-41 task-id grammar (an allowlist that
// already rejects control chars, path separators, and shell/SQL metacharacters)
// on an MCP-supplied task id. Empty is allowed only where the tool treats the
// task id as optional; callers that require it check presence first.
// Refs: MGIT-50, MGIT-41
func validateTaskID(v string) error {
	if len(v) > maxArgLen {
		return fmt.Errorf("task_id exceeds max length %d", maxArgLen)
	}
	if _, err := model.ParseTaskID(v); err != nil {
		return err
	}
	return nil
}

// validatePath rejects a hostile worktree path: empty, oversized, containing a
// control/NUL character, or a ".." traversal segment. A worktree path becomes a
// real filesystem path, so this boundary is load-bearing. Refs: MGIT-50
func validatePath(v string) error {
	if v == "" {
		return fmt.Errorf("path must not be empty")
	}
	if len(v) > maxArgLen {
		return fmt.Errorf("path exceeds max length %d", maxArgLen)
	}
	if strings.ContainsRune(v, 0) {
		return fmt.Errorf("path contains a NUL byte")
	}
	if i := strings.IndexFunc(v, isControlChar); i >= 0 {
		return fmt.Errorf("path contains a control character at byte %d", i)
	}
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return fmt.Errorf("path must not contain a %q traversal segment", "..")
		}
	}
	return nil
}

// validateText bounds a free-text argument (e.g. a commit message): oversized or
// NUL-containing input is rejected, but printable text with newlines/tabs is
// allowed. Refs: MGIT-50
func validateText(name, v string) error {
	if len(v) > maxArgLen {
		return fmt.Errorf("%s exceeds max length %d", name, maxArgLen)
	}
	if strings.ContainsRune(v, 0) {
		return fmt.Errorf("%s contains a NUL byte", name)
	}
	return nil
}

// validateToken bounds an identifier-ish argument (commit hash, config key):
// oversized, control chars, or whitespace are rejected. Refs: MGIT-50
func validateToken(name, v string) error {
	if len(v) > maxArgLen {
		return fmt.Errorf("%s exceeds max length %d", name, maxArgLen)
	}
	if i := strings.IndexFunc(v, func(r rune) bool { return isControlChar(r) || r == ' ' }); i >= 0 {
		return fmt.Errorf("%s contains an invalid character at byte %d", name, i)
	}
	return nil
}

// isControlChar reports whether r is a C0/C1 control character (NUL..US, DEL).
// Tab and newline are NOT treated as control here; free-text validators handle
// those explicitly. Refs: MGIT-50
func isControlChar(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return r < 0x20 || r == 0x7f
}
