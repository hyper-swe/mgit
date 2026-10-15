package git

import "bytes"

// A block mgit (or a tool that shares its convention) writes into a generated
// file is delimited by a begin marker and an end marker, in whatever comment
// syntax the file uses (<!-- -->, #). The label after BEGIN is free text, so
// a COMPLETE span is the definition: BEGIN mgit-sandbox ... END mgit-sandbox.
// A begin marker with no end is not a span and is left alone. A test pins the
// phrases to what internal/agentadapter writes. Refs: MGIT-126, MGIT-11.11.2
const (
	generatedBlockBegin = "BEGIN mgit-sandbox"
	generatedBlockEnd   = "END mgit-sandbox"
)

// StripGeneratedBlock returns content without every complete mgit-sandbox
// span. Everything else is kept byte for byte: text on the begin line before
// the comment opener, text on the end line after the closer, and line endings
// (LF or CRLF). When the span began at the start of a line and nothing follows
// it on its last line, the whole lines go; when the block was the last thing in
// the file, the one blank line that set it apart from the text above goes too.
// Refs: MGIT-126
func StripGeneratedBlock(content []byte) []byte {
	out := content
	for {
		next, changed := stripFirstSpan(out)
		if !changed {
			return out
		}
		out = next
	}
}

func stripFirstSpan(c []byte) ([]byte, bool) {
	b := bytes.Index(c, []byte(generatedBlockBegin))
	if b < 0 {
		return c, false
	}
	e := bytes.Index(c[b:], []byte(generatedBlockEnd))
	if e < 0 {
		return c, false // unterminated: never half-removed
	}
	e += b

	// Span start: the comment opener just before BEGIN, on the same line.
	lineStart := bytes.LastIndexByte(c[:b], '\n') + 1
	start := b
	pre := bytes.TrimRight(c[lineStart:b], " \t")
	closer := ""
	switch {
	case bytes.HasSuffix(pre, []byte("<!--")):
		start = lineStart + len(pre) - len("<!--")
		closer = "-->"
	case bytes.HasSuffix(pre, []byte("#")):
		start = lineStart + len(pre) - 1
	}

	// Span end: past END and, for <!-- ... -->, past the closer.
	end := e + len(generatedBlockEnd)
	if closer != "" {
		rest := end
		for rest < len(c) && (c[rest] == ' ' || c[rest] == '\t') {
			rest++
		}
		if bytes.HasPrefix(c[rest:], []byte(closer)) {
			end = rest + len(closer)
		}
	}

	head := c[:start]
	tail := c[end:]
	startsLine := len(bytes.TrimLeft(c[lineStart:start], " \t")) == 0
	lineEnd := bytes.IndexByte(tail, '\n')
	var restOfLine []byte
	if lineEnd < 0 {
		restOfLine = tail
	} else {
		restOfLine = tail[:lineEnd]
	}
	endsLine := len(bytes.TrimSpace(restOfLine)) == 0

	if startsLine && endsLine {
		// Whole lines: drop from the start of the begin line through the end
		// of the end line, line terminator included.
		head = c[:lineStart]
		if lineEnd < 0 {
			tail = nil
		} else {
			tail = tail[lineEnd+1:]
		}
		if len(tail) == 0 {
			head = trimOneBlankLine(head)
		}
	}
	return append(append([]byte{}, head...), tail...), true
}

// trimOneBlankLine removes the final line terminator when the text ends in a
// blank line (LF or CRLF), so a block that was appended after a blank line
// leaves the text above it as it was.
func trimOneBlankLine(h []byte) []byte {
	switch {
	case bytes.HasSuffix(h, []byte("\r\n\r\n")):
		return h[:len(h)-2]
	case bytes.HasSuffix(h, []byte("\n\n")):
		return h[:len(h)-1]
	}
	return h
}
