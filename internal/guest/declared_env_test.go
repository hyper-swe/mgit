package guest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A base image declares where its toolchain lives; mgit should believe it.
//
// The guest's PATH was a fixed distro list, so a base that installs a
// toolchain anywhere else — golang images put Go at /usr/local/go/bin, which
// is exactly the reported case — had it invisible. `mgit run -- go build`
// failed with "go: command not found" inside a sandbox that did contain Go,
// and every command became `export PATH=... && ...`.
//
// The declaration is read from a file inside the COMPOSED TREE, so the base's
// own content digest covers it: tampering with the declared PATH is exactly as
// detectable as tampering with a binary. Refs: MGIT-152
func TestDeclaredGuestPath_ReadFromTheBase(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		explain string
	}{
		{
			name:    "a_toolchain_path_the_image_declared",
			content: "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n",
			want:    "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		},
		{
			name:    "trailing_whitespace_is_tolerated",
			content: "PATH=/usr/local/go/bin:/usr/bin  \n\n",
			want:    "/usr/local/go/bin:/usr/bin",
		},
		{
			name:    "no_file_means_no_declaration",
			content: "",
			want:    "",
			explain: "a base that declares nothing keeps the built-in default",
		},
		{
			name:    "a_relative_entry_is_refused_whole",
			content: "PATH=/usr/bin:relative/dir\n",
			want:    "",
			explain: "a relative PATH entry resolves against the process cwd, so a guest that " +
				"chdirs could shadow any binary; the declaration is refused rather than filtered",
		},
		{
			name:    "an_empty_entry_is_refused_whole",
			content: "PATH=/usr/bin::/bin\n",
			want:    "",
			explain: "an empty element means cwd in POSIX PATH semantics — the same hazard",
		},
		{
			name:    "a_lone_dot_is_refused",
			content: "PATH=.:/usr/bin\n",
			want:    "",
		},
		{
			name:    "other_variables_are_never_honored",
			content: "LD_PRELOAD=/tmp/evil.so\nPATH=/usr/bin\n",
			want:    "/usr/bin",
			explain: "only PATH is taken from an untrusted image; nothing else",
		},
		{
			name:    "an_absurdly_long_declaration_is_refused",
			content: "PATH=" + strings.Repeat("/a", 5000) + "\n",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.content != "" {
				dir := filepath.Join(root, "etc", "mgit")
				require.NoError(t, os.MkdirAll(dir, 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "guest-env"), []byte(tt.content), 0o600))
			}
			got := DeclaredGuestPath(root)
			assert.Equal(t, tt.want, got, tt.explain)
		})
	}
}

// The declaration reaches the environment a command actually runs in.
// Refs: MGIT-152
func TestBaseEnvWithDeclaredPath_OverridesTheDefaultPath(t *testing.T) {
	base := defaultBaseEnv()
	withPath := baseEnvWith(base, "/usr/local/go/bin:/usr/bin")

	assert.Equal(t, "/usr/local/go/bin:/usr/bin", envValue(withPath, "PATH"),
		"the declared PATH must win over the built-in default")
	assert.Equal(t, "/work", envValue(withPath, "HOME"),
		"every other base variable is untouched")

	// An empty declaration changes nothing.
	assert.Equal(t, envValue(base, "PATH"), envValue(baseEnvWith(base, ""), "PATH"))
}
