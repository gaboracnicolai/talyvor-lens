package distill

import "strings"

// fencedBlock wraps content in a fenced code block whose fence is LONGER than
// any backtick run inside content (the CommonMark rule). This prevents
// fence-break-out: untrusted content containing its own ``` sequence cannot
// escape the block and inject Markdown/instructions downstream.
func fencedBlock(lang, content string) string {
	n := longestBacktickRun(content) + 1
	if n < 3 {
		n = 3
	}
	fence := strings.Repeat("`", n)
	return fence + lang + "\n" + content + "\n" + fence
}

// codeSpan wraps s as an inline code span, using enough backticks to contain
// any backtick run inside s, and padding with a space when s begins or ends
// with a backtick (also CommonMark).
func codeSpan(s string) string {
	ticks := strings.Repeat("`", longestBacktickRun(s)+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || s == "" {
		pad = " "
	}
	return ticks + pad + s + pad + ticks
}

func longestBacktickRun(s string) int {
	longest, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	return longest
}

// mdTable renders a header row + body rows as a GitHub pipe table. Cells are
// sanitized so a stray '|' or newline can't break the table structure. An
// empty header with no rows yields "".
func mdTable(header []string, rows [][]string) string {
	if len(header) == 0 && len(rows) == 0 {
		return ""
	}
	width := len(header)
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	if width == 0 {
		return ""
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteByte('|')
		for i := 0; i < width; i++ {
			cell := ""
			if i < len(cells) {
				cell = mdCell(cells[i])
			}
			b.WriteByte(' ')
			b.WriteString(cell)
			b.WriteByte(' ')
			b.WriteByte('|')
		}
		b.WriteByte('\n')
	}
	writeRow(header)
	// Separator row.
	b.WriteByte('|')
	for i := 0; i < width; i++ {
		b.WriteString(" --- |")
	}
	b.WriteByte('\n')
	for _, r := range rows {
		writeRow(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// mdCell makes a string safe to drop inside a pipe-table cell: collapse
// newlines, escape pipes, trim.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}

// normalizeText canonicalizes whitespace for passthrough/plaintext output:
// CRLF/CR → LF, trailing spaces stripped per line, runs of 3+ blank lines
// collapsed to one blank line, and a single trailing newline removed.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	var out []string
	blanks := 0
	for _, ln := range lines {
		if ln == "" {
			blanks++
			if blanks > 1 {
				continue
			}
		} else {
			blanks = 0
		}
		out = append(out, ln)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}
