# HSWE-17.80.4.2: `--env-name` sets a named variable in the guest

1. A caller has a variable `BUILD_MODE` in its environment and runs
   `mgit run --env-name BUILD_MODE -- ./build.sh` (or
   `mgit sandbox exec --task-id T --env-name BUILD_MODE -- ./build.sh`).
2. The command runs in the guest with `BUILD_MODE` set to the caller's value.
   Only the named variables are passed; the rest of the caller's environment is
   not.
3. A variable that is not set, is empty, has an invalid name, is named twice, is
   also given with `--env`, or is too large is refused before mgit connects to
   anything. The message names the variable and does not show its value.
4. `mgit run --capabilities=json` prints `{"capabilities":["exec-env-name/v1"]}`
   and nothing else, without looking up a task or starting a daemon. Any other
   format is an error. A caller that gets an error, or no such line, treats the
   flag as unsupported.
5. On the container backend, a variable whose name or value it cannot carry (a name
   starting with `#` or space, a name with no `=`, or a value holding a line break)
   is refused by variable name.
6. `--env-name` cannot be combined with `--check` or `--capabilities`.
7. Unchanged: `--env KEY=VALUE` behaves as before.
