package gitref

import "path/filepath"

// CommonExcludePath locates the project's shared info/exclude without reading
// HEAD or changing Git state. Linked worktrees keep this file in the common
// directory, not beside their per-worktree HEAD. Refs: MGIT-295.
func CommonExcludePath(projectRoot string) (string, error) {
	gitDir, err := resolveGitDir(projectRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir(gitDir), "info", "exclude"), nil
}
