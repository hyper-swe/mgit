package git

import "bytes"

// A block mgit (or a tool that shares its convention) writes into a generated
// file is delimited by a begin line and an end line, in the comment syntax of
// the file (<!-- -->, #). The label after BEGIN is free text, so a COMPLETE span
// is the definition: a begin line, then an end line. A marker counts only when
// it STARTS its line (after whitespace and an optional comment opener), so prose
// that mentions the phrases is never a marker. A begin with no end is left
// alone, a stray end is left alone, and when a second begin follows an earlier
// one before the end, the span opens at the LAST begin (the earlier is not a
// block). A test pins the phrases to what internal/agentadapter writes.
// Refs: MGIT-126, MGIT-11.11.2
const (
	generatedBlockBegin = "BEGIN mgit-sandbox"
	generatedBlockEnd   = "END mgit-sandbox"
)

// StripGeneratedBlock returns content without every complete mgit-sandbox
// span. Everything else is kept byte for byte, including line endings (LF or
// CRLF) and any text that follows the end marker on its line. When a span is the
// last thing in the file, the one blank line that set it apart from the text
// above goes with it. Refs: MGIT-126
func StripGeneratedBlock(content []byte) []byte {
	type span struct{ start, end int } // byte offsets; end is exclusive
	var spans []span

	lastBegin := -1
	for off := 0; off < len(content); {
		nl := bytes.IndexByte(content[off:], '\n')
		lineEnd := len(content)
		next := len(content)
		if nl >= 0 {
			lineEnd = off + nl
			next = lineEnd + 1
		}
		line := content[off:lineEnd]
		if _, ok := markerAfter(line, generatedBlockBegin); ok {
			lastBegin = off
		} else if after, ok := markerAfter(line, generatedBlockEnd); ok && lastBegin >= 0 {
			rest := line[after:]
			end := off + after
			if len(bytes.TrimSpace(rest)) == 0 {
				end = next // nothing else on the end line: take the line terminator too
			}
			spans = append(spans, span{lastBegin, end})
			lastBegin = -1
		}
		off = next
	}
	if len(spans) == 0 {
		return content
	}

	var out []byte
	pos := 0
	for _, sp := range spans {
		out = append(out, content[pos:sp.start]...)
		pos = sp.end
	}
	out = append(out, content[pos:]...)
	if pos >= len(content) {
		out = trimOneBlankLine(out) // the block was the last thing in the file
	}
	return out
}

// markerAfter reports whether line starts with the marker word (after leading
// whitespace and an optional comment opener), and returns the offset just past
// the marker, including a trailing "-->" when the opener was "<!--".
func markerAfter(line []byte, word string) (int, bool) {
	i := skipBlanks(line, 0)
	opener := ""
	switch {
	case bytes.HasPrefix(line[i:], []byte("<!--")):
		opener = "<!--"
		i = skipBlanks(line, i+len("<!--"))
	case bytes.HasPrefix(line[i:], []byte("#")):
		opener = "#"
		i = skipBlanks(line, i+1)
	}
	if !bytes.HasPrefix(line[i:], []byte(word)) {
		return 0, false
	}
	end := i + len(word)
	if word == generatedBlockBegin {
		// The begin line carries a free label, so it is the whole line; only the
		// start matters here.
		return len(line), true
	}
	if opener == "<!--" {
		j := skipBlanks(line, end)
		if bytes.HasPrefix(line[j:], []byte("-->")) {
			end = j + len("-->")
		}
	}
	return end, true
}

func skipBlanks(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	return i
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
