package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This is a reviewed inventory, not a second copy of the eligibility predicate.
// New raw getters, wrappers or direct-hash sources require explicit classification.
// A classified content/apply function must also invoke the shared boundary.
var contentReaderClasses = map[string]string{
	// Raw audit/metadata and allocation reads never authorize content.
	"cmd/mgit/diff.go:diffCmd":                               "via:DiffCommits",
	"cmd/mgit/diff.go:renderDiff":                            "raw:format-only; input from gated DiffTask or DiffCommits in diffCmd",
	"internal/mcp/server.go:diffTool":                        "via:DiffCommits",
	"cmd/mgit-sandboxd/land_projection.go:conflictRemedy":    "apply",
	"cmd/mgit-sandboxd/land_projection.go:Check":             "recovery",
	"cmd/mgit-sandboxd/land_projection.go:publish":           "via:RequireKnownContentOrigins",
	"cmd/mgit/cherry_pick.go:cherryPickCmd":                  "via:RequireContentSource",
	"cmd/mgit/export.go:buildExportPayload":                  "raw:metadata-only; git export delegates PreviewGitPatch",
	"cmd/mgit/log_cadence.go:runTaskLog":                     "raw:audit",
	"cmd/mgit/log_cadence.go:subjectFor":                     "raw:subject-only",
	"cmd/mgit/rollback.go:rollbackCmd":                       "raw:source-task-resolution; RollbackTask checks SourceHash",
	"cmd/mgit/show.go:showCmd":                               "raw:forensic-read; InspectCommitDiff and recorded FileDiffs inspect retained objects only",
	"internal/api/http/server.go:getCommitHandler":           "raw:forensic-read",
	"internal/api/http/server.go:getTaskCommitsHandler":      "raw:audit",
	"internal/mcp/server.go:exportTool":                      "raw:audit-json",
	"internal/mcp/server.go:logTool":                         "raw:audit",
	"internal/mcp/server.go:showTool":                        "raw:forensic-read",
	"internal/service/apply_recovery.go:RecoverPendingApply": "raw:host-operation-journal-recovery",
	"internal/service/apply_recovery.go:indexIfMissing":      "raw:duplicate-and-position",
	"internal/service/bundle.go:Export":                      "raw:audit-bundle",
	"internal/service/bundle.go:Import":                      "raw:existence; no admission imported",
	"internal/service/cadence.go:TaskCadence":                "raw:metadata",
	"internal/service/cherrypick.go:CherryPick":              "apply",
	"internal/service/commit.go:CreateCommit":                "raw:position",
	"internal/service/commit.go:GetCommit":                   "raw:forensic-wrapper",
	"internal/service/commit.go:GetTaskCommits":              "raw:audit-wrapper",
	"internal/service/commit.go:RequireContentSource":        "apply",
	"internal/service/diff.go:DiffCommits":                   "content",
	"internal/service/diff.go:InspectCommitDiff":             "raw:forensic-wrapper; never authorizes export or apply",
	"internal/service/diff.go:DiffRange":                     "via:DiffCommits",
	"internal/service/diff.go:DiffTask":                      "content",
	"internal/service/land_service.go:newChain":              "raw:duplicate-and-position",
	"internal/service/restore.go:RestoreAll":                 "apply",
	"internal/service/restore.go:RestoreFile":                "apply",
	"internal/service/rollback.go:RollbackTask":              "apply",
	"internal/service/rollback.go:taskInverseDiffs":          "raw:eligible-records-from-RollbackTask",
	"internal/service/squash.go:SquashTask":                  "raw:position; content delegated planSquash",
	"internal/service/squash.go:planSquash":                  "content",
	"internal/service/verify.go:VerifyCommitChain":           "raw:integrity-not-delivery",
	"internal/service/verify.go:VerifyTaskCommits":           "raw:integrity-not-delivery",
	"internal/service/worktree.go:existingForkBase":          "content",
	"internal/service/worktree.go:resolveBaseCommit":         "gated:RequireDeliverableTip",
	"internal/service/worktree.go:resolveBaseRef":            "raw:resolved-then-gated-by-resolveBaseCommit",
}

func contentReaderCalls(source []byte) (map[string][]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "reader.go", source, 0)
	if err != nil {
		return nil, err
	}
	result := map[string][]string{}
	watched := map[string]bool{"DiffCommits": true, "DiffRange": true, "InspectCommitDiff": true, "FormatUnified": true, "GetContentSnapshot": true, "RequireKnownContentOrigins": true, "RequireContentSource": true, "GetTaskCommits": true, "GetContentCommits": true, "RequireContentCommit": true, "GetCommit": true, "GetFileFromCommit": true, "CreateCommitFromDiffsJournaled": true, "MaterializeDiffs": true}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && watched[sel.Sel.Name] {
				result[fn.Name.Name] = append(result[fn.Name.Name], sel.Sel.Name)
			}
			return true
		})
	}
	for k := range result {
		sort.Strings(result[k])
	}
	return result, nil
}

