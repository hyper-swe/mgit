package land

// Fuzz + property harness for the vsock land-protocol parser — the primary
// guest-controlled parse surface on the host (the guest is the hostile party,
// SEC-01). It exercises the frame decoder (Limits.DecodeObjects), tree-entry
// path canonicalization (ValidateTreePath + the landed-tree walk), and the
// host-side go-git decode/bind of arbitrary guest bytes (CommitFromObjectData,
// poolStorer/landedFileSet, CommitChain/orderChain, VerifyBinding). The
// invariants under test: no input panics or hangs the host; nothing escapes the
// worktree via a tree path; and audit-bearing commit fields are bound to the
// content-addressed object so a guest cannot inject divergent audit data.
//
// The FuzzXxx targets run their committed seed corpus under plain `go test`
// (so the harness runs in CI) and continuously under `go test -fuzz`. The
// TestFuzz_* property tests assert the same invariants deterministically.
// Refs: MGIT-11.13.3, FR-17.35, SEC-01, SEC-06, NFR-5.6, F-03

import (
	"bytes"
	"io"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/hyper-swe/mgit/internal/model"
	gitstore "github.com/hyper-swe/mgit/internal/store/git"
)

// fzLimits are deliberately small ceilings so the fuzzer exercises the bound
// checks without ever allocating large buffers (a declared length over the
// per-object cap is refused before any allocation). Refs: F-03, FR-17.35
func fzLimits() Limits {
	return Limits{MaxObjectBytes: 4096, MaxObjectsPerLand: 64, MaxTotalBytes: 1 << 16}
}

// fzReadObj returns an encoded object's canonical payload bytes (no git
// header) — the form carried in a land frame and consumed by
// CommitFromObjectData. Works from a *testing.F seed context or a *testing.T.
func fzReadObj(tb testing.TB, o plumbing.EncodedObject) []byte {
	tb.Helper()
	r, err := o.Reader()
	if err != nil {
		tb.Fatalf("seed: object reader: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		tb.Fatalf("seed: read object: %v", err)
	}
	return data
}

// fzCommitBytes builds the raw bytes of a git commit object with the given
// audit fields, for seeds and the audit-binding property test.
func fzCommitBytes(tb testing.TB, msg, agent string, tree, parent plumbing.Hash, when time.Time) []byte {
	tb.Helper()
	sig := object.Signature{Name: agent, Email: agent + "@mgit", When: when}
	gc := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree}
	if !parent.IsZero() {
		gc.ParentHashes = []plumbing.Hash{parent}
	}
	o := memory.NewStorage().NewEncodedObject()
	if err := gc.Encode(o); err != nil {
		tb.Fatalf("seed: encode commit: %v", err)
	}
	return fzReadObj(tb, o)
}

// fzTreeBytes builds the raw bytes of a git tree object with the given entries.
func fzTreeBytes(tb testing.TB, entries ...object.TreeEntry) []byte {
	tb.Helper()
	o := memory.NewStorage().NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(o); err != nil {
		tb.Fatalf("seed: encode tree: %v", err)
	}
	return fzReadObj(tb, o)
}

// fzDriveParse runs the full host-side land-parse chain over raw guest bytes:
// frame-decode under the small ceilings, then push every decoded object
// through the index/chain/pool/derive parsers. It returns nothing — the only
// property is that NONE of it panics and the decoder's ceilings hold. Shared
// by the fuzz target and the deterministic no-panic test.
func fzDriveParse(tb testing.TB, raw []byte) {
	tb.Helper()
	lim := fzLimits()
	objs, err := lim.DecodeObjects(bytes.NewReader(raw))
	if err != nil {
		return // a rejected stream is the expected outcome for most inputs
	}
	// The decoder must never return a set that violates its own ceilings.
	if len(objs) > lim.MaxObjectsPerLand {
		tb.Fatalf("decoder returned %d objects over the %d ceiling", len(objs), lim.MaxObjectsPerLand)
	}
	var total int64
	for _, o := range objs {
		if len(o.Data) > lim.MaxObjectBytes {
			tb.Fatalf("decoder returned a %d-byte object over the %d cap", len(o.Data), lim.MaxObjectBytes)
		}
		total += int64(len(o.Data))
	}
	if total > lim.MaxTotalBytes {
		tb.Fatalf("decoder returned %d total bytes over the %d cap", total, lim.MaxTotalBytes)
	}

	// Downstream host-side parsers over the (untrusted) decoded pool. Each may
	// return an error; none may panic. CommitChain/orderChain exercise parent
	// ordering (fork/gap/cycle), poolStorer+landedFileSet the go-git tree walk,
	// and DeriveLandedCommit the commit decode.
	_, _ = CommitObjectsByID(objs)
	_, _ = CommitChain(objs, func(string) bool { return false })
	if st, perr := poolStorer(objs); perr == nil {
		for _, o := range objs {
			if o.Type == ObjTree {
				_, _ = landedFileSet(st, plumbing.ComputeHash(plumbing.TreeObject, o.Data).String())
			}
		}
	}
	tid, err := model.ParseTaskID("MGIT-11.13.3")
	if err != nil {
		tb.Fatalf("parse task id: %v", err)
	}
	for _, o := range objs {
		if o.Type == ObjCommit {
			_, _ = DeriveLandedCommit(o.Data, objs, tid, nil)
		}
	}
}

