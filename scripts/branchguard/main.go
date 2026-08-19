// Command branchguard refuses a branch that carries another branch's commits,
// before that branch becomes a pull request.
//
// It is the executable half of internal/branchguard, wired into the pre-push
// hook (scripts/hooks/pre-push) and available as `make branch-check`. The
// package doc explains the rule and the incident it exists for; this file only
// turns a repository and some flags into an exit code.
//
// Usage:
//
//	go run ./scripts/branchguard                      # check the current branch
//	go run ./scripts/branchguard --branch feat/x      # check another branch
//	go run ./scripts/branchguard --base fix/parent    # declare a deliberate stack
//	go run ./scripts/branchguard --survey             # measure the rule over every branch
//
// Exit status is 2 when a branch is refused, 1 on a usage or repository error,
// and 0 when the branch is clean or a recorded override applies.
//
// Refs: MGIT-142
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	gogit "github.com/go-git/go-git/v5"

	"github.com/hyper-swe/mgit/internal/branchguard"
)

const (
	exitRefused = 2
	exitError   = 1
)

// baseList collects a repeatable --base flag.
type baseList []string

func (b *baseList) String() string { return strings.Join(*b, ",") }

func (b *baseList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*b = append(*b, part)
		}
	}
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var bases baseList
	fs := flag.NewFlagSet("branchguard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoPath := fs.String("repo", ".", "repository to inspect")
	branch := fs.String("branch", "", "branch to check (default: the checked-out branch)")
	survey := fs.Bool("survey", false, "check every branch and report how many would be refused")
	fs.Var(&bases, "base", "base ref this branch was cut from (repeatable; default main, origin/main)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	repo, err := gogit.PlainOpenWithOptions(*repoPath, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		fmt.Fprintf(stderr, "branchguard: open %s: %v\n", *repoPath, err)
		return exitError
	}
	opts := branchguard.Options{Branch: *branch, Bases: bases}
	if *survey {
		return runSurvey(repo, opts, stdout, stderr)
	}
	return runCheck(repo, opts, stderr)
}

// runCheck is the pre-push path: silent when the branch is clean, loud and
// blocking when it is not. Refs: MGIT-142
func runCheck(repo *gogit.Repository, opts branchguard.Options, stderr io.Writer) int {
	res, err := branchguard.Check(repo, opts)
	if err != nil {
		fmt.Fprintf(stderr, "branchguard: %v\n", err)
		return exitError
	}
	switch {
	case res.Clean():
		return 0
	case res.Overridden():
		fmt.Fprint(stderr, branchguard.OverrideNotice(res))
		return 0
	default:
		fmt.Fprint(stderr, branchguard.Refusal(res))
		return exitRefused
	}
}

// runSurvey measures the rule against the repository's real branches and
// prints the false-positive count as a number. Refs: MGIT-142, MGIT-131
func runSurvey(repo *gogit.Repository, opts branchguard.Options, stdout, stderr io.Writer) int {
	results, err := branchguard.Survey(repo, opts)
	if err != nil {
		fmt.Fprintf(stderr, "branchguard: %v\n", err)
		return exitError
	}
	refused := 0
	for _, res := range results {
		if res.Clean() {
			continue
		}
		refused++
		fmt.Fprintf(stdout, "REFUSED  %-45s %d commit(s), %d file(s) from %s%s\n",
			res.Branch, commitCount(res), len(res.Files), parents(res), overrideNote(res))
	}
	fmt.Fprintf(stdout, "\n%d branches surveyed against %s, %d refused\n",
		len(results), strings.Join(orDefaultBases(opts.Bases), "+"), refused)
	return 0
}

func commitCount(res *branchguard.Result) int {
	n := 0
	for _, in := range res.Inherited {
		n += len(in.Commits)
	}
	return n
}

func parents(res *branchguard.Result) string {
	names := make([]string, 0, len(res.Inherited))
	for _, in := range res.Inherited {
		names = append(names, in.Refs[0])
	}
	return strings.Join(names, ", ")
}

func overrideNote(res *branchguard.Result) string {
	if res.Overridden() {
		return "  [overridden: " + res.Override.Reason + "]"
	}
	return ""
}

func orDefaultBases(bases []string) []string {
	if len(bases) == 0 {
		return branchguard.DefaultBases
	}
	return bases
}
