package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/hyper-swe/mgit/internal/model"
)

// warnIfNoPatchIdentity tells the reader of a read-only patch (the preview
// behind `squash --to-git --dry-run`, and `export --format git`) that no git
// identity is configured, so the patch names no author, and how to set one.
// Those paths do not refuse: nothing is written. Only the writing `squash
// --to-git` refuses (option C, MGIT-237). Refs: MGIT-237
func warnIfNoPatchIdentity(app *App, w io.Writer) {
	if _, err := app.Squash.PatchAuthor(); errors.Is(err, model.ErrNoPatchIdentity) {
		_, _ = fmt.Fprintln(w, "warning: no git identity is configured, so this patch names no author "+
			"(git am will ask for one). Set one with `git config user.name \"Your Name\"` and "+
			"`git config user.email you@example.com`; `mgit squash --to-git` refuses until you do.")
	}
}