func unclassifiedContentReaders(path string, source []byte, classes map[string]string) ([]string, error) {
	calls, err := contentReaderCalls(source)
	if err != nil {
		return nil, err
	}
	var findings []string
	for fn, methods := range calls {
		key := path + ":" + fn

		counts := map[string]int{}
		for _, method := range methods {
			counts[method]++
		}
		for method, count := range counts {
			if count > 1 {
				findings = append(findings, key+" adds another "+method+" reader; classify each use")
			}
		}
		class, ok := classes[key]
		if !ok {
			findings = append(findings, key+" ["+strings.Join(methods, ",")+"]")
			continue
		}
		if strings.HasPrefix(class, "via:") {
			want := strings.TrimPrefix(class, "via:")
			found := false
			for _, method := range methods {
				if method == want {
					found = true
				}
			}
			if !found {
				findings = append(findings, key+" missing delegated source boundary")
			}
		}
		if class == "content" || class == "apply" {
			allocation := key == "internal/service/cherrypick.go:CherryPick" || key == "internal/service/rollback.go:RollbackTask"
			if counts["GetTaskCommits"] > 0 && (class == "content" || !allocation) {
				findings = append(findings, key+" adds an unclassified raw content read")
			}

			gated := false
			for _, method := range methods {
				if method == "GetContentCommits" || method == "RequireContentCommit" {
					gated = true
				}
			}
			if !gated {
				findings = append(findings, key+" missing shared content boundary")
			}
		}
	}
	sort.Strings(findings)
	return findings, nil
}

func TestContentReaderInventory(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	var findings []string
	for _, dir := range []string{"internal/service", "internal/api", "internal/mcp", "cmd"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // G304: Go source selected by WalkDir under this repository, read-only test inventory
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found, err := unclassifiedContentReaders(filepath.ToSlash(rel), b, contentReaderClasses)
			findings = append(findings, found...)
			return err
		}))
	}
	sort.Strings(findings)
	require.Empty(t, findings, "unclassified content readers:\n%s", strings.Join(findings, "\n"))
}

func TestContentReaderInventory_NegativeControls(t *testing.T) {
	for _, body := range []string{
		`func export(s *Store) { s.GetTaskCommits(ctx, task) }`,
		`func export(s *DiffService) { s.DiffCommits(ctx, from, to) }`,
		`func export(s *DiffService) { s.InspectCommitDiff(ctx, from, to) }`,
		`func export(s *DiffService) { s.FormatUnified(diffs) }`,
		`func export(s *DiffService) { s.DiffRange(ctx, from, to) }`,
		`func export(s *CommitService) { s.GetTaskCommits(ctx, task) }`,
		`func apply(s *CommitService) { s.GetCommit(ctx, hash); s.CreateCommitFromDiffsJournaled(ctx, c, diffs, entry) }`,
		`func apply(s *RestoreService) { s.GetFileFromCommit(ctx, hash, path) }`,
	} {
		findings, err := unclassifiedContentReaders("new.go", []byte("package service\n"+body), map[string]string{})
		require.NoError(t, err)
		require.NotEmpty(t, findings, "new direct or wrapper source must fail inventory")
	}
	findings, err := unclassifiedContentReaders("x.go", []byte(`package service;func export(s *Service){s.GetContentCommits(ctx,task);s.GetTaskCommits(ctx,task)}`), map[string]string{"x.go:export": "content"})
	require.NoError(t, err)
	require.Contains(t, findings, "x.go:export adds an unclassified raw content read")
	findings, err = unclassifiedContentReaders("x.go", []byte(`package service;func metadata(s *Service){s.GetTaskCommits(ctx,task);s.GetTaskCommits(ctx,other)}`), map[string]string{"x.go:metadata": "raw:audit"})
	require.NoError(t, err)
	require.Contains(t, findings, "x.go:metadata adds another GetTaskCommits reader; classify each use")

	findings, err = unclassifiedContentReaders("x.go", []byte(`package service;func apply(s *Service){s.GetCommit(ctx,hash)}`), map[string]string{"x.go:apply": "apply"})
	require.NoError(t, err)
	require.Contains(t, findings, "x.go:apply missing shared content boundary")
}
