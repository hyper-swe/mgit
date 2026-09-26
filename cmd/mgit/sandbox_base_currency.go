package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hyper-swe/mgit/internal/sandboxd/basecache"
	"github.com/hyper-swe/mgit/internal/sandboxd/guestbase"
	"github.com/hyper-swe/mgit/internal/sandboxd/images"
)

// recomposePlaceholder stands for the image to recompose from when the
// repository's lock does not name it.
const recomposePlaceholder = "<image>"

// baseCurrencyNotice is the one warning for a sandbox whose guest base was
// composed by another mgit, or does not record which one composed it, or ""
// when the base is this build's or is not a base this CLI composed (a cached
// tree it can read).
//
// MGIT-174 shipped half of its acceptance: doctor asks, and nothing else
// does, so a base composed by the previous release ran every command of a
// loop round with no word said, while its guest silently lacked every
// guest-side fix since. This is the other half. It warns and never refuses:
// refusing would break every working setup on release day. It compares with
// the same function doctor's base/currency row uses, so the two cannot
// disagree. Refs: MGIT-224, MGIT-174
func baseCurrencyNotice(cache *basecache.Cache, imageDigest, running string) string {
	if cache == nil || imageDigest == "" || !cache.Has(imageDigest) {
		return ""
	}
	dir, err := cache.Path(imageDigest)
	if err != nil {
		return ""
	}
	composed := ""
	if rec, err := guestbase.ReadComposedBy(dir); err == nil {
		composed = rec.Version
	}
	switch guestbase.BaseCurrency(composed, running) {
	case guestbase.CurrencyStale:
		return fmt.Sprintf("warning: this sandbox's guest base was composed by mgit %s, and this is mgit %s, "+
			"so the guest binaries frozen into it are not this build's: it lacks every guest-side change between "+
			"them. Recompose it with `mgit sandbox base from %s` and relaunch the sandbox. This is a warning; "+
			"nothing was refused.\n", composed, running, recomposePlaceholder)
	case guestbase.CurrencyUnknown:
		return fmt.Sprintf("warning: this sandbox's guest base does not record which mgit composed it (UNKNOWN), "+
			"so whether its guest binaries are this build's (mgit %s) cannot be told. Recompose it with `mgit "+
			"sandbox base from %s` and relaunch the sandbox. This is a warning; nothing was refused.\n",
			running, recomposePlaceholder)
	}
	return ""
}

// warnStaleBase writes the notice for the base a sandbox runs, naming the
// image to recompose from when this repository's lock records it. Anything
// it cannot read says nothing: this is a warning beside a verb, not a check
// of its own (doctor's base/currency row is that). Refs: MGIT-224
func warnStaleBase(w io.Writer, imageDigest string) {
	cache, err := basecache.Open()
	if err != nil {
		return
	}
	notice := baseCurrencyNotice(cache, imageDigest, Version)
	if notice == "" {
		return
	}
	if hostRoot, err := sandboxHostRoot(); err == nil {
		if entry, err := images.LookupEntry(hostRoot, defaultGuestBaseName); err == nil &&
			entry.Digest == imageDigest && entry.Source != "" {
			notice = strings.Replace(notice, recomposePlaceholder, guestbase.SourceTag(entry.Source), 1)
		}
	}
	_, _ = io.WriteString(w, notice)
}

// imageRefDigest is the digest of a pinned reference name@sha256:<hex>.
func imageRefDigest(ref string) string {
	_, digest, _ := strings.Cut(ref, "@")
	return digest
}
