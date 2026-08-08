package help

import (
	"strings"
	"testing"
)

func TestRenderMarkdown_Headings(t *testing.T) {
	html, plain := RenderMarkdown("# H1\n\n## H2\n\n### H3\n")
	s := string(html)
	if !strings.Contains(s, "<h1>H1</h1>") {
		t.Errorf("html = %q, expected <h1>H1</h1>", s)
	}
	if !strings.Contains(s, "<h2>H2</h2>") {
		t.Errorf("html = %q, expected <h2>H2</h2>", s)
	}
	if !strings.Contains(s, "<h3>H3</h3>") {
		t.Errorf("html = %q, expected <h3>H3</h3>", s)
	}
	if !strings.Contains(plain, "H1") || !strings.Contains(plain, "H2") || !strings.Contains(plain, "H3") {
		t.Errorf("plain = %q, expected bare heading words, markers stripped", plain)
	}
	if strings.Contains(plain, "#") {
		t.Errorf("plain = %q, expected no leftover # markers", plain)
	}
}

func TestRenderMarkdown_Paragraphs_BlankLineSeparated(t *testing.T) {
	html, _ := RenderMarkdown("First paragraph,\nstill first.\n\nSecond paragraph.\n")
	s := string(html)
	if strings.Count(s, "<p>") != 2 {
		t.Errorf("html = %q, expected exactly 2 paragraphs", s)
	}
	if !strings.Contains(s, "First paragraph, still first.") {
		t.Errorf("html = %q, expected consecutive lines joined into one paragraph", s)
	}
}

func TestRenderMarkdown_Bold(t *testing.T) {
	html, plain := RenderMarkdown("This is **bold** text.")
	if !strings.Contains(string(html), "<strong>bold</strong>") {
		t.Errorf("html = %q, expected <strong>bold</strong>", html)
	}
	if !strings.Contains(plain, "bold") || strings.Contains(plain, "**") {
		t.Errorf("plain = %q, expected bare word bold with markers stripped", plain)
	}
}

func TestRenderMarkdown_Italic_StarAndUnderscore(t *testing.T) {
	html, _ := RenderMarkdown("This is *italic* and this is _also italic_.")
	s := string(html)
	if !strings.Contains(s, "<em>italic</em>") {
		t.Errorf("html = %q, expected <em>italic</em> from *italic*", s)
	}
	if !strings.Contains(s, "<em>also italic</em>") {
		t.Errorf("html = %q, expected <em>also italic</em> from _also italic_", s)
	}
}

func TestRenderMarkdown_InlineCode(t *testing.T) {
	html, plain := RenderMarkdown("Run `go test` now.")
	if !strings.Contains(string(html), "<code>go test</code>") {
		t.Errorf("html = %q, expected <code>go test</code>", html)
	}
	if !strings.Contains(plain, "go test") {
		t.Errorf("plain = %q, expected bare text go test", plain)
	}
}

func TestRenderMarkdown_Link_SafeSchemes(t *testing.T) {
	cases := []struct {
		name, md, wantHref string
	}{
		{"https", "[docs](https://example.com/docs)", "https://example.com/docs"},
		{"http", "[docs](http://example.com/docs)", "http://example.com/docs"},
		{"root-relative", "[docs](/help/entity/Item)", "/help/entity/Item"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			html, plain := RenderMarkdown(c.md)
			s := string(html)
			if !strings.Contains(s, `<a href="`+c.wantHref+`">docs</a>`) {
				t.Errorf("html = %q, expected a live link to %q", s, c.wantHref)
			}
			if plain != "docs" {
				t.Errorf("plain = %q, want %q ([text](url) -> text)", plain, "docs")
			}
		})
	}
}

func TestRenderMarkdown_Link_JavascriptSchemeNeverBecomesLiveHref(t *testing.T) {
	html, plain := RenderMarkdown("[Click me](javascript:alert(1))")
	s := string(html)
	if strings.Contains(s, "<a ") || strings.Contains(s, "href=") {
		t.Errorf("html = %q, a javascript: URL must never become a live href", s)
	}
	if !strings.Contains(s, "Click me") {
		t.Errorf("html = %q, expected the link text to still be visible", s)
	}
	if plain != "Click me" {
		t.Errorf("plain = %q, want %q", plain, "Click me")
	}
}

