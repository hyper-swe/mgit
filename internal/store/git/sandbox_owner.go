package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// sandboxOwnerName is the file inside a sandbox worktree's .mgit/ directory
// that names the repository whose daemon registered the sandbox for it.
//
// `mgit sandbox launch --worktree <dir>` decorates <dir> with agent files under
// <dir>/.mgit, after which <dir> looked like a repository of its own to every
// verb run from inside it — its own daemon, an empty registry, and
// status/run/doctor answering three different things about one sandbox
// (MGIT-196, FEAT-7.28). The owner file points those verbs back at the daemon
// that holds the binding. A linked worktree needs none: its marker already
// names the shared store. Refs: MGIT-196
const sandboxOwnerName = "sandbox-owner"

// SandboxOwner records which repository registered a sandbox for a worktree
// directory, and for which task. Refs: MGIT-196
type SandboxOwner struct {
	RepoRoot string `json:"repo_root"` // root of the owning repository, as its daemon is keyed
	Task     string `json:"task"`
}

// WriteSandboxOwner writes worktreeRoot/.mgit/sandbox-owner, creating the
// .mgit directory if needed (it also holds the agent shims). Refs: MGIT-196
func WriteSandboxOwner(worktreeRoot string, o SandboxOwner) error {
	if o.RepoRoot == "" {
		return fmt.Errorf("sandbox owner: write %s: missing repo_root", worktreeRoot)
	}
	dir := filepath.Join(worktreeRoot, mgitDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("sandbox owner: mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("sandbox owner: marshal: %w", err)
	}
	path := filepath.Join(dir, sandboxOwnerName)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("sandbox owner: write %s: %w", path, err)
	}
	return nil
}

// ReadSandboxOwner reads worktreeRoot/.mgit/sandbox-owner. The bool is false
// (with a nil error) when no owner file exists; a present-but-corrupt file is
// a hard error, never silence. Refs: MGIT-196
func ReadSandboxOwner(worktreeRoot string) (*SandboxOwner, bool, error) {
	path := filepath.Join(worktreeRoot, mgitDirName, sandboxOwnerName)
	data, err := os.ReadFile(path) //nolint:gosec // mgit-internal path under .mgit
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read sandbox owner: %w", err)
	}
	var o SandboxOwner
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, false, fmt.Errorf("parse sandbox owner %s: %w", path, err)
	}
	if o.RepoRoot == "" {
		return nil, false, fmt.Errorf("invalid sandbox owner %s: missing repo_root", path)
	}
	return &o, true, nil
}

// StorePresent reports whether root holds a repository's store — a .mgit
// carrying the HEAD that Init writes — as opposed to the .mgit a linked
// worktree (marker, shims) or a sandbox launch (shims, owner file) leaves
// behind. It answers only "is there a store here", never "is it healthy":
// Open is the authority on that. Refs: MGIT-196
func StorePresent(root string) bool {
	fi, err := os.Stat(filepath.Join(root, mgitDirName, "HEAD"))
	return err == nil && !fi.IsDir()
}

// LaunchDecorated reports whether root's .mgit carries what `mgit sandbox
// launch --worktree` (and `mgit work`) write into a worktree — the generated
// file list or the agent shims — which is what made a plain directory look
// like a repository of its own (MGIT-196). It is the evidence the store-less
// refusal cites, so the refusal fires only where that evidence exists.
// Refs: MGIT-196
func LaunchDecorated(root string) bool {
	for _, name := range []string{"generated", "shims"} {
		if _, err := os.Stat(filepath.Join(root, mgitDirName, name)); err == nil {
			return true
		}
	}
	return false
}

// ClearSandboxOwner removes worktreeRoot/.mgit/sandbox-owner when it names
// task — the record `mgit sandbox remove` retires with the binding. An owner
// file naming another task, or none, is left alone. Refs: MGIT-196
func ClearSandboxOwner(worktreeRoot, task string) error {
	owner, ok, err := ReadSandboxOwner(worktreeRoot)
	if err != nil || !ok || owner.Task != task {
		return err
	}
	path := filepath.Join(worktreeRoot, mgitDirName, sandboxOwnerName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sandbox owner: remove %s: %w", path, err)
	}
	return nil
}
