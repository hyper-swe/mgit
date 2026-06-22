package agentadapter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// replaceMarked replaces the text between begin and end markers (inclusive)
// in body with section, or appends section (separated by a blank line) when
// no marked block is present. Used to upsert generated blocks into
// user-owned files idempotently. Refs: MGIT-11.11.2, MGIT-11.11.3
func replaceMarked(body, begin, end, section string) string {
	start := strings.Index(body, begin)
	stop := strings.Index(body, end)
	if start >= 0 && stop > start {
		stop += len(end)
		return body[:start] + section + body[stop:]
	}
	if strings.TrimSpace(body) == "" {
		return section + "\n"
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section + "\n"
}

// upsertMarkedFile reads path, replaces (or appends) the marked section,
// and writes it back, creating parent directories as needed. A path that
// is not a regular file (e.g. a directory) surfaces the read error rather
// than being clobbered. Refs: MGIT-11.11.2, MGIT-11.11.3
func upsertMarkedFile(path, begin, end, section string) error {
	existing, err := os.ReadFile(path) //nolint:gosec // worktree-owned generated file
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create dir for %s: %w", filepath.Base(path), err)
	}
	out := replaceMarked(string(existing), begin, end, section)
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil { //nolint:gosec // worktree-owned generated file
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