func TestRenderMarkdown_Link_DataAndMailtoSchemesRejected(t *testing.T) {
	for _, url := range []string{"data:text/html,<script>", "mailto:x@example.com", "ftp://example.com/x"} {
		html, _ := RenderMarkdown("[text](" + url + ")")
		if strings.Contains(string(html), "href=") {
			t.Errorf("url %q: html = %q, expected no live href for a disallowed scheme", url, html)
		}
	}
}

func TestRenderMarkdown_Image_Basic(t *testing.T) {
	html, _ := RenderMarkdown("![A warehouse shelf](/help/assets/list/en.jpg)")
	s := string(html)
	want := `<img src="/help/assets/list/en.jpg" alt="A warehouse shelf" loading="lazy" class="uc-help-image">`
	if !strings.Contains(s, want) {
		t.Errorf("html = %q, expected to contain %q", s, want)
	}
}

func TestRenderMarkdown_Image_AltTextInPlainText(t *testing.T) {
	_, plain := RenderMarkdown("![A warehouse shelf](/help/assets/list/en.jpg)")
	if !strings.Contains(plain, "A warehouse shelf") {
		t.Errorf("plain = %q, expected the alt text present so the image is searchable", plain)
	}
	if strings.Contains(plain, "![") || strings.Contains(plain, "(/help") {
		t.Errorf("plain = %q, expected no leftover markdown image syntax", plain)
	}
}

func TestRenderMarkdown_Image_UnsafeSrcRejected(t *testing.T) {
	for _, url := range []string{"javascript:alert(1)", "//evil.example/x", "data:text/html,x"} {
		t.Run(url, func(t *testing.T) {
			html, plain := RenderMarkdown("![alt text](" + url + ")")
			s := string(html)
			if strings.Contains(s, "<img") {
				t.Errorf("html = %q, an unsafe src must never become a live <img>", s)
			}
			if !strings.Contains(s, "alt text") {
				t.Errorf("html = %q, expected the alt text to still show even with a rejected src", s)
			}
			if plain != "alt text" {
				t.Errorf("plain = %q, want %q", plain, "alt text")
			}
		})
	}
}

// TestRenderMarkdown_Image_RootRelativeSrcAllowed proves the one src
// shape a real topic ever actually uses (a same-origin /help/assets/...
// screenshot, helpassets.go) renders as a live <img>.
func TestRenderMarkdown_Image_RootRelativeSrcAllowed(t *testing.T) {
	html, _ := RenderMarkdown("![diagram](/help/assets/form/en.jpg)")
	s := string(html)
	want := `src="/help/assets/form/en.jpg"`
	if !strings.Contains(s, want) {
		t.Errorf("html = %q, expected a live src of %q", s, want)
	}
}

// TestRenderMarkdown_Image_HTTPSchemesRejected is the complement of
// TestRenderMarkdown_Image_RootRelativeSrcAllowed (independent review,
// uc-infra#145): unlike a [text](url) LINK — a user-initiated
// navigation, safe to point at any http(s):// origin — an ![alt](url)
// IMAGE fires an unconditional request the instant the topic renders.
// isSafeHelpImageSrc (markdown.go) is deliberately narrower than
// isSafeHelpLinkURL for exactly this reason: same-origin "/" only, no
// http(s):// branch, so a topic can never make this self-hosted,
// air-gapped-capable product silently phone home to a third-party host
// just by rendering.
func TestRenderMarkdown_Image_HTTPSchemesRejected(t *testing.T) {
	for _, url := range []string{"https://example.com/diagram.png", "http://example.com/diagram.png"} {
		t.Run(url, func(t *testing.T) {
			html, plain := RenderMarkdown("![diagram](" + url + ")")
			s := string(html)
			if strings.Contains(s, "<img") {
				t.Errorf("html = %q, an off-origin http(s):// src must never become a live <img> (unlike a link href, an image src fires unconditionally on render)", s)
			}
			if !strings.Contains(s, "diagram") {
				t.Errorf("html = %q, expected the alt text to still show even with a rejected src", s)
			}
			if plain != "diagram" {
				t.Errorf("plain = %q, want %q", plain, "diagram")
			}
		})
	}
}

