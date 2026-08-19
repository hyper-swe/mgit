package branchguard

import (
	"fmt"
	"strings"
)

// maxListedFiles caps the out-of-scope file list. The list has to be long
// enough to recognize the foreign work in it and short enough to read at a
// push prompt; the incident's six files fit whole, and a rewrite of fifty is
// recognizable from its first dozen. Refs: MGIT-142
const maxListedFiles = 12

// Refusal renders the message that blocks the push. It has to survive being
// read by someone mid-push who wants to get on with their day, so it answers
// three questions in order: what is in my branch that I did not put there,
// which files will show up in the pull request, and what do I type now.
// Refs: MGIT-142, MGIT-131
func Refusal(r *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BRANCH SCOPE REFUSED: %s carries another branch's commits\n\n", r.Branch)
	fmt.Fprintf(&b, "  Base: %s\n", strings.Join(r.Bases, ", "))
	for _, in := range r.Inherited {
		writeInherited(&b, in)
	}
	fmt.Fprintf(&b, "\n  %s in this branch's diff came from there, not from your work:\n", countOf(len(r.Files), "file"))
	writeFiles(&b, r.Files)
	b.WriteString("\n" + explanation + "\n")
	writeRemedies(&b, r)
	return b.String()
}

const explanation = "  `git checkout -b` inherits whatever branch you were standing on. The pull\n" +
	"  request will show these files and its commit message will not mention them:\n" +
	"  that is how 9abf4ce reached main as a 24-line CI fix carrying 531 lines."

// writeInherited names one parent branch and the commits it contributed.
// Refs: MGIT-142
func writeInherited(b *strings.Builder, in Inherited) {
	fmt.Fprintf(b, "  From: %s", in.Refs[0])
	if len(in.Refs) > 1 {
		fmt.Fprintf(b, " (also %s)", strings.Join(in.Refs[1:], ", "))
	}
	fmt.Fprintf(b, " — %s not in the base and not yours:\n", countOf(len(in.Commits), "commit"))
	for _, c := range in.Commits {
		fmt.Fprintf(b, "        %s  %s\n", shortHash(c.Hash), c.Subject)
	}
}

// writeFiles prints the out-of-scope paths, truncated so the remedy stays on
// screen. Refs: MGIT-142
func writeFiles(b *strings.Builder, files []string) {
	for i, f := range files {
		if i == maxListedFiles {
			fmt.Fprintf(b, "        ... and %d more\n", len(files)-maxListedFiles)
			break
		}
		fmt.Fprintf(b, "        %s\n", f)
	}
}

// writeRemedies prints the three ways out, cheapest first, with the recorded
// override named last and in full — a guard whose override is undiscoverable
// gets disabled wholesale. Refs: MGIT-142, MGIT-131
func writeRemedies(b *strings.Builder, r *Result) {
	parent, base := r.Inherited[0].Refs[0], r.Bases[0]
	fmt.Fprintf(b, "\n  Replay your own commits onto the base:\n")
	fmt.Fprintf(b, "      git rebase --onto %s %s %s\n", base, parent, r.Branch)
	fmt.Fprintf(b, "  Or, if this stack is deliberate, declare the parent for this run:\n")
	fmt.Fprintf(b, "      make branch-check BASE=%s\n", parent)
	fmt.Fprintf(b, "      go run ./scripts/branchguard --base %s\n", parent)
	fmt.Fprintf(b, "  Or record WHY it must ship this way — the reason travels in the pull\n")
	fmt.Fprintf(b, "  request, so it is reviewed rather than assumed:\n")
	fmt.Fprintf(b, "      git commit --amend --trailer %q\n", OverrideTrailer+" <why this branch needs "+parent+">")
}

// OverrideNotice renders what is printed when a recorded waiver lets the push
// through: the same facts as the refusal, plus who waived them and why. It is
// deliberately not silent — an override nobody sees is the guard being off.
// Refs: MGIT-142
func OverrideNotice(r *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BRANCH SCOPE OVERRIDDEN: %s carries %s and %s from %s\n",
		r.Branch, countOf(inheritedCommitCount(r), "commit"), countOf(len(r.Files), "file"),
		strings.Join(parentNames(r), ", "))
	fmt.Fprintf(&b, "  Reason:   %s\n", r.Override.Reason)
	fmt.Fprintf(&b, "  Recorded: %s (%s), so the reviewer reads it on the pull request.\n",
		shortHash(r.Override.Commit.Hash), OverrideTrailer)
	return b.String()
}

func parentNames(r *Result) []string {
	names := make([]string, 0, len(r.Inherited))
	for _, in := range r.Inherited {
		names = append(names, in.Refs[0])
	}
	return names
}

func inheritedCommitCount(r *Result) int {
	n := 0
	for _, in := range r.Inherited {
		n += len(in.Commits)
	}
	return n
}

// countOf renders "1 file" / "6 files" so the message reads like English.
func countOf(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}
