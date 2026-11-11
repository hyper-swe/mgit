# HSWE-17.62.1.1: a committed nested store no longer blocks a new task

1. A project's git history committed another store, a `.mgit` folder, one folder
   down (`archive/.mgit`) and three folders down (`a/b/c/.mgit`), each with an
   ordinary committed file beside it (`archive/keep.go`, `a/b/c/keep.go`).
2. The user runs `mgit work <path> --task-id <id>` (or `mgit worktree add`).
3. Before: the command stopped with
   `snapshot a/b/c/.mgit/more/payload-three: path is in an excluded directory`
   and no worktree was created.
4. After: the worktree is created. The store's files are not in it; the files
   beside the stores are. The command prints one line per skipped folder, sorted,
   whatever the store held. These lines are from
   `TestE2E_WorkWithCommittedNestedStores_SkipsThemAndNamesEach`'s logged output:

   ```
   Skipped nested snapshot data in a/b/c/.mgit
   Skipped nested snapshot data in archive/.mgit
   ```

5. Nothing inside a skipped folder is printed, and the committed files beside
   the stores are not reported as missing from the checkout.
6. Unchanged: the root `.mgit`, a committed file that is itself named `.mgit`,
   and a nested `.git` are still refused as before; a name that only starts with
   `.mgit` is ordinary content; `--include-uncommitted` uses the checkout's live
   walk, which is untouched.
