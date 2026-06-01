package distill

import (
	"bytes"
	"context"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlConverter turns HTML into clean Markdown using the pure-Go x/net/html
// tokenizer. It preserves the structure that helps an LLM (headings, lists,
// pipe tables, code blocks, inline emphasis/links) and strips the rest. Active
// content is dropped by construction: <script>/<style>/<head>/<noscript>/
// <template>/<svg>/<iframe>/<object>/<embed> subtrees are skipped, and because
// the renderer emits only text + structural Markdown (never element
// attributes), event-handler attributes (onclick=…) and the like simply never
// reach the output. Link hrefs are scheme-sanitized (javascript:/data:/etc.
// dropped). This is the incidental injection-surface reduction the design
// calls out.
//
// All tree walks are depth-bounded (htmlMaxDepth): a deeply-nested document
// cannot exhaust the goroutine stack (which is a FATAL, unrecoverable crash in
// Go) — past the bound the walk stops, degrading gracefully instead.
type htmlConverter struct{}

// htmlMaxDepth bounds DOM-traversal recursion. Far deeper than any
// human-authored document; the cap exists only to defang pathological nesting.
const htmlMaxDepth = 1000

func (htmlConverter) Format() Format { return FormatHTML }

func (htmlConverter) Convert(ctx context.Context, input []byte) (Result, error) {
	doc, err := html.Parse(bytes.NewReader(input))
	if err != nil {
		return Result{Format: FormatHTML}, err
	}
	r := &htmlRenderer{ctx: ctx}
	root := findFirstElement(doc, atom.Body, 0)
	if root == nil {
		root = doc
	}
	r.walkBlocks(root, 0, 0)
	return Result{Markdown: normalizeText(r.blocks.String()), Format: FormatHTML}, nil
}

type htmlRenderer struct {
	ctx    context.Context
	blocks strings.Builder
}

func (r *htmlRenderer) writeBlock(s string) {
	s = strings.Trim(s, "\n")
	if s == "" {
		return
	}
	if r.blocks.Len() > 0 {
		r.blocks.WriteString("\n\n")
	}
	r.blocks.WriteString(s)
}

// walkBlocks renders block-level structure, recursing into generic containers.
func (r *htmlRenderer) walkBlocks(n *html.Node, listDepth, depth int) {
	if depth > htmlMaxDepth {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if r.ctx.Err() != nil {
			return
		}
		switch c.Type {
		case html.TextNode:
			if t := strings.TrimSpace(collapseWS(c.Data)); t != "" {
				r.writeBlock(t)
			}
		case html.ElementNode:
			r.block(c, listDepth, depth+1)
		}
	}
}

func (r *htmlRenderer) block(n *html.Node, listDepth, depth int) {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Head, atom.Noscript, atom.Template, atom.Svg, atom.Iframe, atom.Object, atom.Embed:
		return // active / non-text content stripped
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		r.writeBlock(strings.Repeat("#", level) + " " + r.inline(n))
	case atom.P:
		r.writeBlock(r.inline(n))
	case atom.Hr:
		r.writeBlock("---")
	case atom.Pre:
		r.writeBlock(fencedBlock("", strings.Trim(textContent(n, 0), "\n")))
	case atom.Blockquote:
		r.writeBlock(quoteLines(r.inline(n)))
	case atom.Ul:
		r.writeBlock(r.list(n, false, listDepth, depth))
	case atom.Ol:
		r.writeBlock(r.list(n, true, listDepth, depth))
	case atom.Table:
		r.writeBlock(r.table(n))
	default:
		// Generic container (div/section/article/body/…) or unknown block.
		r.walkBlocks(n, listDepth, depth+1)
	}
}

// list renders ul/ol, indenting nested lists by two spaces per level.
func (r *htmlRenderer) list(n *html.Node, ordered bool, listDepth, depth int) string {
	if depth > htmlMaxDepth || listDepth > htmlMaxDepth {
		return ""
	}
	var b strings.Builder
	indent := strings.Repeat("  ", listDepth)
	i := 0
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.DataAtom != atom.Li {
			continue
		}
		i++
		marker := "- "
		if ordered {
			marker = itoa(i) + ". "
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(indent + marker + r.inline(c))
		for gc := c.FirstChild; gc != nil; gc = gc.NextSibling {
			if gc.Type == html.ElementNode && (gc.DataAtom == atom.Ul || gc.DataAtom == atom.Ol) {
				b.WriteByte('\n')
				b.WriteString(r.list(gc, gc.DataAtom == atom.Ol, listDepth+1, depth+1))
			}
		}
	}
	return b.String()
}