// TestRenderMarkdown_ImageNotMisparsedAsLiteralBangThenLink is the exact
// regression case CLAUDE.md's test-first rule exists for: without the
// "!" being consumed as part of the image marker, `![alt](url)` would
// fall through to the "[" case and render as a literal "!" followed by a
// live <a> link (linkText="alt"), never an <img> at all.
func TestRenderMarkdown_ImageNotMisparsedAsLiteralBangThenLink(t *testing.T) {
	html, _ := RenderMarkdown("![alt](/help/assets/list/en.jpg)")
	s := string(html)
	if strings.Contains(s, "<a ") {
		t.Errorf("html = %q, expected no <a> link — this must parse as an image, not a literal \"!\" plus a [text](url) link", s)
	}
	if !strings.HasPrefix(s, "<p><img") {
		t.Errorf("html = %q, expected the image to start the paragraph with no leading literal \"!\"", s)
	}
}

func TestRenderMarkdown_Image_EscapesAltAndSrc(t *testing.T) {
	html, _ := RenderMarkdown(`![<b>alt</b>](/help/assets/list/en.jpg)`)
	s := string(html)
	if strings.Contains(s, "<b>alt</b>") {
		t.Errorf("html = %q, expected the alt text HTML-escaped, not interpreted", s)
	}
	if !strings.Contains(s, "&lt;b&gt;alt&lt;/b&gt;") {
		t.Errorf("html = %q, expected the escaped alt text present", s)
	}
}

func TestRenderMarkdown_UnorderedList(t *testing.T) {
	html, plain := RenderMarkdown("- one\n- two\n* three\n")
	s := string(html)
	if !strings.Contains(s, "<ul>") || !strings.Contains(s, "</ul>") {
		t.Errorf("html = %q, expected a single <ul>...</ul>", s)
	}
	if strings.Count(s, "<li>") != 3 {
		t.Errorf("html = %q, expected 3 <li> items (both - and * markers)", s)
	}
	if !strings.Contains(plain, "one") || !strings.Contains(plain, "two") || !strings.Contains(plain, "three") {
		t.Errorf("plain = %q, missing list item words", plain)
	}
}

func TestRenderMarkdown_OrderedList(t *testing.T) {
	html, _ := RenderMarkdown("1. first\n2. second\n3. third\n")
	s := string(html)
	if !strings.Contains(s, "<ol>") || !strings.Contains(s, "</ol>") {
		t.Errorf("html = %q, expected a single <ol>...</ol>", s)
	}
	if strings.Count(s, "<li>") != 3 {
		t.Errorf("html = %q, expected 3 <li> items", s)
	}
}

func TestRenderMarkdown_ConsecutiveListLinesBecomeOneList(t *testing.T) {
	html, _ := RenderMarkdown("Intro paragraph.\n\n- a\n- b\n- c\n\nOutro paragraph.\n")
	s := string(html)
	if strings.Count(s, "<ul>") != 1 {
		t.Errorf("html = %q, expected exactly one <ul> grouping all three items", s)
	}
	if strings.Count(s, "<p>") != 2 {
		t.Errorf("html = %q, expected exactly 2 paragraphs surrounding the list", s)
	}
}

func TestRenderMarkdown_UnrecognizedInputEscapedAsParagraph(t *testing.T) {
	html, plain := RenderMarkdown("<script>alert('xss')</script>")
	s := string(html)
	if strings.Contains(s, "<script>") {
		t.Fatalf("html = %q, a literal <script> tag in body text must render escaped, not live", s)
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Errorf("html = %q, expected the script tag HTML-escaped", s)
	}
	if !strings.Contains(plain, "<script>") {
		t.Errorf("plain = %q, expected the plain-text form to keep the literal characters (no escaping needed off-HTML)", plain)
	}
}

