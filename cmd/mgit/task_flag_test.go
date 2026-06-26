package main

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findSubCmd returns the named immediate subcommand or fails the test.
func findSubCmd(t *testing.T, parent *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("subcommand %q not found under %q", name, parent.Name())
	return nil
}

// buildCmd constructs each representative command the same way main wires it.
func buildCmd(t *testing.T, which string) *cobra.Command {
	t.Helper()
	switch which {
	case "commit":
		return commitCmd()
	case "squash":
		return squashCmd()
	case "work":
		return newWorkCmd(func(context.Context, *App, workOptions) error { return nil })
	case "worktree add":
		return findSubCmd(t, worktreeCmd(), "add")
	default:
		t.Fatalf("unknown command %q", which)
		return nil
	}
}

// TestTaskIDFlag_BothSpellings_ResolveToSameValue asserts every representative
// command accepts both `--task-id` and the back-compat `--task` alias, that
// both bind the same target, and that the alias stays hidden from help.
// Refs: MGIT-37
func TestTaskIDFlag_BothSpellings_ResolveToSameValue(t *testing.T) {
	commands := []string{"commit", "squash", "work", "worktree add"}
	for _, name := range commands {
		t.Run(name, func(t *testing.T) {
			// Canonical and alias flags must both exist on the command.
			cmd := buildCmd(t, name)
			canonical := cmd.Flags().Lookup("task-id")
			alias := cmd.Flags().Lookup("task")
			require.NotNil(t, canonical, "%s: --task-id must be registered", name)
			require.NotNil(t, alias, "%s: --task alias must be registered", name)

			// Both spellings must point at the identical target variable.
			assert.Same(t, canonical.Value, alias.Value,
				"%s: --task-id and --task must share one target", name)

			// The alias must be hidden; canonical must be visible.
			assert.True(t, alias.Hidden, "%s: --task alias must be hidden", name)
			assert.False(t, canonical.Hidden, "%s: --task-id must be visible", name)

			// Setting via the canonical spelling resolves on the shared value.
			require.NoError(t, cmd.Flags().Set("task-id", "MGIT-37"))
			assert.Equal(t, "MGIT-37", canonical.Value.String(),
				"%s: --task-id should resolve", name)

			// Setting via the alias resolves to the same shared value.
			fresh := buildCmd(t, name)
			require.NoError(t, fresh.Flags().Set("task", "MGIT-37"))
			assert.Equal(t, "MGIT-37", fresh.Flags().Lookup("task-id").Value.String(),
				"%s: --task alias should resolve onto --task-id target", name)
		})
	}
}

// TestTaskIDFlag_Alias_NotInVisibleHelp asserts the hidden `--task` alias does
// not appear in the command's rendered (non-hidden) usage text, while the
// canonical `--task-id` does.
// Refs: MGIT-37
func TestTaskIDFlag_Alias_NotInVisibleHelp(t *testing.T) {
	commands := []string{"commit", "squash", "work", "worktree add"}
	for _, name := range commands {
		t.Run(name, func(t *testing.T) {
			cmd := buildCmd(t, name)
			usage := cmd.Flags().FlagUsages()
			assert.Contains(t, usage, "--task-id",
				"%s: canonical --task-id must show in help", name)
			assert.NotContains(t, usage, "--task ",
				"%s: hidden --task alias must not show in help", name)
		})
	}
}