// table renders a <table> as a GitHub pipe table. The first row containing
// <th>, or the first row overall, becomes the header.
func (r *htmlRenderer) table(n *html.Node) string {
	var rows [][]string
	headerIdx := -1
	var collect func(*html.Node, int)
	collect = func(node *html.Node, depth int) {
		if depth > htmlMaxDepth {
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.DataAtom {
			case atom.Tr:
				var cells []string
				hasTH := false
				for cell := c.FirstChild; cell != nil; cell = cell.NextSibling {
					if cell.Type != html.ElementNode {
						continue
					}
					if cell.DataAtom == atom.Th || cell.DataAtom == atom.Td {
						if cell.DataAtom == atom.Th {
							hasTH = true
						}
						cells = append(cells, r.inline(cell))
					}
				}
				if len(cells) > 0 {
					if hasTH && headerIdx == -1 {
						headerIdx = len(rows)
					}
					rows = append(rows, cells)
				}
			default:
				collect(c, depth+1) // thead/tbody/tfoot wrappers
			}
		}
	}
	collect(n, 0)
	if len(rows) == 0 {
		return ""
	}
	if headerIdx == -1 {
		headerIdx = 0
	}
	header := rows[headerIdx]
	body := append(append([][]string{}, rows[:headerIdx]...), rows[headerIdx+1:]...)
	return mdTable(header, body)
}

// inline renders the inline content of a node: text + emphasis/code/links/br.
func (r *htmlRenderer) inline(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node, int)
	walk = func(node *html.Node, depth int) {
		if depth > htmlMaxDepth || r.ctx.Err() != nil {
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				b.WriteString(collapseWS(c.Data))
			case html.ElementNode:
				switch c.DataAtom {
				case atom.Script, atom.Style:
					// skip
				case atom.Br:
					b.WriteString("\n")
				case atom.Strong, atom.B:
					b.WriteString("**")
					walk(c, depth+1)
					b.WriteString("**")
				case atom.Em, atom.I:
					b.WriteString("*")
					walk(c, depth+1)
					b.WriteString("*")
				case atom.Code:
					b.WriteString(codeSpan(strings.TrimSpace(textContent(c, 0))))
				case atom.A:
					href := sanitizeHref(attrVal(c, "href"))
					txt := strings.TrimSpace(collapseWS(textContent(c, 0)))
					if href != "" && txt != "" {
						b.WriteString("[" + txt + "](" + href + ")")
					} else {
						b.WriteString(txt)
					}
				case atom.Img:
					if alt := strings.TrimSpace(attrVal(c, "alt")); alt != "" {
						b.WriteString(alt)
					}
				default:
					walk(c, depth+1)
				}
			}
		}
	}
	walk(n, 0)
	return strings.TrimSpace(collapseWS(b.String()))
}

// ── helpers ─────────────────────────────────────────────

func findFirstElement(n *html.Node, a atom.Atom, depth int) *html.Node {
	if depth > htmlMaxDepth {
		return nil
	}
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findFirstElement(c, a, depth+1); found != nil {
			return found
		}
	}
	return nil
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// textContent returns the concatenated text of a subtree, skipping
// script/style, with whitespace preserved (used for <pre>/<code>).
func textContent(n *html.Node, depth int) string {
	var b strings.Builder
	var walk func(*html.Node, int)
	walk = func(node *html.Node, d int) {
		if d > htmlMaxDepth {
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				b.WriteString(c.Data)
			case html.ElementNode:
				if c.DataAtom == atom.Script || c.DataAtom == atom.Style {
					continue
				}
				walk(c, d+1)
			}
		}
	}
	walk(n, depth)
	return b.String()
}

// collapseWS collapses every run of ASCII whitespace to a single space. Used
// for flowing text (not <pre>).
func collapseWS(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}

func quoteLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight("> "+ln, " ")
	}
	return strings.Join(lines, "\n")
}

// sanitizeHref drops dangerous URL schemes, keeping http(s)/mailto/tel/ftp and
// relative URLs. The output Markdown is consumed by a model, not a browser,
// but dropping javascript:/data:/vbscript: keeps conversion an honest
// injection-surface reducer. ASCII control characters (which attackers use to
// smuggle a scheme past naive checks, e.g. "java\tscript:") are stripped before
// the scheme is examined, so they cannot disguise a dangerous scheme.
func sanitizeHref(href string) string {
	h := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1 // drop C0 controls + DEL
		}
		return r
	}, strings.TrimSpace(href))
	if h == "" {
		return ""
	}
	lower := strings.ToLower(h)
	if i := strings.IndexAny(lower, ":/?#"); i >= 0 && lower[i] == ':' {
		switch lower[:i] {
		case "http", "https", "mailto", "tel", "ftp":
			return h
		default:
			return "" // javascript:, data:, vbscript:, file:, etc.
		}
	}
	return h // relative URL (no scheme)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
