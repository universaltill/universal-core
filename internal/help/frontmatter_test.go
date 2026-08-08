package help

import (
	"strings"
	"testing"
)

func TestParseTopicFile_Valid(t *testing.T) {
	raw := []byte("---\ntitle: Purchase Orders\naudience: user\nmodule: purchasing\norder: 10\n---\n# Purchase Orders\n\nBody text.\n")
	fm, body, err := parseTopicFile(raw)
	if err != nil {
		t.Fatalf("parseTopicFile: %v", err)
	}
	want := frontMatter{Title: "Purchase Orders", Audience: "user", Module: "purchasing", Order: 10}
	if fm != want {
		t.Errorf("frontMatter = %+v, want %+v", fm, want)
	}
	if !strings.Contains(body, "# Purchase Orders") {
		t.Errorf("body = %q, expected the markdown heading to survive", body)
	}
}

func TestParseTopicFile_QuotedValues(t *testing.T) {
	raw := []byte(`---
title: "Purchase Orders: A Guide"
audience: 'admin'
module: "purchasing"
order: 3
---
Body.
`)
	fm, _, err := parseTopicFile(raw)
	if err != nil {
		t.Fatalf("parseTopicFile: %v", err)
	}
	if fm.Title != "Purchase Orders: A Guide" {
		t.Errorf("Title = %q, want the quoted value with its inner colon preserved", fm.Title)
	}
	if fm.Audience != "admin" {
		t.Errorf("Audience = %q, want %q", fm.Audience, "admin")
	}
	if fm.Module != "purchasing" {
		t.Errorf("Module = %q, want %q", fm.Module, "purchasing")
	}
}

func TestParseTopicFile_UnquotedVerbatim(t *testing.T) {
	fm, _, err := parseTopicFile([]byte("---\ntitle:   Spaced Out Title   \naudience: both\nmodule: foundation\norder: 0\n---\nx\n"))
	if err != nil {
		t.Fatalf("parseTopicFile: %v", err)
	}
	if fm.Title != "Spaced Out Title" {
		t.Errorf("Title = %q, want surrounding whitespace trimmed", fm.Title)
	}
}

func TestParseTopicFile_MissingOpeningDelimiter(t *testing.T) {
	_, _, err := parseTopicFile([]byte("title: X\naudience: user\nmodule: m\norder: 1\n---\nbody\n"))
	if err == nil {
		t.Fatal("expected an error for a file with no opening --- delimiter")
	}
}

func TestParseTopicFile_MissingClosingDelimiter(t *testing.T) {
	_, _, err := parseTopicFile([]byte("---\ntitle: X\naudience: user\nmodule: m\norder: 1\nbody with no closing delimiter\n"))
	if err == nil {
		t.Fatal("expected an error for a file with no closing --- delimiter")
	}
}

func TestParseTopicFile_EmptyFile(t *testing.T) {
	_, _, err := parseTopicFile([]byte(""))
	if err == nil {
		t.Fatal("expected an error for a completely empty file")
	}
}

func TestParseTopicFile_MissingRequiredKey(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"missing title", "---\naudience: user\nmodule: m\norder: 1\n---\nbody\n"},
		{"missing audience", "---\ntitle: T\nmodule: m\norder: 1\n---\nbody\n"},
		{"missing module", "---\ntitle: T\naudience: user\norder: 1\n---\nbody\n"},
		{"missing order", "---\ntitle: T\naudience: user\nmodule: m\n---\nbody\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := parseTopicFile([]byte(c.raw))
			if err == nil {
				t.Fatalf("expected a parse error for %s", c.name)
			}
		})
	}
}

func TestParseTopicFile_EmptyTitleOrModule(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty title", "---\ntitle:\naudience: user\nmodule: m\norder: 1\n---\nbody\n"},
		{"empty module", "---\ntitle: T\naudience: user\nmodule:\norder: 1\n---\nbody\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := parseTopicFile([]byte(c.raw))
			if err == nil {
				t.Fatalf("expected a parse error for %s", c.name)
			}
		})
	}
}

func TestParseTopicFile_InvalidAudience(t *testing.T) {
	_, _, err := parseTopicFile([]byte("---\ntitle: T\naudience: superuser\nmodule: m\norder: 1\n---\nbody\n"))
	if err == nil {
		t.Fatal("expected a parse error for an audience value outside user|admin|both")
	}
}

func TestParseTopicFile_UnparseableOrder(t *testing.T) {
	_, _, err := parseTopicFile([]byte("---\ntitle: T\naudience: user\nmodule: m\norder: not-a-number\n---\nbody\n"))
	if err == nil {
		t.Fatal("expected a parse error for an unparseable order")
	}
}

func TestParseTopicFile_NegativeAndZeroOrderAreValid(t *testing.T) {
	for _, order := range []string{"0", "-1", "100"} {
		fm, _, err := parseTopicFile([]byte("---\ntitle: T\naudience: user\nmodule: m\norder: " + order + "\n---\nbody\n"))
		if err != nil {
			t.Fatalf("order=%s: unexpected error: %v", order, err)
		}
		_ = fm
	}
}

func TestParseTopicFile_BodyStripsOneLeadingBlankLine(t *testing.T) {
	_, body, err := parseTopicFile([]byte("---\ntitle: T\naudience: user\nmodule: m\norder: 1\n---\n\nActual body starts here.\n"))
	if err != nil {
		t.Fatalf("parseTopicFile: %v", err)
	}
	if strings.HasPrefix(body, "\n") {
		t.Errorf("body = %q, expected the single leading blank line to be stripped", body)
	}
	if !strings.Contains(body, "Actual body starts here.") {
		t.Errorf("body = %q, missing expected content", body)
	}
}

func TestUnquote(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"quoted"`, "quoted"},
		{`'quoted'`, "quoted"},
		{"unquoted", "unquoted"},
		{`"mismatched'`, `"mismatched'`},
		{`"`, `"`},
		{"", ""},
	}
	for _, c := range cases {
		if got := unquote(c.in); got != c.want {
			t.Errorf("unquote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
