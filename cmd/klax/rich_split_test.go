package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/mdhtml"
	"github.com/PiDmitrius/klax/internal/runner"
)

// In rich mode the breathing separators (legacy "\n\n") must become real spacer
// blocks, since rich ignores inter-block whitespace. Legacy must be untouched.
func TestFormatLogChunksRichSpacer(t *testing.T) {
	items := []runner.ProgressEvent{
		{Kind: runner.ProgressKindTool, Text: "tool one"},
		{Kind: runner.ProgressKindTool, Text: "tool two"},
		{Kind: runner.ProgressKindNarration, Text: "thought"},
	}
	rich := strings.Join(formatLogChunks(items, "<p>answer</p>", "rich", 100000), "\n")
	// One spacer before the narration, one before the answer tail.
	if got := strings.Count(rich, richSpacerBlock); got != 2 {
		t.Errorf("expected 2 rich spacer blocks, got %d:\n%s", got, rich)
	}
	// Two tools stay tight (no spacer between them).
	if strings.Contains(rich, "tool one</code></p>\n"+richSpacerBlock+"\n<p><code>tool two") {
		t.Errorf("tool→tool should be tight, not spaced:\n%s", rich)
	}
	if leg := strings.Join(formatLogChunks(items, "answer", "html", 100000), "\n"); strings.Contains(leg, richSpacerBlock) {
		t.Errorf("legacy log must not contain the rich spacer:\n%s", leg)
	}
}

// reconstructing the blocks from every chunk, in order, must equal the original
// block list — proving no block was split, dropped, or merged into another.
func assertBlocksPreserved(t *testing.T, text string, chunks []string, limit int) {
	t.Helper()
	var got []string
	for _, c := range chunks {
		got = append(got, topLevelRichBlocks(c)...)
		// A chunk may exceed the soft limit only if it is a single block.
		if len(c) > limit && len(topLevelRichBlocks(c)) != 1 {
			t.Errorf("chunk over limit (%d) holds multiple blocks: %q", limit, c)
		}
	}
	want := topLevelRichBlocks(text)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blocks not preserved:\n got %#v\nwant %#v", got, want)
	}
}

func TestSplitRichMessageKeepsBlocksWhole(t *testing.T) {
	blocks := []string{
		"<h1>Title</h1>",
		"<ul><li>one</li><li>two</li><li>three</li></ul>",
		"<table><tr><th>a</th><th>b</th></tr><tr><td>1</td><td>2</td></tr></table>",
		"<p>tail</p>",
	}
	text := strings.Join(blocks, "\n")
	const limit = 80 // every block fits — none is oversized, so all stay whole
	chunks := splitRichMessage(text, limit)
	assertBlocksPreserved(t, text, chunks, limit)
	// The table occupies its own chunk intact (it doesn't pack with its neighbors).
	tableWhole := false
	for _, c := range chunks {
		if c == blocks[2] {
			tableWhole = true
		}
	}
	if !tableWhole {
		t.Errorf("oversized table block was not emitted whole: %#v", chunks)
	}
}

func TestSplitRichMessagePreUnderLimitWhole(t *testing.T) {
	// A multi-line <pre> that fits under the limit must survive intact (its
	// internal newlines must not be treated as block boundaries).
	pre := "<pre><code>line1\nline2\nline3\nline4</code></pre>"
	text := "<p>head</p>\n" + pre + "\n<p>tail</p>"
	const limit = 50
	chunks := splitRichMessage(text, limit)
	assertBlocksPreserved(t, text, chunks, limit)
	found := false
	for _, c := range chunks {
		if strings.Contains(c, pre) {
			found = true
		}
	}
	if !found {
		t.Errorf("multi-line <pre> under the limit was split: %#v", chunks)
	}
}

func TestSplitRichMessagePreOversizedSplit(t *testing.T) {
	// An over-limit <pre> is split on its internal newlines into several valid
	// <pre> blocks — no line is lost and each piece is a well-formed code block.
	pre := "<pre><code>aaaa\nbbbb\ncccc\ndddd</code></pre>"
	const limit = 40
	chunks := splitRichMessage(pre, limit)
	if len(chunks) < 2 {
		t.Fatalf("oversized <pre> should split, got %#v", chunks)
	}
	all := strings.Join(chunks, "")
	for _, ln := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		if !strings.Contains(all, ln) {
			t.Errorf("line %q lost when splitting <pre>: %#v", ln, chunks)
		}
	}
	for _, c := range chunks {
		if !strings.HasPrefix(c, "<pre>") || !strings.HasSuffix(c, "</code></pre>") {
			t.Errorf("split <pre> piece is not a valid pre block: %q", c)
		}
	}
}

