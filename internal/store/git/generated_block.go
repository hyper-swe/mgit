package git

import "bytes"

// The block mgit writes into a generated file is delimited by a begin line and
// an end line, in whatever comment syntax the file uses (<!-- -->, #). The same
// two phrases are written by internal/agentadapter; a test pins them together.
// Refs: MGIT-126, MGIT-11.11.2
const (
	generatedBlockBegin = "BEGIN mgit-sandbox (generated; do not edit)"
	generatedBlockEnd   = "END mgit-sandbox"
)

// StripGeneratedBlock returns content without mgit's generated block: the begin
// line through the end line, inclusive, and the one blank line that separated the
// block from the text above it. Content with no complete block is returned
// unchanged, as is content with a begin line and no end line, so a damaged
// block is never half-removed. Refs: MGIT-126
func StripGeneratedBlock(content []byte) []byte {
	lines := bytes.SplitAfter(content, []byte("\n"))
	begin, end := -1, -1
	for i, l := range lines {
		switch {
		case begin < 0 && bytes.Contains(l, []byte(generatedBlockBegin)):
			begin = i
		case begin >= 0 && bytes.Contains(l, []byte(generatedBlockEnd)):
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if begin < 0 || end < 0 {
		return content
	}
	head := bytes.Join(lines[:begin], nil)
	tail := bytes.Join(lines[end+1:], nil)
	if len(tail) == 0 && bytes.HasSuffix(head, []byte("\n\n")) {
		head = head[:len(head)-1] // the blank line that set the block apart
	}
	return append(append([]byte{}, head...), tail...)
}
