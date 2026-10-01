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

	// Held shared from adopting the entry until its pins are repointed and
	// recorded: prune cannot remove what this migration is pinning.
	release, err := cache.HoldShared()
	if err != nil {
		return fmt.Errorf("migrate in-tree guest base: %w", err)
	}
	defer release()
	entry, err := cache.Adopt(legacy, images.TreeDigest)
	if err != nil {
		return fmt.Errorf("migrate in-tree guest base: %w", err)
	}
	repointed, err := images.RepointToCache(hostRoot, legacy)
	if err != nil {
		return fmt.Errorf("migrate in-tree guest base: %w", err)
	}
	for _, name := range repointed {
		if err := recordCachedPin(cache, hostRoot, name); err != nil {
			return fmt.Errorf("migrate in-tree guest base: %w", err)
		}
	}
	if !entry.Deduplicated {
		// This migration published the entry and recorded its one pinner.
		markFullyRecorded(cache, entry.Digest, out)
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

// recordCachedPin records hostRoot as a pinner of the base registered under
// name, when that base lives in the cache — a base pinned by path is the
// operator's tree and no cache entry's business.
//
// It runs where a pin already exists: the in-tree migration and launch.
// Launch is what gives an entry composed before back-references existed its
// pinners, the first time each repository boots it. A record that cannot be
// written leaves a pin the records do not name, so it also withdraws the
// entry's fully-recorded mark: prune then keeps the entry's pinners unknown
// rather than judge it on an incomplete list. Refs: MGIT-239
func recordCachedPin(cache *basecache.Cache, hostRoot, name string) error {
	entry, err := images.LookupEntry(hostRoot, name)
	if err != nil {
		return err
	}
	if entry.RootfsPath != "" {
		return nil
	}
	if err := cache.RecordPinner(entry.Digest, hostRoot); err != nil {
		return errors.Join(err, cache.ForgetFullyRecorded(entry.Digest))
	}
	return nil
}

// recordComposedPin records a compose's pin BEFORE the lock is signed, and
// refuses the compose when it cannot: a repository must never come to pin an
// entry unrecorded. When this compose published the entry (rather than
// finding the same bytes cached), it is the entry's first and only pinner,
// so it marks the entry fully recorded. Bytes already cached keep whatever
// their publisher asserted. Refs: MGIT-239
func recordComposedPin(cache *basecache.Cache, hostRoot string, cached basecache.Entry, errOut io.Writer) error {
	if err := cache.RecordPinner(cached.Digest, hostRoot); err != nil {
		return fmt.Errorf("base from: record this repository as a pinner of %s; nothing was pinned: %w",
			cached.Digest, err)
	}
	if !cached.Deduplicated {
		markFullyRecorded(cache, cached.Digest, errOut)
	}
	return nil
}

// markFullyRecorded asserts completeness for an entry this process published.
// Failing to is a warning: the entry then keeps its pinners unknown, which is
// the safe direction.
func markFullyRecorded(cache *basecache.Cache, digest string, w io.Writer) {
	if err := cache.MarkFullyRecorded(digest); err != nil {
		_, _ = fmt.Fprintf(w, "warning: %v\n  `mgit sandbox base prune` will keep this entry unless it is named.\n", err)
	}
}

// warnUnrecordedPin says that a pin holds but its back-reference could not be
// written. It is a warning and not a failure because the pin itself is sound.
// Prune then treats the entry's pinners as unknown, until a later launch
// records this repository. Refs: MGIT-239
func warnUnrecordedPin(w io.Writer, err error) {
	_, _ = fmt.Fprintf(w, "warning: could not record this repository as a pinner of its guest base: %v\n"+
		"  `mgit sandbox base prune` keeps that base's pinners unknown until a launch records it.\n", err)
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
func registerComposedBase(hostRoot string, cached basecache.Entry, resolved guestbase.Ref,
	opts composeOptions, signer signFunc, clock func() time.Time,
) (composeResult, error) {
	sourceRef := resolved.String()
	rec := guestbase.Compose{
		Name:       opts.name,
		SourceTag:  guestbase.SourceTag(sourceRef),
		SourceRef:  sourceRef,
		BaseDigest: cached.Digest,
		// The platform manifest the index selected, kept beside the index so
		// a later recompose can compare like with like. Refs: MGIT-223
		PlatformDigest: resolved.SelectedPlatform,
	}
	// What this compose is about to supersede, read BEFORE the lock is
	// rewritten — afterwards it is unrecoverable from the lock alone.
	if prev, err := images.LookupEntry(hostRoot, opts.name); err == nil {
		rec.PrevSourceRef, rec.PrevBaseDigest = prev.Source, prev.Digest
		rec.PrevPlatformDigest = previousPlatform(hostRoot, opts.name, prev.Source)
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

// previousPlatform is the platform manifest the superseded compose recorded
// selecting, read from the journal: the newest entry for this name whose
// source is the one being replaced. Empty when that compose recorded none (a
// single-platform tag, or an mgit before the field existed) or the journal
// cannot say; the comparison then falls back to what it can prove.
// Refs: MGIT-223
func previousPlatform(hostRoot, name, prevSource string) string {
	history, err := guestbase.ComposeHistory(hostRoot)
	if err != nil {
		return ""
	}
	for i := len(history) - 1; i >= 0; i-- {
		if h := history[i]; h.Name == name && h.SourceRef == prevSource {
			return h.PlatformDigest
		}
	}
	return ""
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
	reportSourceChange(out, res.Record)
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

// reportSourceChange says what a recompose of the same tag did to its
// source, comparing like with like (MGIT-223): it shouts only when the image
// itself moved, and names each digest by its kind.
func reportSourceChange(out io.Writer, rec guestbase.Compose) {
	prev, now := guestbase.SourceDigest(rec.PrevSourceRef), guestbase.SourceDigest(rec.SourceRef)
	switch rec.SourceChange() {
	case guestbase.SourceKindChanged:
		_, _ = fmt.Fprintf(out, "\n  %s resolves to the same image as the base you had pinned.\n"+
			"        selects   %s  (the platform manifest that base recorded)\n"+
			"        recorded  %s  (the image index: since mgit 0.6.8, the same on every host)\n"+
			"        base      %s -> %s  (it includes mgit's own guest binaries)\n",
			rec.SourceTag, prev, now, rec.PrevBaseDigest, rec.BaseDigest)
	case guestbase.SourceIndexMoved:
		_, _ = fmt.Fprintf(out, "\n  NOTE: %s's image index moved; the image this host composes is unchanged.\n"+
			"        index     %s -> %s\n"+
			"        selects   %s  (unchanged: the platform manifest for this host)\n"+
			"        Other architectures may now compose a different image.\n",
			rec.SourceTag, prev, now, rec.PlatformDigest)
	case guestbase.SourceImageMoved:
		selected := ""
		if rec.PlatformDigest != "" {
			selected = fmt.Sprintf(", selecting platform manifest %s for this host", rec.PlatformDigest)
		}
		_, _ = fmt.Fprintf(out, "\n  NOTE: %s now resolves to a different image than the base you had pinned.\n"+
			"        was  %s  (base %s)\n"+
			"        now  %s%s  (base %s)\n"+
			"        Nothing was replaced: the previous base is still in the cache, and\n"+
			"        anything already pinned to it keeps resolving.\n",
			rec.SourceTag, prev, rec.PrevBaseDigest, now, selected, rec.BaseDigest)
	}
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
