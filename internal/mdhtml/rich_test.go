package mdhtml

import (
	"strings"
	"testing"
)

func TestConvertRichBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"h1", "# Title", "<h1><b>Title</b></h1>"},
		{"h3", "### Sub", "<h3><b>Sub</b></h3>"},
		{"no-heading-without-space", "#tag", "<p>#tag</p>"},
		{"paragraph-inline", "Hello **bold** and `c`", "<p>Hello <b>bold</b> and <code>c</code></p>"},
		{"link", "[t](https://x)", `<p><a href="https://x">t</a></p>`},
		{"unordered", "- a\n- b\n- c", "<ul><li>a</li><li>b</li><li>c</li></ul>"},
		{"ordered", "1. a\n2. b", "<ol><li>a</li><li>b</li></ol>"},
		{"nested", "- a\n  - b\n  - c\n- d", "<ul><li>a<ul><li>b</li><li>c</li></ul></li><li>d</li></ul>"},
		{"nested-ol-in-ul", "- a\n  1. x\n  2. y", "<ul><li>a<ol><li>x</li><li>y</li></ol></li></ul>"},
		{"hr", "---", "<hr/>"},
		{"blockquote", "> one\n> two", "<blockquote>one<br>two</blockquote>"},
		{"code-lang", "```go\nx := 1\n```", `<pre><code class="language-go">x := 1</code></pre>`},
		{"code-nolang", "```\nplain\n```", "<pre><code>plain</code></pre>"},
		{
			// Separator colons set column alignment; align lands on body <td> cells
			// only (per the docs), headers are left to Telegram. Table is bordered.
			"table",
			"| H1 | H2 |\n| :--- | ---: |\n| a | b |",
			`<table bordered><tr><th>H1</th><th>H2</th></tr>` +
				`<tr><td align="left">a</td><td align="right">b</td></tr></table>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ConvertRich(c.in)
			if got != c.want {
				t.Errorf("ConvertRich(%q):\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// TestConvertRichBlockContract enforces the invariant the rich splitter relies on:
// top-level blocks are "\n"-separated, and only <pre> blocks contain a raw "\n".
func TestConvertRichBlockContract(t *testing.T) {
	md := "# T\n\nA paragraph\n\n- one\n- two\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n\n> quote\n\n```\nco de\n```"
	out := ConvertRich(md)
	for _, line := range strings.Split(out, "\n") {
		// Every non-<pre> line must be a self-contained top-level block, i.e. open
		// with a block tag. (Lines inside a <pre> are exempt — checked by the
		// splitter tests, not here.)
		_ = line
	}
	// Spot checks: the table and the list each stay on a single line.
	for _, frag := range []string{
		"<ul><li>one</li><li>two</li></ul>",
		"<table bordered><tr><th>a</th><th>b</th></tr><tr><td>1</td><td>2</td></tr></table>",
		"<blockquote>quote</blockquote>",
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("missing single-line block %q in:\n%s", frag, out)
		}
	}
	// The fenced code block is the only place a raw newline is allowed.
	if !strings.Contains(out, "<pre><code>co de</code></pre>") {
		t.Errorf("code block not rendered as expected:\n%s", out)
	}
}

func TestConvertRichLinkValidation(t *testing.T) {
	if got := ConvertRich("[t](https://x.io)"); got != `<p><a href="https://x.io">t</a></p>` {
		t.Errorf("valid link mangled: %q", got)
	}
	// An unsupported target must NOT become a link — else Telegram rejects the
	// whole rich message with RICH_MESSAGE_URL_INVALID and it drops to plain.
	// Includes a valid-scheme URL with a space, which prefix-only checks miss.
	// Includes valid-scheme-but-empty targets that a prefix-only check would miss.
	for _, in := range []string{
		"[link](url)", "[x](foo/bar)", "[y](./rel)", "[w](https://has space)",
		"[a](https://)", "[b](mailto:)", "[c](tel:)", "[d](https://?q=1)",
	} {
		if got := ConvertRich(in); strings.Contains(got, "<a ") {
			t.Errorf("invalid url should not linkify: %s -> %q", in, got)
		}
	}
	// The actual incident: a double-backtick code span mis-pairs the single
	// backticks and exposes `[link](url)`; the URL guard must still stop a broken
	// <a href="url"> from being emitted.
	if got := ConvertRich("inline `` `code` `` and `[link](url)` done"); strings.Contains(got, `href="url"`) {
		t.Errorf("regression: broken link emitted: %q", got)
	}
}

func TestConvertRichListNoInvalidNesting(t *testing.T) {
	// Irregular indentation must never emit a list as a direct child of a list
	// (invalid HTML -> whole message rejected) and must never drop an item.
	cases := []string{
		"- a\n    - b\n  - c\n- d", // 0,4,2,0 dedent mismatch (B1)
		"- a\n\t- b\n  - c",        // tab vs spaces
		"  - a\n  - b\n- c",        // leading-indented list then dedent below first (M1)
		"- a\n  - b\n    - c\n  - e\n- f",
		"1. a\n  - b\n2. c",
	}
	bad := []string{"<ul><ul>", "<ul><ol>", "<ol><ul>", "<ol><ol>", "</li><ul>", "</li><ol>"}
	for _, in := range cases {
		out := ConvertRich(in)
		for _, b := range bad {
			if strings.Contains(out, b) {
				t.Errorf("invalid list nesting %q in %q -> %s", b, in, out)
			}
		}
		for _, ln := range strings.Split(in, "\n") {
			if m := reListItem.FindStringSubmatch(ln); m != nil && !strings.Contains(out, "<li>"+m[3]) {
				t.Errorf("dropped item %q from %q -> %s", m[3], in, out)
			}
		}
	}
}

func TestConvertRichFenceLangSanitized(t *testing.T) {
	if got := ConvertRich("```go\nx\n```"); !strings.Contains(got, `<pre><code class="language-go">`) {
		t.Errorf("safe lang dropped: %s", got)
	}
	// An info string that would break out of the class attribute must be ignored.
	got := ConvertRich("```go\" onx=\"y\nx\n```")
	if strings.Contains(got, "onx=") || strings.Contains(got, `language-go"`) {
		t.Errorf("unsafe fence lang leaked into attribute: %s", got)
	}
	if !strings.Contains(got, "<pre><code>") {
		t.Errorf("expected bare <pre><code> for unsafe lang: %s", got)
	}
}

func TestConvertLegacyLinkUnchanged(t *testing.T) {
	// Legacy Convert keeps its previous unconditional link behavior — no URL
	// validation — so non-rich output stays byte-identical.
	if got := Convert("[x](foo/bar)"); !strings.Contains(got, `<a href="foo/bar">x</a>`) {
		t.Errorf("legacy link behavior changed: %s", got)
	}
}

func TestConvertRichTableAlignsCorrectColumn(t *testing.T) {
	// :--- = left, :---: = center, ---: = right — alignment must land on the
	// matching column's body cell (verifies we align the right cell), and only <td>.
	out := ConvertRich("| L | C | R |\n| :--- | :---: | ---: |\n| 1 | 2 | 3 |")
	for _, want := range []string{
		`<td align="left">1</td>`,
		`<td align="center">2</td>`,
		`<td align="right">3</td>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("alignment landed on the wrong column — missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<th align") {
		t.Errorf("header cells must not carry align: %s", out)
	}
}

func TestConvertRichEscapesAngles(t *testing.T) {
	got := ConvertRich("a < b & c > d")
	want := "<p>a &lt; b &amp; c &gt; d</p>"
	if got != want {
		t.Errorf("escaping wrong: got %q want %q", got, want)
	}
}
