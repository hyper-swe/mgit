package microvm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The guest-exec chain is pinned from the code itself, funnel by funnel, down
// to its LOWEST rung — the exec-frame encoder that every guest exec must call,
// so there is no lower primitive to bypass to (a hand-built frame without the
// encoder is below what a caller pin can see, the accepted floor):
//
//	execwire.WriteRequest    <- guestexec.Run            (the exec-frame encoder)
//	guestexec.Run            <- execOnce                 (dial + encode + read)
//	execOnce                 <- execUntilTheGuestAnswers, awaitGuestServing (the probe)
//	execUntilTheGuestAnswers <- Manager.Exec (client), defaultSettler (settle wiring)
//
// The settle funnel (execSettler.run — refuses an unregistered program, sets
// the audited identity) is wired through execUntilTheGuestAnswers, so it is the
// only internal exec path.
//
// Three properties make the pin durable against varied bypass shapes:
//   - SCOPE: the scan covers the whole internal/sandboxd subtree, so a new exec
//     in ANY backend (not just this package) is in scope;
//   - ALIASES: a package-qualified primitive is matched by the import's PATH,
//     not the local name, so `import ge "…/guestexec"; ge.Run(…)` does not
//     escape;
//   - FORMS: every declaration is scanned — function bodies and top-level
//     declarations — so a package-level method value `var x =
//     (*Manager).execOnce` is a reference too.
//
// A reference outside the allowed callers fails, named; each funnel must remain
// present, so a rename that moves a rung is noticed. Refs: MGIT-272
func TestInternalExec_OnlyKnownFunctionsReachTheGuestExecChain(t *testing.T) {
	const topLevel = "<package-level>"
	allowed := map[string]map[string]bool{
		"execwire.WriteRequest":    {"Run": true},
		"guestexec.Run":            {"execOnce": true},
		"execOnce":                 {"execUntilTheGuestAnswers": true, "awaitGuestServing": true},
		"execUntilTheGuestAnswers": {"Exec": true, "defaultSettler": true},
	}
	seen := map[string]map[string]bool{}
	for prim := range allowed {
		seen[prim] = map[string]bool{}
	}

	// The WHOLE module — every host-side package that could import the exec
	// packages and call a primitive (a backend, the daemon main under cmd/, a
	// helper). "../../../.." is the repo root from this package.
	repoRoot := filepath.Join("..", "..", "..", "..")
	fset := token.NewFileSet()
	filesScanned := 0
	var dotImports []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: source in the module tree, test-only
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		filesScanned++
		imports := importPaths(file)
		// A dot-import of an exec package would make its funcs unqualified
		// idents (Run, WriteRequest) that no selector-based pin can see. Ban
		// it outright — nothing legitimately dot-imports these — so every
		// reference stays qualified and path-resolvable.
		if dotImportsExecPackage(file) {
			dotImports = append(dotImports, path)
		}
		recordExecReferences(file, imports, topLevel, seen)
		return nil
	})
	require.NoError(t, err)
	require.Positive(t, filesScanned, "scanned the module")
	assert.Empty(t, dotImports, "no file may dot-import the exec packages (guestexec/execwire) — it hides Run/WriteRequest from the caller pin (MGIT-272)")
	// Scope guard: the encoder's known caller and the settle funnel must have
	// been seen, or the scan missed the packages it is meant to cover.
	require.True(t, seen["execwire.WriteRequest"]["Run"], "scanned guestexec (the encoder's caller)")
	require.True(t, seen["execUntilTheGuestAnswers"]["defaultSettler"], "scanned the settle wiring")

	for prim, callers := range seen {
		for caller := range callers {
			assert.Truef(t, allowed[prim][caller],
				"%s reaches the guest-exec primitive %s — only %v may (MGIT-272); route a new "+
					"internal exec through the settle funnel (execSettler.run, which refuses an "+
					"unregistered program) or register it and pin its caller here",
				caller, prim, sortedKeys(allowed[prim]))
		}
		for caller := range allowed[prim] {
			assert.Truef(t, callers[caller], "expected %s to reach %s; the funnel moved", caller, prim)
		}
	}
}

// dotImportsExecPackage reports whether the file dot-imports guestexec or
// execwire (import name "."), which would expose their funcs as unqualified
// identifiers a selector-based pin cannot see.
func dotImportsExecPackage(file *ast.File) bool {
	for _, imp := range file.Imports {
		if imp.Name == nil || imp.Name.Name != "." {
			continue
		}
		p := strings.Trim(imp.Path.Value, `"`)
		if strings.HasSuffix(p, "internal/sandboxd/guestexec") || strings.HasSuffix(p, "internal/execwire") {
			return true
		}
	}
	return false
}

// importPaths maps each file-local import name (its alias, or the package's
// base name) to the import path, so a primitive is matched by path not by the
// local identifier an alias could change.
func importPaths(file *ast.File) map[string]string {
	m := map[string]string{}
	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		m[name] = p
	}
	return m
}

// recordExecReferences records, per enclosing declaration, which guest-exec
// primitives it references. Package-qualified primitives are matched by import
// path; the two Manager methods by selector name (which also catches a method
// value like (*Manager).execOnce).
func recordExecReferences(file *ast.File, imports map[string]string, topLevel string, seen map[string]map[string]bool) {
	visit := func(enclosing string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if prim := execPrimitive(sel, imports); prim != "" {
				if _, tracked := seen[prim]; tracked {
					seen[prim][enclosing] = true
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			visit(d.Name.Name, d)
		case *ast.GenDecl:
			visit(topLevel, d)
		}
	}
}

func execPrimitive(sel *ast.SelectorExpr, imports map[string]string) string {
	switch sel.Sel.Name {
	case "execUntilTheGuestAnswers", "execOnce":
		return sel.Sel.Name // Manager methods: match by name (also catches a method value)
	case "WriteRequest":
		if pkgHasSuffix(sel, imports, "internal/execwire") {
			return "execwire.WriteRequest"
		}
	case "Run":
		if pkgHasSuffix(sel, imports, "internal/sandboxd/guestexec") {
			return "guestexec.Run"
		}
	}
	return ""
}

// pkgHasSuffix reports whether sel is a call on an imported package whose path
// ends with suffix, resolving the local name (or alias) to its import path.
func pkgHasSuffix(sel *ast.SelectorExpr, imports map[string]string, suffix string) bool {
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return strings.HasSuffix(imports[id.Name], suffix)
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
