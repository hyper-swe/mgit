package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// stagingMaxAge is how long an abandoned compose keeps its staging tree.
// Each is a whole base, so leaking them is a multi-gigabyte problem; a day is
// far longer than any compose and far shorter than "forever".
const stagingMaxAge = 24 * time.Hour

// openBaseCache opens this machine's guest-base cache and collects the debris
// of any compose that died mid-pull.
//
// Pruning here rather than on a timer is deliberate: the only process that
// can safely decide a staging tree is abandoned is one about to make another,
// and this is the only place that happens. Refs: MGIT-147
func openBaseCache() (*basecache.Cache, error) {
	cache, err := basecache.Open()
	if err != nil {
		return nil, err
	}
	// Best-effort: a cache that cannot be swept can still be composed into,
	// and failing a compose over housekeeping would be the wrong trade.
	_, _ = cache.PruneStaging(time.Now(), stagingMaxAge)
	return cache, nil
}

// migrateInTreeBase moves a pre-MGIT-147 in-tree guest base out of the
// repository and into the machine-wide cache, announcing what it did.
//
// MIGRATED, not ignored and not silently deleted. Ignoring it would leave the
// original complaint standing — hundreds of megabytes inside the repo that
// every `gofmt -l .` walks — and deleting it would cost the user a multi-
// gigabyte re-pull for no reason. Adoption preserves the digest, so the pin
// in images.lock keeps resolving across the move and no running task notices.
// The lock entry is then repointed off the in-tree path; no re-signing is
// involved, because paths were never part of the signing payload.
//
// It is announced rather than silent because bytes moving between directories
// without explanation is exactly the kind of surprise that costs trust.
// Refs: MGIT-147
func migrateInTreeBase(hostRoot string, cache *basecache.Cache, out io.Writer) error {
	legacy := images.LegacyInTreeBase(hostRoot)
	if legacy == "" {
		return nil
	}
	_, _ = fmt.Fprintf(out,
		"Moving this repo's guest base out of the repository (MGIT-147).\n"+
			"  from %s\n"+
			"  to   %s\n"+
			"  An in-tree base is walked by every test command that walks your repo, and\n"+
			"  recomposing it in one worktree used to invalidate every other worktree's\n"+
			"  pinned digest. The bytes and the digest are unchanged, so what you pinned\n"+
			"  still resolves.\n", legacy, cache.Root())

	entry, err := cache.Adopt(legacy, images.TreeDigest)
	if err != nil {
		return fmt.Errorf("migrate in-tree guest base: %w", err)
	}
	repointed, err := images.RepointToCache(hostRoot, legacy)
	if err != nil {
		return fmt.Errorf("migrate in-tree guest base: %w", err)
	}
	switch {
	case len(repointed) > 0:
		_, _ = fmt.Fprintf(out, "  moved %s; %v now resolve from the cache\n", entry.Digest, repointed)
	default:
		// The in-tree tree did not match any pin — it was already stale, and
		// its true digest is what got cached. Nothing is lost; say so.
		_, _ = fmt.Fprintf(out,
			"  moved %s. No images.lock entry pointed at it, so it was already stale;\n"+
				"  it is cached under its own digest rather than discarded.\n", entry.Digest)
	}
	return nil
}

// composeOptions are the knobs `sandbox base from` and `sandbox base set`
// share: which lock name to register under and where the guest pair is.
type composeOptions struct {
	name        string
	guestBinDir string
	plainHTTP   bool
}

// composeResult is what a completed composition produced, for printing.
type composeResult struct {
	Ref       string            // digest-pinned reference, <name>@sha256:<hex>
	CachePath string            // where the immutable entry lives
	Reused    bool              // the identical tree was already cached
	Record    guestbase.Compose // the provenance journal entry just written
}

