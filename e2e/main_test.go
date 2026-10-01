package e2e

import (
	"fmt"
	"os"
	"testing"

	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// TestMain gives every mgit these tests run a fixed git identity. The
// subprocesses inherit this environment, and `mgit squash --to-git` refuses
// with no identity configured (MGIT-237, option C), so without it the suite
// would pass or fail by whether the machine running it has one: a
// developer's laptop does, a hosted CI runner does not. A test that needs no
// identity unsets these itself. Refs: MGIT-237
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME":  "mgit e2e",
		"GIT_AUTHOR_EMAIL": "e2e@example.invalid",
	} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "hermetic test setup: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// e2ePatchAuthor is the fixed identity the in-process service fixtures author
// exported patches with, as the CLI's own wiring would from git config.
// Refs: MGIT-237
func e2ePatchAuthor() (gitstore.AuthorIdentity, error) {
	return gitstore.AuthorIdentity{Name: "mgit e2e", Email: "e2e@example.invalid"}, nil
}
