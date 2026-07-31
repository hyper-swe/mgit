package libkrun

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The ADR-010 guardrail — every libkrun VM has an explicit host-backed NIC,
// because a VM without one boots on TSI and leaks host egress — is enforced
// by funneling context creation through newGuestCtx, which attaches the NIC
// before it returns the handle.
//
// Outside this package that funnel is absolute (nothing is exported). Inside
// it, Go's encapsulation floor is the package, so the funnel is only as good
// as "newGuestCtx is the sole place that mints a context". These tests pin
// that structurally instead of leaving it to review discipline: a sibling
// file that calls CreateCtx or builds a guestCtx literal fails the build's
// test run, with the reason. Precedent: internal/model/sandbox_manager_test.go
// parses the AST the same way. Refs: FR-17.7, SEC-04, ADR-010

// enclosingFuncs parses the package's non-test sources and reports the names
// of the functions containing each node matched by want.
//
// Files are parsed individually rather than via parser.ParseDir (deprecated,
// and build-tag aware) BECAUSE the guard must see every file — including the
// CGO binding built only under the "libkrun" tag, which is exactly where a
// stray context could be minted.
func enclosingFuncs(t *testing.T, want func(ast.Node) bool) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	var found []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if want(n) {
					found = append(found, fn.Name.Name)
				}
				return true
			})
		}
	}
	return found
}

func TestCreateCtx_HasExactlyOneCallSite(t *testing.T) {
	callers := enclosingFuncs(t, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "CreateCtx"
	})

	if len(callers) != 1 || callers[0] != "newGuestCtx" {
		t.Fatalf("krunAPI.CreateCtx is called from %v, want exactly [newGuestCtx].\n"+
			"Minting a libkrun context anywhere else bypasses the mandatory NIC "+
			"attachment and would boot the guest on TSI with full host egress "+
			"(ADR-010). Route it through newGuestCtx.", callers)
	}
}

func TestGuestCtx_IsOnlyConstructedByItsConstructor(t *testing.T) {
	builders := enclosingFuncs(t, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return false
		}
		ident, ok := lit.Type.(*ast.Ident)
		return ok && ident.Name == "guestCtx"
	})

	if len(builders) != 1 || builders[0] != "newGuestCtx" {
		t.Fatalf("guestCtx literals are built in %v, want exactly [newGuestCtx].\n"+
			"A guestCtx built elsewhere has not been through the mandatory NIC "+
			"attachment (ADR-010).", builders)
	}
}

// The NIC attach and the host-peer bind must each have exactly one call site,
// for the same reason CreateCtx does: a second one would be a path that
// attaches a NIC without a peer (boot hang) or binds a peer nothing uses.
func TestNetworkSetup_HasExactlyOneCallSiteEach(t *testing.T) {
	tests := []struct {
		name   string
		method string
	}{
		{name: "nic_attach", method: "AddNetUnixgram"},
		{name: "host_peer_bind", method: "bindHostPeer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callers := enclosingFuncs(t, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return false
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					return fn.Sel.Name == tt.method
				case *ast.Ident:
					return fn.Name == tt.method
				}
				return false
			})
			if len(callers) != 1 || callers[0] != "newGuestCtx" {
				t.Fatalf("%s is called from %v, want exactly [newGuestCtx].\n"+
					"The NIC and its host peer must be acquired together: a NIC with no "+
					"peer hangs the VM at boot, and a NIC with no device leaks host egress "+
					"via TSI (ADR-010).", tt.method, callers)
			}
		})
	}
}
