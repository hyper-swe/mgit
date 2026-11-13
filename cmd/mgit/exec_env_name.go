// The --env-name flag of the verbs that run a command in a guest, and the
// capability report a caller reads to learn the flag exists. Split from run.go
// and sandbox_exec.go, which both bind the same flag. Refs: FR-17.3, FR-17.11
package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hyper-swe/mgit/internal/controlproto"
)

// capabilityExecEnvName names the version of --env-name a caller can rely on.
const capabilityExecEnvName = "exec-env-name/v1"

// envNameGrammar is the portable shape of an environment variable name.
var envNameGrammar = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// bindEnvNameFlag adds --env-name to a verb that runs a guest command.
func bindEnvNameFlag(cmd *cobra.Command, names *[]string) {
	cmd.Flags().StringArrayVar(names, "env-name", nil,
		"name of a variable to read from this process's environment and set in the guest "+
			"(repeatable; only the named variables are passed)")
}

// takeEnvNames resolves each named variable from this process's environment
// into a KEY=VALUE entry appended to the explicit --env entries, and then
// removes the variable from the environment, so a helper process started
// afterward (the daemon) cannot inherit it.
//
// Every refusal names the variable and never shows a value, and none connects
// to anything: the whole request is checked before the first lookup is acted on.
func takeEnvNames(names, explicit []string) ([]string, error) {
	if len(names) == 0 {
		return explicit, nil
	}
	given := map[string]bool{}
	for _, e := range explicit {
		k, _, _ := strings.Cut(e, "=")
		given[k] = true
	}
	entries := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		switch {
		case !envNameGrammar.MatchString(n):
			return nil, fmt.Errorf("--env-name %q is not a valid variable name", n)
		case seen[n]:
			return nil, fmt.Errorf("--env-name %s was given more than once", n)
		case given[n]:
			return nil, fmt.Errorf("%s is given by both --env and --env-name", n)
		}
		seen[n] = true
		v, ok := os.LookupEnv(n)
		switch {
		case !ok || v == "":
			return nil, fmt.Errorf("--env-name %s: the variable is not set (or is empty) in this environment", n)
		case len(n)+1+len(v) > controlproto.MaxEnvEntryBytes:
			return nil, fmt.Errorf("--env-name %s: the value is larger than the %d-byte limit", n, controlproto.MaxEnvEntryBytes)
		}
		entries = append(entries, n+"="+v)
	}
	if len(explicit)+len(entries) > controlproto.MaxEnv {
		return nil, fmt.Errorf("too many environment entries (limit %d)", controlproto.MaxEnv)
	}
	for _, n := range names {
		_ = os.Unsetenv(n)
	}
	return append(append([]string{}, explicit...), entries...), nil
}

// writeCapabilities prints the capability report for format, which must be
// "json". It touches no sandbox, task or daemon.
func writeCapabilities(w io.Writer, format string) error {
	if format != "json" {
		return fmt.Errorf("--capabilities: unsupported format %q (use json)", format)
	}
	_, err := fmt.Fprintf(w, "{\"capabilities\":[%q]}\n", capabilityExecEnvName)
	return err
}