// registerComposedBase pins a freshly cached base into images.lock and
// journals the provenance of the composition that produced it.
//
// THE TAG IS PROVENANCE; THE DIGEST IS IDENTITY. A tag is a name that can
// point twice — golang:1.26-bookworm resolved to two different images a day
// apart, and with the tag as identity nobody could say whether upstream had
// moved or our composition had changed. So the tag is resolved to a digest
// ONCE, at pull time, the digest is what gets pinned, and the tag rides along
// as the human-facing half of the record. A recompose whose input digest
// differs is a NEW cache entry and a NEW journal line; it never overwrites
// what came before. Refs: MGIT-147, MGIT-105
func registerComposedBase(hostRoot string, cached basecache.Entry, sourceRef string,
	opts composeOptions, signer signFunc, clock func() time.Time,
) (composeResult, error) {
	rec := guestbase.Compose{
		Name:       opts.name,
		SourceTag:  guestbase.SourceTag(sourceRef),
		SourceRef:  sourceRef,
		BaseDigest: cached.Digest,
	}
	// What this compose is about to supersede, read BEFORE the lock is
	// rewritten — afterwards it is unrecoverable from the lock alone.
	if prev, err := images.LookupEntry(hostRoot, opts.name); err == nil {
		rec.PrevSourceRef, rec.PrevBaseDigest = prev.Source, prev.Digest
	} else if !errors.Is(err, images.ErrNoSuchImage) {
		return composeResult{}, fmt.Errorf("base %s: %w", opts.name, err)
	}

	entry := images.BuildCachedBaseEntry(cached.Digest)
	entry.Source = sourceRef
	ref, err := signer(hostRoot, opts.name, entry)
	if err != nil {
		return composeResult{}, err
	}
	if err := guestbase.RecordCompose(hostRoot, rec, clock); err != nil {
		return composeResult{}, err
	}
	return composeResult{Ref: ref, CachePath: cached.Path, Reused: cached.Deduplicated, Record: rec}, nil
}

// signFunc registers a signed entry and returns its digest-pinned reference.
// Injected so the compose flow does not carry the signing key around.
type signFunc func(hostRoot, name string, entry images.Entry) (string, error)

// reportComposition prints what changed, and shouts when a tag moved.
//
// A moved tag is the one outcome a user must not scroll past: they asked for
// the same name and got different bytes. Saying WHICH digests, and that the
// old base is still cached, is the difference between an audit trail and a
// surprise. Refs: MGIT-147
func reportComposition(out io.Writer, res composeResult) {
	if res.Record.TagMoved() {
		_, _ = fmt.Fprintf(out,
			"\n  NOTE: %s now resolves to a different image than the base you had pinned.\n"+
				"        was  %s  (base %s)\n"+
				"        now  %s  (base %s)\n"+
				"        Nothing was replaced: the previous base is still in the cache, and\n"+
				"        anything already pinned to it keeps resolving.\n",
			res.Record.SourceTag,
			guestbase.SourceDigest(res.Record.PrevSourceRef), res.Record.PrevBaseDigest,
			guestbase.SourceDigest(res.Record.SourceRef), res.Record.BaseDigest)
	}
	_, _ = fmt.Fprintf(out, "Registered guest base %s\n", res.Ref)
	if res.Record.SourceRef != "" {
		_, _ = fmt.Fprintf(out, "  from %s\n", res.Record.SourceRef)
	}
	reused := ""
	if res.Reused {
		reused = " (already cached; nothing was re-unpacked)"
	}
	_, _ = fmt.Fprintf(out, "  bytes in %s%s\n", res.CachePath, reused)
}

// composeJSON is the machine-readable form of a composition.
func composeJSON(res composeResult) map[string]any {
	doc := map[string]any{
		"image_ref":   res.Ref,
		"base_digest": res.Record.BaseDigest,
		"cache_path":  res.CachePath,
		"reused":      res.Reused,
	}
	if res.Record.SourceRef != "" {
		doc["source"] = res.Record.SourceRef
		doc["source_tag"] = res.Record.SourceTag
	}
	if res.Record.TagMoved() {
		doc["tag_moved"] = true
		doc["previous_source"] = res.Record.PrevSourceRef
		doc["previous_base_digest"] = res.Record.PrevBaseDigest
	}
	return doc
}

// inTreeBasePath is the in-tree location a pre-MGIT-147 mgit composed into.
// Nothing writes there any more; tests assert its absence.
func inTreeBasePath(repoRoot string) string {
	return filepath.Join(repoRoot, ".mgit", "sandbox", images.InTreeBaseDir)
}
