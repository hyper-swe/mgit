package packaging

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// An agent cannot hold an interactive session inside a libkrun guest: exec
// gives the guest no stdin or TTY and `sandbox shell` is refused. The docs say
// so and name the alternative, rather than listing the verb as if it worked.
// Refs: MGIT-320, MGIT-321

func TestDocs_InteractiveSessionsInALibkrunGuestAreUnsupportedWithTheAlternative(t *testing.T) {
	doc := flatten(readRepoFile(t, "docs/INSTALL-SANDBOX.md"))
	for _, want := range []string{
		"interactive sessions in a guest are unsupported on libkrun",
		"mgit work --sandbox",
		"KVM guest PTY transport",
	} {
		assert.Contains(t, doc, want, "INSTALL-SANDBOX.md must say %q", want)
	}
	readme := flatten(readRepoFile(t, "README.md"))
	assert.Contains(t, readme, "unsupported on libkrun", "the shell row in the README must carry the limit")
}

func TestDocs_AgentsStayOnTheHostByDesign(t *testing.T) {
	doc := flatten(readRepoFile(t, "docs/INSTALL-SANDBOX.md"))
	assert.Contains(t, doc, "agents stay on the host by design",
		"the docs must answer where an agent runs: the guest has no inbox, no credentials and no way to reach the host")
}

// flatten collapses line wrapping so a sentence is found wherever it breaks.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }
