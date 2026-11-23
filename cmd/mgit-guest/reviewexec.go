package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hyper-swe/mgit/internal/reviewlaunch"
)

// reviewExec runs the one pinned entrypoint of the reviewer-ro profile:
//
//	mgit-guest review-exec --sha256 <hex> [--uid N --gid N] [--dir D] -- <entrypoint> [args...]
//
// A refusal prints the mgit.review-error/v1 envelope on stderr and exits 70;
// otherwise the exit status is the entrypoint's. Refs: FEAT-3.347.1
func reviewExec(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// The nonce is for the refusal envelope only; take it out of the
	// environment so nothing started from here can inherit it.
	nonce := os.Getenv(reviewlaunch.RefusalNonceEnv)
	_ = os.Unsetenv(reviewlaunch.RefusalNonceEnv)
	fs := flag.NewFlagSet("review-exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sha := fs.String("sha256", "", "lower-case hex SHA-256 of the entrypoint")
	uid := fs.Int("uid", -1, "unprivileged uid to run as (required when started as root)")
	gid := fs.Int("gid", -1, "unprivileged gid to run as")
	dir := fs.String("dir", "", "working directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "review-exec: an entrypoint path is required")
		return 2
	}
	cfg := reviewlaunch.Config{Entrypoint: rest[0], SHA256: *sha, Argv: rest[1:], Dir: *dir, Stdin: stdin, Stdout: stdout, Stderr: stderr}
	if *uid >= 0 {
		g := *gid
		if g < 0 {
			g = *uid
		}
		cfg.DropTo = &reviewlaunch.Identity{UID: *uid, GID: g}
	}
	code, err := reviewlaunch.Run(cfg)
	if err != nil {
		var r *reviewlaunch.RefusalError
		if errors.As(err, &r) {
			_, _ = stderr.Write(append(reviewlaunch.RefusalJSONWithNonce(r, nonce), '\n'))
			return 70
		}
		fmt.Fprintln(stderr, err)
		return 70
	}
	return code
}
