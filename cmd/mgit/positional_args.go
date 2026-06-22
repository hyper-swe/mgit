package main

// firstNonEmpty returns positional if it is non-empty, otherwise flag. It
// standardizes the positional-vs-flag convention used by commands that accept
// a commit hash or task ID both ways: an explicit positional wins over the
// equivalent flag. Refs: MGIT-23
func firstNonEmpty(positional, flag string) string {
	if positional != "" {
		return positional
	}
	return flag
}

// argAt returns args[i] or "" when the index is out of range, for safe
// optional-positional extraction. Refs: MGIT-23
func argAt(args []string, i int) string {
	if i < 0 || i >= len(args) {
		return ""
	}
	return args[i]
}
