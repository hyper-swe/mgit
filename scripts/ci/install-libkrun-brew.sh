#!/usr/bin/env bash
# install-libkrun-brew.sh — tap, trust and install libkrun from the third-party
# Homebrew tap, with every network step guarded. Refs: MGIT-143, MGIT-119
#
# WHY THIS IS A SCRIPT AND NOT THREE COPIES OF FIVE LINES. These same three
# commands appear in ci.yml's `libkrun` job, release.yml's `release` job and
# release.yml's `release-smoke` job. Only the first ever grew a retry, so the
# 2026-08-15 bottle-CDN outage was absorbed on the PR path and would still have
# taken the release path down -- a guard that protects the cheap job and not
# the expensive one. One script, three call sites, one place to fix.
#
# `brew trust` is not decorative: this runner's Homebrew refuses to load a
# formula from a newly-added third-party tap until the tap is explicitly
# trusted ("Refusing to load formula libkrun/krun/libkrun from untrusted tap
# libkrun/krun", first seen 2026-07-30).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GUARD="$HERE/guard-fetch.sh"

# -t 900 for both network steps: this step's worst SUCCESSFUL run in CI history
# is 192s (n=138, p50 117s), and the rule is 4x that, rounded up the ladder.
# The measurement is step-level -- tap, trust and install are timed together --
# so each command inherits the step's bound rather than a split we never
# measured. Refs: MGIT-143 (the RULE, in guard-fetch.sh)
BOUND="${MGIT_BREW_LIBKRUN_TIMEOUT:-900}"

# CLAUSE 3, and a real one. `brew tap` CLONES the tap from GitHub. A clone that
# dies part-way leaves the tap directory on disk, and `brew tap` then treats the
# tap as already present and does not re-clone -- the same existence-check trap
# that made the libkrunfw retry fail three times identically. Removing the
# directory is what makes attempt 2 an attempt rather than a repeat.
"$GUARD" -t "$BOUND" -l brew-tap-libkrun \
	-c 'rm -rf "$(brew --repository)/Library/Taps/libkrun/homebrew-krun"' -- \
	brew tap libkrun/krun

# fetch-guard: local only -- it records trust for a tap already on disk and
# makes no network request. Refs: MGIT-143
brew trust libkrun/krun

# Bottles come from a third-party CDN. On 2026-08-15 that CDN returned 504 for
# over half an hour (`Failed to download resource "virglrenderer"`, ultimately a
# gitlab.freedesktop.org outage) and put main red three times with nothing wrong
# in this repository. A job that goes red on someone else's CDN teaches the team
# to re-run reds without reading them, which is how a REAL failure eventually
# gets waved through. Refs: MGIT-119
"$GUARD" -t "$BOUND" -l brew-install-libkrun \
	-c none:'brew verifies the checksum of everything it reuses and keeps a partial download as .incomplete, which it resumes rather than re-extracting; clearing the cache here would also defeat the MGIT-119 downloads cache that makes this fetch survivable in the first place' -- \
	brew install libkrun
