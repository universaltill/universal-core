package help

import (
	"html/template"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RenderMarkdown turns a topic's markdown body into two related outputs
// from one parse: HTML for real rendering, and PlainText — the same
// content with markup markers stripped to bare words — for Search
// (index.go) to match against, not for display.
//
// This is a hand-rolled, deliberately narrow renderer — no new Markdown
// library dependency, the same "no new dependency for a narrow,
// controlled need" reasoning frontmatter.go's own doc comment gives
// (uc-infra#31/#95). It supports exactly: `#`/`##`/`###` headings,
// blank-line-separated paragraphs, **bold**, *italic*/_italic_,
// `inline code`, [text](url) links, and `-`/`*` unordered or `1.`
// ordered lists. Anything else — including a literal `<script>` typed
// into a topic's body — passes through as an escaped paragraph:
// html/template.HTMLEscapeString is applied to every run of literal
// text this parser doesn't recognize as one of the constructs above,
// defense in depth even though topic content is authored by this
// pipeline/Farshid, never arbitrary user input.
func RenderMarkdown(body string) (template.HTML, string) {
	blocks := parseBlocks(body)
	var h, p strings.Builder
	for _, b := range blocks {
		switch b.kind {
		case blockH1, blockH2, blockH3:
			tag := headingTag[b.kind]
			htmlText, plainText := renderInline(b.lines[0])
			h.WriteString("<" + tag + ">" + htmlText + "</" + tag + ">\n")
			if plainText != "" {
				p.WriteString(plainText + "\n\n")
			}
		case blockParagraph:
			htmlText, plainText := renderInline(b.lines[0])
			h.WriteString("<p>" + htmlText + "</p>\n")
			if plainText != "" {
				p.WriteString(plainText + "\n\n")
			}
		case blockUnordered, blockOrdered:
			tag := "ul"
			if b.kind == blockOrdered {
				tag = "ol"
			}
			h.WriteString("<" + tag + ">\n")
			for _, item := range b.lines {
				htmlText, plainText := renderInline(item)
				h.WriteString("<li>" + htmlText + "</li>\n")
				if plainText != "" {
					p.WriteString(plainText + "\n")
				}
			}
			h.WriteString("</" + tag + ">\n")
			p.WriteString("\n")
		}
	}
	return template.HTML(h.String()), strings.TrimSpace(p.String())
}

type blockKind int

const (
	blockParagraph blockKind = iota
	blockH1
	blockH2
	blockH3
	blockUnordered
	blockOrdered
)

var headingTag = map[blockKind]string{blockH1: "h1", blockH2: "h2", blockH3: "h3"}

type mdBlock struct {
	kind  blockKind
	lines []string // one entry for a heading/paragraph, one per item for a list
}

// orderedItemRe matches an ordered-list marker ("1. ", "12.  ", ...) at
// the start of a (whitespace-trimmed) line.
var orderedItemRe = regexp.MustCompile(`^\d+\.\s+`)

func isUnorderedItem(trimmed string) bool {
	return strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ")
}

func isOrderedItem(trimmed string) bool {
	return orderedItemRe.MatchString(trimmed)
}

// parseBlocks groups body's lines into headings, blank-line-separated
// paragraphs (consecutive non-blank, non-list, non-heading lines join
// into one paragraph), and consecutive-line unordered/ordered lists.
func parseBlocks(body string) []mdBlock {
	lines := strings.Split(body, "\n")
	var blocks []mdBlock
	var para []string

	flushPara := func() {
		if len(para) > 0 {
			blocks = append(blocks, mdBlock{kind: blockParagraph, lines: []string{strings.Join(para, " ")}})
			para = nil
		}
	}

	i := 0
	for i < len(lines) {
		line := strings.TrimRight(lines[i], "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flushPara()
			i++
		case strings.HasPrefix(trimmed, "### "):
			flushPara()
			blocks = append(blocks, mdBlock{kind: blockH3, lines: []string{strings.TrimSpace(trimmed[4:])}})
			i++
		case strings.HasPrefix(trimmed, "## "):
			flushPara()
			blocks = append(blocks, mdBlock{kind: blockH2, lines: []string{strings.TrimSpace(trimmed[3:])}})
			i++
		case strings.HasPrefix(trimmed, "# "):
			flushPara()
			blocks = append(blocks, mdBlock{kind: blockH1, lines: []string{strings.TrimSpace(trimmed[2:])}})
			i++
		case isUnorderedItem(trimmed):
			flushPara()
			var items []string
			for i < len(lines) {
				t := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
				if !isUnorderedItem(t) {
					break
				}
				items = append(items, strings.TrimSpace(t[2:]))
				i++
			}
			blocks = append(blocks, mdBlock{kind: blockUnordered, lines: items})
		case isOrderedItem(trimmed):
			flushPara()
			var items []string
			for i < len(lines) {
				t := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
				if !isOrderedItem(t) {
					break
				}
				loc := orderedItemRe.FindStringIndex(t)
				items = append(items, strings.TrimSpace(t[loc[1]:]))
				i++
			}
			blocks = append(blocks, mdBlock{kind: blockOrdered, lines: items})
		default:
			para = append(para, trimmed)
			i++
		}
	}
	flushPara()
	return blocks
}

// renderInline handles the character-level constructs (**bold**,
// *italic*/_italic_, `code`, [text](url)) within one block's text,
// returning the HTML fragment and the plain-word fragment in one pass.
// Every literal character not consumed by a recognized construct is
// escaped via html/template.HTMLEscapeString before being written to
// the HTML output — this is what keeps a literal `<script>` (or any
// other unrecognized markup) inert in the rendered page.
func renderInline(s string) (htmlOut, plainOut string) {
	var h, p strings.Builder
	var literal strings.Builder
	flushLiteral := func() {
		if literal.Len() == 0 {
			return
		}
		h.WriteString(template.HTMLEscapeString(literal.String()))
		p.WriteString(literal.String())
		literal.Reset()
	}

	i := 0
	n := len(s)
	for i < n {
		matched := false
		switch {
		case strings.HasPrefix(s[i:], "**"):
			if end := strings.Index(s[i+2:], "**"); end >= 0 {
				flushLiteral()
				inner := s[i+2 : i+2+end]
				h.WriteString("<strong>" + template.HTMLEscapeString(inner) + "</strong>")
				p.WriteString(inner)
				i += 2 + end + 2
				matched = true
			}
		case s[i] == '*' || s[i] == '_':
			marker := s[i]
			if end := strings.IndexByte(s[i+1:], marker); end >= 0 {
				closeIdx := i + 1 + end
				inner := s[i+1 : closeIdx]
				// Flanking guard (independent review, uc-infra#144):
				// without this, any two "_" or "*" bytes anywhere later
				// in the same block paired up unconditionally — and
				// this kernel's own prose is full of snake_case
				// identifiers (tenant_id, actor_type, entity_type,
				// input_hash, ...), so two unrelated identifiers in one
				// paragraph silently became a bogus <em> span, AND the
				// PlainText search index lost the underscores entirely
				// (a topic that visibly says "tenant_id" became
				// unsearchable for the literal string "tenant_id").
				// CommonMark's own rule is exactly this: "_" never
				// starts emphasis intraword (unlike "*", which is
				// allowed inside a word — "snake_case*emphasis*here" is
				// fine) — reject when both the byte immediately before
				// the opening marker and the byte immediately after the
				// closing marker are word characters. "*" gets a
				// narrower, single-purpose guard instead: reject an
				// empty inner span or one that starts/ends with
				// whitespace, so "5 * 3 grid ... 2 * 2 grid" (a literal
				// multiplication a numeric-bounds help topic will
				// contain) doesn't get read as "* 3 grid ... 2 *".
				ok := len(inner) > 0
				if marker == '_' {
					ok = ok && !(isWordByte(s, i-1) && isWordByte(s, closeIdx+1))
				} else {
					first, _ := utf8.DecodeRuneInString(inner)
					last, _ := utf8.DecodeLastRuneInString(inner)
					ok = ok && !unicode.IsSpace(first) && !unicode.IsSpace(last)
				}
				if ok {
					flushLiteral()
					h.WriteString("<em>" + template.HTMLEscapeString(inner) + "</em>")
					p.WriteString(inner)
					i = closeIdx + 1
					matched = true
				}
			}
		case s[i] == '`':
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				flushLiteral()
				inner := s[i+1 : i+1+end]
				h.WriteString("<code>" + template.HTMLEscapeString(inner) + "</code>")
				p.WriteString(inner)
				i += 1 + end + 1
				matched = true
			}
		case s[i] == '[':
			if textEnd := strings.IndexByte(s[i+1:], ']'); textEnd >= 0 {
				textEndAbs := i + 1 + textEnd
				if textEndAbs+1 < n && s[textEndAbs+1] == '(' {
					// Depth-counted scan for the matching close paren,
					// not a first-IndexByte(')'): a URL can itself
					// contain balanced parens (javascript:alert(1) is
					// exactly the kind of malicious URL this construct
					// must defang, and it contains one pair itself) —
					// a naive first-')' match would truncate the URL
					// mid-way and leak the rest as literal trailing text.
					depth := 1
					j := textEndAbs + 2
					closed := false
					for ; j < n; j++ {
						switch s[j] {
						case '(':
							depth++
						case ')':
							depth--
							if depth == 0 {
								closed = true
							}
						}
						if closed {
							break
						}
					}
					if closed {
						flushLiteral()
						linkText := s[i+1 : textEndAbs]
						url := s[textEndAbs+2 : j]
						if isSafeHelpLinkURL(url) {
							h.WriteString(`<a href="` + template.HTMLEscapeString(url) + `">` + template.HTMLEscapeString(linkText) + `</a>`)
						} else {
							// Reject rather than emit a live href for any
							// scheme other than http(s):// or a leading
							// "/" — e.g. a `javascript:` URL never
							// becomes clickable. The link's text still
							// shows (escaped, no anchor) rather than
							// vanishing outright.
							h.WriteString(template.HTMLEscapeString(linkText))
						}
						p.WriteString(linkText)
						i = j + 1
						matched = true
					}
				}
			}
		}
		if matched {
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		literal.WriteRune(r)
		i += size
	}
	flushLiteral()
	return h.String(), p.String()
}

// isSafeHelpLinkURL allows exactly http://, https://, or a same-origin
// leading "/" — every other scheme (javascript:, data:, mailto: is even
// disallowed here even though it's benign, since this narrow allowlist
// is simpler and more defensible than an ever-growing denylist) is
// rejected defensively, per ADR-0023/uc-infra#144's scope. A leading
// "//" (a protocol-relative URL, e.g. "//evil.example/x") is explicitly
// excluded even though it also starts with "/": the browser resolves it
// against whatever scheme the current page loaded over, silently
// navigating off-origin — exactly the kind of surprise "reject/escape
// anything else defensively" exists to prevent, not a bare same-origin
// path. A leading "/\" is rejected the same way (independent review,
// uc-infra#144): per the WHATWG URL standard, the relative-slash state
// for a special scheme (http/https, which every real page here loads
// over) treats "\" exactly like "/", so "/\evil.example/x" resolves
// off-origin in every current browser identically to "//evil.example/x"
// — a bare single-backslash check, not a full net/url parse, because
// this allowlist only ever needs to tell "same-origin path" apart from
// "looks like one but isn't", not parse a URL for any other purpose.
// isWordByte reports whether s[i] is a "word" byte (ASCII letter,
// digit, underscore, or any byte belonging to a multibyte UTF-8
// sequence, treated conservatively as word-like so this never
// misjudges a rune boundary) for renderInline's intraword-"_" flanking
// guard above — false for an out-of-range i (start/end of string is
// never "inside a word").
func isWordByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	b := s[i]
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

func isSafeHelpLinkURL(url string) bool {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return true
	}
	if !strings.HasPrefix(url, "/") {
		return false
	}
	return len(url) < 2 || (url[1] != '/' && url[1] != '\\')
}