// fzFrame encodes one [type][BE len][payload] frame (seed construction).
func fzFrame(typ byte, data []byte) []byte {
	buf := make([]byte, frameHeaderLen, frameHeaderLen+len(data))
	buf[0] = typ
	n := uint32(len(data))
	buf[1], buf[2], buf[3], buf[4] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	return append(buf, data...)
}

// FuzzLandParser_DecodeObjects fuzzes the whole host-side parse chain with
// arbitrary guest bytes. Property: no panic, and the decoder never returns a
// set exceeding its ceilings. Refs: MGIT-11.13.3, F-03, SEC-06
func FuzzLandParser_DecodeObjects(f *testing.F) {
	commit := fzCommitBytes(f, "seed", "agent", plumbing.ZeroHash, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	f.Add([]byte(nil))
	f.Add([]byte{ObjCommit})                 // truncated header
	f.Add([]byte{0x00, 0, 0, 0, 1, 'x'})     // unknown type
	f.Add(fzFrame(ObjBlob, []byte("hello"))) // one valid blob frame
	f.Add(fzFrame(ObjCommit, commit))        // one valid commit frame
	f.Add(append(fzFrame(ObjTree, []byte("garbagetree")), fzFrame(ObjBlob, []byte("b"))...))
	f.Add(fzFrame(ObjBlob, bytes.Repeat([]byte("z"), 5000))) // over per-object cap
	f.Add(bytes.Repeat(fzFrame(ObjBlob, []byte("a")), 200) /*over count*/)
	f.Add([]byte{ObjBlob, 0xFF, 0xFF, 0xFF, 0xFF}) // huge declared length, no body
	f.Fuzz(func(t *testing.T, raw []byte) { fzDriveParse(t, raw) })
}

// FuzzLandParser_ValidateTreePath fuzzes tree-path canonicalization. Property:
// no panic, and any ACCEPTED path is genuinely worktree-confined (canonical,
// relative, no traversal/NUL/backslash). A fuzzer that finds an accepted-but-
// unsafe path has found a real escape. Refs: MGIT-11.13.3, NFR-5.6, FR-17.35
func FuzzLandParser_ValidateTreePath(f *testing.F) {
	for _, s := range []string{
		"", ".", "..", "a", "a/b.go", "../x", "/abs", "a//b", "a/", "./a",
		"a/../b", "a\\b", "a\x00b", "src/app/main.go", "..\\..\\x", "\x00", "a/./b",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if err := ValidateTreePath(p); err != nil {
			return // rejected — fine
		}
		// Accepted: it MUST be safe by every measure the validator promises.
		if p == "" || path.IsAbs(p) || path.Clean(p) != p ||
			strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') {
			t.Fatalf("ValidateTreePath accepted an unsafe path %q", p)
		}
		for _, comp := range strings.Split(p, "/") {
			if comp == ".." {
				t.Fatalf("ValidateTreePath accepted a traversal path %q", p)
			}
		}
	})
}

// FuzzLandParser_DeriveCommit fuzzes the go-git commit decode of arbitrary
// guest bytes (the audit-field source). Property: no panic. Refs: MGIT-11.13.3
func FuzzLandParser_DeriveCommit(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("not a commit"))
	f.Add(fzCommitBytes(f, "ok", "a", plumbing.ZeroHash, plumbing.ZeroHash, time.Unix(0, 0).UTC()))
	f.Add(fzCommitBytes(f, "with\nnewlines\x1b[31m", "a\tb", plumbing.ZeroHash, plumbing.ZeroHash, time.Unix(0, 0).UTC()))
	f.Fuzz(func(t *testing.T, data []byte) {
		// CommitFromObjectData parses untrusted bytes; it may error, never panic.
		if c, err := gitstore.CommitFromObjectData(data); err == nil {
			_ = c.ComputeContentHash()
		}
	})
}

