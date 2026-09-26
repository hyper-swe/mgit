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

// The guest-exec PRIMITIVES — the raw calls that run a command inside a guest —
// have exactly one funnel per path, and this test derives that from the code
// (not a hand-named list) so a NEW function that reaches a primitive directly
// fails rather than drifting in ungoverned. The primitives are:
//
//   - execUntilTheGuestAnswers: reached only by the client path (Manager.Exec,
//     which runs decideExecIdentity first) and the daemon's own settle funnel
//     (defaultSettler wires execSettler through it, and execSettler.run refuses
//     an unregistered program and sets the audited identity);
//   - execOnce: reached only by execUntilTheGuestAnswers and by the readiness
//     probe (awaitGuestServing), the one no-exec site.
//
// A new caller of either primitive — the exact bypass a registry of named
// constants cannot see — makes this test fail, naming the offending function.
// Refs: MGIT-272
func TestInternalExec_OnlyKnownFunctionsReachTheGuestExecPrimitives(t *testing.T) {
	allowed := map[string]map[string]bool{
		"execUntilTheGuestAnswers": {"Exec": true, "defaultSettler": true},
		"execOnce":                 {"execUntilTheGuestAnswers": true, "awaitGuestServing": true},
	}
	seen := map[string]map[string]bool{
		"execUntilTheGuestAnswers": {},
		"execOnce":                 {},
	}

	fset := token.NewFileSet()
	goFiles, err := filepath.Glob("*.go")
	require.NoError(t, err)
	parsedAny := false
	for _, path := range goFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: package-local source, test-only
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		parsedAny = true
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			enclosing := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if _, tracked := seen[sel.Sel.Name]; tracked {
					seen[sel.Sel.Name][enclosing] = true
				}
				return true
			})
		}
	}
	require.True(t, parsedAny, "parsed the package's source")

	for prim, callers := range seen {
		for caller := range callers {
			assert.Truef(t, allowed[prim][caller],
				"%s reaches the guest-exec primitive %s — only %v may (MGIT-272); "+
					"route a new internal exec through the settle funnel (execSettler.run, which "+
					"refuses an unregistered program) or register it and pin its caller here",
				caller, prim, sortedKeys(allowed[prim]))
		}
		// The known funnels must still be present, so a rename that moves a
		// primitive out of its funnel is noticed rather than silently passing.
		for caller := range allowed[prim] {
			assert.Truef(t, callers[caller], "expected %s to reach %s; the funnel moved", caller, prim)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
