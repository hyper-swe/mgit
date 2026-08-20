package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Message-source flags, shared by every verb that records a message the caller
// composed: commit (MGIT-105), squash and merge (MGIT-106).
//
// They live here rather than beside any one command because the contract they
// implement is a byte-integrity guarantee, and three divergent copies of a
// byte-integrity guarantee is how one of them quietly stops matching.
// Refs: FR-2.9, FR-8.3, MGIT-105, MGIT-106

// bindMessageFlags registers the -m/--message and -F/--file pair on cmd.
//
// noun names the record in the flag's help and in every refusal ("commit",
// "squash", "merge"); inlineUsage is the one-line help for -m, which differs
// per verb because what an empty message falls back to differs per verb.
// Refs: MGIT-105, MGIT-106
func bindMessageFlags(cmd *cobra.Command, message, file *string, noun, inlineUsage string) {
	cmd.Flags().StringVarP(message, "message", "m", "", inlineUsage)
	// No backticks in this usage string: pflag's UnquoteUsage would read the
	// backticked span as the flag's value placeholder. Refs: MGIT-105
	cmd.Flags().StringVarP(file, "file", "F", "",
		"Read the "+noun+" message verbatim from a file, or from stdin when the path is - "+
			"(mutually exclusive with -m)")
}

// resolveMessage returns the message the caller asked to record.
//
// -m/--message supplies it inline; --file/-F reads it from a file, or from
// stdin when the path is "-". A message read from a file is taken as BYTES and
// recorded verbatim: no trimming, no normalization, no interpretation of the
// content. Trailing newlines and internal blank lines survive, so the recorded
// message round-trips byte-identical to the file. That is the point of
// MGIT-105: a message routed through the shell as -m "$(cat file)" makes the
// SHELL responsible for the integrity of an audit artifact, and the shell's
// failure modes are silent truncation and mangling, not a loud refusal.
// (git-compatible comment stripping would belong behind a -t flag, never here.)
//
// It matters more for squash than it did for commit: the squash message is the
// one message that leaves mgit's store for the user's real git via --to-git,
// so a quoting accident there escapes into a repository mgit does not own.
//
// Passing both sources is refused naming both flags: silently preferring one
// would let the caller believe it recorded one thing while the record said
// another — the same defect class. An empty file is refused for that reason
// too, because the verb would substitute a generated message for the one the
// caller supplied.
//
// Callers must invoke this as the FIRST act of RunE, before the repository is
// opened or anything is staged: a failure here must leave zero partial state.
// Refs: FR-2.9, FR-7, FR-8.3, MGIT-105, MGIT-106
func resolveMessage(cmd *cobra.Command, noun, inline, path string) (string, error) {
	if !cmd.Flags().Changed("file") {
		return inline, nil
	}
	if cmd.Flags().Changed("message") {
		return "", fmt.Errorf("--message/-m and --file/-F are mutually exclusive: "+
			"pass the %s message inline or from a file, not both", noun)
	}
	data, err := readMessageFile(cmd, noun, path)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("--file %s: the %s message is empty — "+
			"refusing to record a generated message in its place", path, noun)
	}
	return string(data), nil
}

// readMessageFile reads a message as raw bytes from path, or from the command's
// stdin when path is "-". Stdin is the path a programmatic caller uses to avoid
// a temp file entirely. Any read failure is returned before the repository is
// touched, so nothing is recorded. Refs: FR-2.9, MGIT-105, MGIT-106
func readMessageFile(cmd *cobra.Command, noun, path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("--file -: read %s message from stdin: %w", noun, err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // user-supplied message file path
	if err != nil {
		return nil, fmt.Errorf("--file: read %s message from %s: %w", noun, path, err)
	}
	return data, nil
}