// TestFuzz_LandParser_NoPanic feeds a battery of hostile/degenerate framings
// through the full parse chain and asserts none panics — each case is run under
// a recover guard so one panic is reported with context rather than aborting
// the suite. Refs: MGIT-11.13.3, F-03
func TestFuzz_LandParser_NoPanic(t *testing.T) {
	commit := fzCommitBytes(t, "msg", "agent", plumbing.ZeroHash, plumbing.ZeroHash, time.Unix(0, 0).UTC())
	cases := map[string][]byte{
		"empty":              nil,
		"truncated_header":   {ObjCommit, 0x00},
		"unknown_type":       fzFrame('Z', []byte("x")),
		"huge_declared_len":  {ObjBlob, 0xFF, 0xFF, 0xFF, 0xFF},
		"valid_commit":       fzFrame(ObjCommit, commit),
		"garbage_commit":     fzFrame(ObjCommit, []byte("nope")),
		"garbage_tree":       fzFrame(ObjTree, bytes.Repeat([]byte{0xDE, 0xAD}, 64)),
		"over_count":         bytes.Repeat(fzFrame(ObjBlob, []byte("a")), 200),
		"trailing_partial":   append(fzFrame(ObjBlob, []byte("ok")), ObjBlob, 0x00),
		"commit_refs_no_obj": fzFrame(ObjCommit, fzCommitBytes(t, "m", "a", plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), plumbing.ZeroHash, time.Unix(0, 0).UTC())),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parse chain panicked on %q input: %v", name, r)
				}
			}()
			fzDriveParse(t, raw)
		})
	}
}

// TestFuzz_LandParser_NoTraversalEscape asserts every traversal / absolute /
// non-canonical / NUL / backslash path is rejected — both directly through
// ValidateTreePath and through the landed-tree walk (a traversal hidden in a
// tree-entry name). Safe paths are accepted. Refs: MGIT-11.13.3, NFR-5.6
func TestFuzz_LandParser_NoTraversalEscape(t *testing.T) {
	unsafe := []string{
		"..", "../etc/passwd", "a/../../b", "foo/..", "/abs", "/", "",
		"a//b", "a/", "./a", "a/./b", "a\\b", "..\\x", "a\x00b", "\x00",
		"....//", "../", "a/../..",
	}
	for _, p := range unsafe {
		if err := ValidateTreePath(p); err == nil {
			t.Fatalf("ValidateTreePath accepted unsafe path %q", strings.ReplaceAll(p, "\x00", "<NUL>"))
		}
	}
	for _, p := range []string{"main.go", "src/app/main.go", "a/b/c.txt", ".github/workflows/ci.yml", "..foo", "a..b"} {
		if err := ValidateTreePath(p); err != nil {
			t.Fatalf("ValidateTreePath rejected safe path %q: %v", p, err)
		}
	}

	// A traversal smuggled as a tree-entry name must be rejected by the walk.
	treeWithBackslash := fzTreeBytes(t, object.TreeEntry{
		Name: "a\\b", Mode: filemode.Regular, Hash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	})
	objs := []Object{{Type: ObjTree, Data: treeWithBackslash}}
	st, err := poolStorer(objs)
	if err != nil {
		t.Fatalf("poolStorer: %v", err)
	}
	th := plumbing.ComputeHash(plumbing.TreeObject, treeWithBackslash).String()
	if _, err := landedFileSet(st, th); err == nil {
		t.Fatal("landedFileSet accepted a tree entry with a backslash path")
	}
}

// TestFuzz_LandParser_NoAuditInjection asserts that audit-bearing commit fields
// are BOUND to the content-addressed object: a guest cannot present honest
// bytes with lying audit metadata (VerifyBinding rejects any divergence), and
// hostile content (control chars, ANSI, SQL-ish text) embedded in a bound field
// neither panics nor escapes binding — it lands only as content-bound data
// (the audit store writes it via parameterized SQL, SQL Rule 1). Refs: MGIT-11.13.3, SEC-06
func TestFuzz_LandParser_NoAuditInjection(t *testing.T) {
	hostile := "'; DROP TABLE task_commits;--\n\x1b[31m\x07rm -rf /"
	objData := fzCommitBytes(t, hostile, "agent\tx", plumbing.ZeroHash, plumbing.ZeroHash, time.Unix(100, 0).UTC())
	pool := []Object{{Type: ObjCommit, Data: objData}}

	// Derived commit is content-bound and passes VerifyBinding by construction.
	c, err := DeriveLandedCommit(objData, pool, taskID(t, "MGIT-11.13.3"), nil)
	if err != nil {
		t.Fatalf("DeriveLandedCommit on hostile-message commit: %v", err)
	}
	if c.Message != hostile {
		t.Fatalf("derived message not preserved verbatim from the object")
	}
	if err := VerifyBinding(objData, c); err != nil {
		t.Fatalf("a content-derived commit must pass binding: %v", err)
	}

	// Tampering any audit field away from the object bytes must be rejected —
	// the guest cannot inject audit data not bound to the hashed object.
	{
		cc := *c
		cc.Message = "innocent"
		if VerifyBinding(objData, &cc) == nil {
			t.Fatal("VerifyBinding accepted a commit with a tampered message (audit injection)")
		}
	}
	{
		cc := *c
		cc.AgentID = "root"
		if VerifyBinding(objData, &cc) == nil {
			t.Fatal("VerifyBinding accepted a commit with a tampered agent_id (audit injection)")
		}
	}
	{
		cc := *c
		cc.CommitID = "deadbeef"
		if VerifyBinding(objData, &cc) == nil {
			t.Fatal("VerifyBinding accepted a commit with a tampered commit_id (audit injection)")
		}
	}
}
