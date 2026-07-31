package libkrun

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The libkrun networking prerequisite is enforced at build time and at
// runtime, but a user hitting it needs to find the remedy in the docs — and
// docs rot silently. This pins that the prerequisite and its verification
// command are actually present, so deleting them fails a test rather than
// only being noticed by whoever's sandbox stops working. Refs: MGIT-61.14
func TestDocs_StateTheLibkrunNetworkingPrerequisite(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))

	tests := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "install_guide",
			path: "docs/INSTALL-SANDBOX.md",
			want: []string{"NET=1", "krun_add_net_unixgram", "TSI"},
		},
		{
			name: "brew_caveat",
			path: "brew/mgit.rb",
			want: []string{"NET=1", "krun_add_net_unixgram"},
		},
		{
			name: "release_checklist",
			path: "docs/release/RELEASE-CHECKLIST.md",
			want: []string{"krun_add_net_unixgram"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, tt.path)) //nolint:gosec // repo-relative doc path
			if err != nil {
				t.Fatalf("read %s: %v", tt.path, err)
			}
			for _, want := range tt.want {
				if !strings.Contains(string(b), want) {
					t.Errorf("%s no longer documents %q — a user who hits the "+
						"capability refusal has nowhere to look for the fix", tt.path, want)
				}
			}
		})
	}
}
