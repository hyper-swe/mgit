# A known formula is not necessarily installed

On a fresh macOS runner with Homebrew, libkrun can have a prospective prefix
even when there is no installed keg. Run `scripts/brew-install-no-libkrun.sh`
on a disposable runner without libkrun, libkrunfw or the third-party tap.
The check should pass the absence precondition, install the local mgit formula,
verify libkrunfw, and exercise a core commit. If libkrun is already installed,
the check must refuse before installing anything.

The packaging regression test models the prospective prefix with libkrun absent
and libkrunfw already installed. The script must reach the libkrunfw refusal;
an earlier refusal about libkrun is a false positive.