func TestSplitRichMessageOversizedNonPre(t *testing.T) {
	// A huge non-<pre> block (here a long list) must be split so no chunk — nor
	// the plain fallback — overruns the limit; splitHTMLMessage keeps ul/li
	// balanced across the cut and loses no items.
	var b strings.Builder
	b.WriteString("<ul>")
	for i := 0; i < 40; i++ {
		b.WriteString("<li>item</li>")
	}
	b.WriteString("</ul>")
	orig := b.String()
	const limit = 60
	chunks := splitRichMessage(orig, limit)
	if len(chunks) < 2 {
		t.Fatalf("oversized list should split, got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		// splitHTMLMessage re-opens tags across a cut, so each chunk's open/close
		// counts must still match (balanced), even though the total tag count grows.
		if strings.Count(c, "<ul>") != strings.Count(c, "</ul>") || strings.Count(c, "<li>") != strings.Count(c, "</li>") {
			t.Errorf("unbalanced ul/li in chunk: %q", c)
		}
	}
	// No content lost: stripping tags and concatenating reproduces the original text.
	if got := stripHTML(strings.Join(chunks, "")); got != stripHTML(orig) {
		t.Errorf("content changed after split:\n got %q\nwant %q", got, stripHTML(orig))
	}
}

func TestSplitRichMessageOversizedBlockquote(t *testing.T) {
	// Regression: an oversized <blockquote> contains <br>; the splitter must treat
	// <br> as void, so it terminates (no infinite loop), never emits invalid
	// </br>, keeps each chunk balanced, and loses no content.
	var b strings.Builder
	b.WriteString("<blockquote>")
	for i := 0; i < 60; i++ {
		if i > 0 {
			b.WriteString("<br>")
		}
		b.WriteString("quoted line of text")
	}
	b.WriteString("</blockquote>")
	orig := b.String()
	const limit = 120
	chunks := splitRichMessage(orig, limit) // must terminate
	if len(chunks) < 2 {
		t.Fatalf("oversized blockquote should split, got %d", len(chunks))
	}
	for _, c := range chunks {
		if strings.Contains(c, "</br>") {
			t.Errorf("invalid </br> emitted: %q", c)
		}
		if strings.Count(c, "<blockquote>") != strings.Count(c, "</blockquote>") {
			t.Errorf("unbalanced blockquote in chunk: %q", c)
		}
	}
	if got := stripHTML(strings.Join(chunks, "")); got != stripHTML(orig) {
		t.Errorf("content changed after split:\n got %q\nwant %q", got, stripHTML(orig))
	}
}

func TestSplitRichMessageShortStaysOne(t *testing.T) {
	text := mdhtml.ConvertRich("# Hi\n\nshort answer")
	chunks := splitRichMessage(text, maxMessageLen)
	if len(chunks) != 1 || chunks[0] != text {
		t.Errorf("short rich message should be one chunk, got %#v", chunks)
	}
}

func TestHTMLToPlain(t *testing.T) {
	cases := map[string]string{
		"<ul><li>a</li><li>b</li></ul>": "a\nb",
		"<p>one</p><p>two</p>":          "one\ntwo",
		"<b>x</b> &amp; <code>y</code>": "x & y",
	}
	for in, want := range cases {
		if got := htmlToPlain(in); got != want {
			t.Errorf("htmlToPlain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithProgressEllipsisRich(t *testing.T) {
	chunks := withProgressEllipsis([]string{"<p>x</p>"}, "rich", maxMessageLen)
	if len(chunks) != 1 || !strings.HasSuffix(chunks[0], "\n<p>…</p>") {
		t.Fatalf("rich ellipsis wrong: %#v", chunks)
	}
	if empty := withProgressEllipsis(nil, "rich", maxMessageLen); len(empty) != 1 || empty[0] != "<p>…</p>" {
		t.Fatalf("rich empty ellipsis wrong: %#v", empty)
	}
	// An over-soft-limit single block: the ellipsis must become its own chunk, not
	// be concatenated (which would push the block further over the limit).
	big := "<p>" + strings.Repeat("x", 40) + "</p>"
	if got := withProgressEllipsis([]string{big}, "rich", 20); len(got) != 2 || got[0] != big || got[1] != "<p>…</p>" {
		t.Fatalf("rich ellipsis over limit should be a separate chunk: %#v", got)
	}
}

// End-to-end: real ConvertRich output packs into block-aligned chunks.
func TestSplitRichMessageIntegration(t *testing.T) {
	md := "# Report\n\nIntro paragraph with **bold**.\n\n" +
		"| Metric | Value |\n| :--- | ---: |\n| Speed | 42 |\n| Status | ok |\n\n" +
		"- first item\n- second item\n- third item\n\n" +
		"```go\nfunc main() {}\n```\n\nClosing remarks."
	text := mdhtml.ConvertRich(md)
	const limit = 256 // above the largest block, so blocks pack without being split
	chunks := splitRichMessage(text, limit)
	assertBlocksPreserved(t, text, chunks, limit)
}
