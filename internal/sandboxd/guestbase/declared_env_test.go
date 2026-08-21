package guestbase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyper-swe/mgit/internal/guest"
)

// An OCI image declares its own environment in its config blob, and that is
// where a toolchain path already lives: golang images set
// PATH=/usr/local/go/bin:... there. Reading it is what makes the mechanism
// GENERAL — mgit special-cases no toolchain, it believes the image.
// Refs: MGIT-152
func TestPathFromImageConfig(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "a_golang_image",
			env:  []string{"PATH=/usr/local/go/bin:/usr/local/sbin:/usr/bin", "GOLANG_VERSION=1.23"},
			want: "/usr/local/go/bin:/usr/local/sbin:/usr/bin",
		},
		{
			name: "last_declaration_wins_as_in_any_environment",
			env:  []string{"PATH=/first", "PATH=/second"},
			want: "/second",
		},
		{
			name: "no_path_declared",
			env:  []string{"LANG=C.UTF-8"},
			want: "",
		},
		{
			name: "nothing_declared",
			env:  nil,
			want: "",
		},
		{
			name: "a_variable_merely_containing_PATH_is_not_PATH",
			env:  []string{"GOPATH=/go", "MANPATH=/usr/share/man"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pathFromImageConfig(imageConfigDoc{
				Config: imageConfigInner{Env: tt.env},
			}))
		})
	}
}

// The declaration written into the base is exactly what the guest reads back,
// so the two halves cannot drift into different formats. Refs: MGIT-152
func TestRenderDeclaredEnv_IsWhatTheGuestParses(t *testing.T) {
	assert.Equal(t, "PATH=/usr/local/go/bin:/usr/bin\n",
		renderDeclaredEnv("/usr/local/go/bin:/usr/bin"))
	assert.Empty(t, renderDeclaredEnv(""), "nothing declared writes nothing")
}

// The composer WRITES this file and the guest READS it. They are in different
// packages and different binaries — mgit composes on the host, mgit-guest
// parses inside the VM — so the shared path is pinned from both sides here. A
// silent rename would produce a base that declares a PATH nothing ever reads,
// which fails as "the toolchain is still invisible": the original bug, with a
// fix in place. Refs: MGIT-152
func TestDeclaredEnvFile_MatchesWhatTheGuestReads(t *testing.T) {
	assert.Equal(t, guest.DeclaredGuestEnvFile, declaredEnvFile,
		"the composer writes a file the guest does not read")
}

// And the bytes one writes are the bytes the other accepts. Refs: MGIT-152
func TestDeclaredEnv_RoundTripsThroughTheGuestParser(t *testing.T) {
	dir := t.TempDir()
	const declared = "/usr/local/go/bin:/usr/local/sbin:/usr/bin"
	require.NoError(t, writeDeclaredEnv(dir, declared))
	assert.Equal(t, declared, guest.DeclaredGuestPath(dir),
		"what the composer wrote is not what the guest reads back")
}