func TestRenderMarkdown_UnclosedMarkersFallBackToLiteral(t *testing.T) {
	html, plain := RenderMarkdown("An unclosed *asterisk and an unclosed `backtick.")
	s := string(html)
	if strings.Contains(s, "<em>") || strings.Contains(s, "<code>") {
		t.Errorf("html = %q, an unclosed marker must not open a tag", s)
	}
	if !strings.Contains(s, "*") || !strings.Contains(s, "`") {
		t.Errorf("html = %q, expected the literal marker characters to survive", s)
	}
	if !strings.Contains(plain, "*") {
		t.Errorf("plain = %q, expected the literal asterisk to survive", plain)
	}
}

func TestRenderMarkdown_HTMLEscapesLiteralAngleBracketsInsideInlineConstructs(t *testing.T) {
	html, _ := RenderMarkdown("**<b>not real html</b>**")
	s := string(html)
	if strings.Contains(s, "<b>") {
		t.Errorf("html = %q, expected the inner <b> to be escaped, not interpreted", s)
	}
	if !strings.Contains(s, "<strong>&lt;b&gt;not real html&lt;/b&gt;</strong>") {
		t.Errorf("html = %q, expected escaped inner content wrapped in <strong>", s)
	}
}

func TestIsSafeHelpLinkURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://example.com", true},
		{"http://example.com", true},
		{"/help/entity/Item", true},
		{"javascript:alert(1)", false},
		{"data:text/html,x", false},
		{"//example.com", false},
		// A backslash right after the leading "/" is a protocol-relative
		// URL in disguise: WHATWG URL's relative-slash state treats "\"
		// exactly like "/" for http(s) URLs, so "/\evil.example/x"
		// resolves off-origin in every real browser the same way
		// "//evil.example/x" does (independent review, uc-infra#144) —
		// both must be rejected, not just the forward-slash form.
		{`/\evil.example/pwn`, false},
		{"", false},
	}
	for _, c := range cases {
		if got := isSafeHelpLinkURL(c.url); got != c.want {
			t.Errorf("isSafeHelpLinkURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// TestRenderMarkdown_IntrawordUnderscoreNotEmphasis (independent review,
// uc-infra#144) is the regression test for a real bug found post-Tester:
// this repo's own snake_case identifiers (tenant_id, actor_type, ...)
// appear constantly in prose about this kernel, and the naive "pair any
// two markers" renderInline had no left/right-flanking rule, so two
// unrelated snake_case words in one paragraph were silently paired into
// a bogus <em> span — and, worse, the PlainText search index dropped the
// underscores entirely, so searching the literal identifier a topic
// visibly contains returned zero hits. CommonMark's own rule (intraword
// "_" never starts emphasis, unlike "*") exists for exactly this reason.
func TestRenderMarkdown_IntrawordUnderscoreNotEmphasis(t *testing.T) {
	html, plain := RenderMarkdown("Every audit row carries tenant_id and entity_type columns.")
	s := string(html)
	if strings.Contains(s, "<em>") {
		t.Errorf("html = %q, expected no <em> — tenant_id/entity_type are snake_case identifiers, not emphasis markers", s)
	}
	if !strings.Contains(s, "tenant_id") || !strings.Contains(s, "entity_type") {
		t.Errorf("html = %q, expected the literal identifiers tenant_id and entity_type to survive intact", s)
	}
	if !strings.Contains(plain, "tenant_id") || !strings.Contains(plain, "entity_type") {
		t.Errorf("plain = %q, expected the identifiers intact in the search-indexed plain text too, underscores not silently dropped", plain)
	}
}

// TestRenderMarkdown_StarSurroundedBySpacesNotEmphasis covers the "*"
// half of the same class of bug: "5 * 3" (a literal multiplication, the
// kind of prose a numeric-bounds help topic will contain) must not
// become "5 <em> 3</em>" just because there happen to be two "*" on the
// line.
func TestRenderMarkdown_StarSurroundedBySpacesNotEmphasis(t *testing.T) {
	html, _ := RenderMarkdown("A 5 * 3 grid and a 2 * 2 grid.")
	s := string(html)
	if strings.Contains(s, "<em>") {
		t.Errorf("html = %q, expected no <em> — these are literal asterisks, not emphasis (each is followed by a space)", s)
	}
}
