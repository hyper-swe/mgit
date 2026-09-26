package microvm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guest-exec chain is pinned from the code itself — funnel by funnel down
// to the LOWEST primitive — so a new reference that reaches any rung fails
// rather than drifting in ungoverned. There is no lower rung to bypass to:
// no guest exec happens without encoding a request frame, and that encoder
// (execwire.WriteRequest) has exactly one caller.
//
//	execwire.WriteRequest   ← guestexec.Run            (the exec-frame encoder)
//	guestexec.Run           ← execOnce                 (dial + encode + read)
//	execOnce                ← execUntilTheGuestAnswers, awaitGuestServing (the probe)
//	execUntilTheGuestAnswers← Manager.Exec (client), defaultSettler (settle wiring)
//	GuestDialer.DialGuest   ← dialGuestReady           (the one exec-path dial)
//
// The settle funnel (execSettler.run, which refuses an unregistered program
// and sets the audited identity) is wired through execUntilTheGuestAnswers, so
// it is the only internal exec path. The scan covers this package AND
// guestexec (where the encoder is called), and every declaration form —
// function bodies and top-level declarations, so a package-level method value
// like `var x = (*Manager).execOnce` is a reference too. A caller outside the
// allowed set fails, named; and each allowed funnel must remain present, so a
// rename that moves a rung out of its funnel is noticed. Refs: MGIT-272
func TestInternalExec_OnlyKnownFunctionsReachTheGuestExecChain(t *testing.T) {
	const topLevel = "<package-level>"
	allowed := map[string]map[string]bool{
		"execwire.WriteRequest":    {"Run": true},
		"guestexec.Run":            {"execOnce": true},
		"execOnce":                 {"execUntilTheGuestAnswers": true, "awaitGuestServing": true},
		"execUntilTheGuestAnswers": {"Exec": true, "defaultSettler": true},
		"DialGuest":                {"dialGuestReady": true},
	}
	seen := map[string]map[string]bool{}
	for prim := range allowed {
		seen[prim] = map[string]bool{}
	}

	// This package (where a bypass would most likely be added) and guestexec
	// (where the encoder is called). A missing guestexec means the layout
	// moved — fail loudly rather than skip the encoder's pin.
	var files []string
	for _, glob := range []string{"*.go", filepath.Join("..", "..", "guestexec", "*.go")} {
		matches, err := filepath.Glob(glob)
		require.NoError(t, err)
		files = append(files, matches...)
	}
	require.NotEmpty(t, files)
	sawGuestexec := false

	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if strings.Contains(path, "guestexec") {
			sawGuestexec = true
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: source in the module tree, test-only
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)

		record := func(enclosing string, node ast.Node) {
			ast.Inspect(node, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if prim := execPrimitive(sel); prim != "" {
						seen[prim][enclosing] = true
					}
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				record(d.Name.Name, d)
			case *ast.GenDecl:
				record(topLevel, d)
			}
		}
	}
	require.True(t, sawGuestexec, "scanned the guestexec package (the encoder's caller)")

	for prim, callers := range seen {
		for caller := range callers {
			assert.Truef(t, allowed[prim][caller],
				"%s reaches the guest-exec primitive %s — only %v may (MGIT-272); "+
					"route a new internal exec through the settle funnel (execSettler.run, which "+
					"refuses an unregistered program) or register it and pin its caller here",
				caller, prim, sortedKeys(allowed[prim]))
		}
		for caller := range allowed[prim] {
			assert.Truef(t, callers[caller], "expected %s to reach %s; the funnel moved", caller, prim)
		}
	}
}

// execPrimitive returns the guest-exec primitive a selector references, or "".
// The package-qualified encoder and cross-package Run are matched by their
// qualifier so an unrelated .Run or .WriteRequest (e.g. controlproto's) is not
// tracked; the two Manager methods and DialGuest by selector name, which also
// catches a method value like (*Manager).execOnce.
func execPrimitive(sel *ast.SelectorExpr) string {
	name := sel.Sel.Name
	qualifier := ""
	if id, ok := sel.X.(*ast.Ident); ok {
		qualifier = id.Name
	}
	switch {
	case name == "WriteRequest" && qualifier == "execwire":
		return "execwire.WriteRequest"
	case name == "Run" && qualifier == "guestexec":
		return "guestexec.Run"
	case name == "execUntilTheGuestAnswers", name == "execOnce", name == "DialGuest":
		return name
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
