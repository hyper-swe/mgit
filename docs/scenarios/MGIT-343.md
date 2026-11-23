# MGIT-343: the branch-scope corpus test and the branches a clone happens to hold

1. CI checks out full history, so the clone holds every remote branch, merge-train
   branches included.
2. A merge train merges pull request A onto main as its own commit. A second train
   merges pull request B on top of that first train. Main takes A as a different
   commit, so the first train's commit never reaches main while both trains exist.
3. Before: the scope rule refuses the second train, because it carries the first
   train's commit, and `TestSurvey_ThisRepositoryRealBranches_RefusesOnlyKnownIncidents`
   failed on main. Whether main was green depended on which branches had been pushed.
4. After: the rule is unchanged. Its answer on this shape is pinned by
   `TestSurvey_ATrainBuiltOnAnotherUnmergedTrainIsRefusedAndNamesIt`, which builds
   its own repository: the chained train is refused and names the train under it,
   and the first train and a train cut from main are clean.
5. The live survey still runs over the clone's branches. By default it reports an
   unknown refusal in the test log and passes. With `MGIT_LIVE_BRANCH_SURVEY=1` it
   fails on one, as before. `make branch-survey` is the same measurement as a command.
