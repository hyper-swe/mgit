package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The root a daemon records and watches: --repo-root when given, else the
// repository the host root sits in, else nothing (a greet-only daemon has
// no repository to outlive). Refs: MGIT-191
func TestServedRepoRoot(t *testing.T) {
	tests := []struct {
		name     string
		repoRoot string
		hostRoot string
		want     string
	}{
		{"explicit_repo_root_wins", "/w/repo", "/elsewhere/.mgit/sandbox", "/w/repo"},
		{"derived_from_the_host_root", "", "/w/repo/.mgit/sandbox", "/w/repo"},
		{"a_host_root_of_another_shape_derives_nothing", "", "/srv/hostroot", ""},
		{"greet_only_daemon_has_no_root", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, servedRepoRoot(&daemonOpts{repoRoot: tt.repoRoot, hostRoot: tt.hostRoot}))
		})
	}
}
