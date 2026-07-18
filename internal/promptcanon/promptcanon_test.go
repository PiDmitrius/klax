package promptcanon

import "testing"

func TestCanonicalNewlinesOnly(t *testing.T) {
	got := Canonical(" a\r\nb\rc \n")
	if got != " a\nb\nc \n" {
		t.Fatalf("Canonical = %q", got)
	}
	if Digest("x\r\ny") != Digest("x\ny") {
		t.Fatal("equivalent newline forms differ")
	}
	if Digest("x ") == Digest("x") {
		t.Fatal("meaningful whitespace was trimmed")
	}
}

func TestDigestPreservesPromptContent(t *testing.T) {
	cases := []string{
		"one line", "multi\nline\n", "  surrounding whitespace  ", "Юникод 🏓",
		"Пользователь отправил файлы. Прочитай и проанализируй их:\n/tmp/klax-attach-123/report.pdf",
		"caption\n\nПрикреплённые файлы:\n/tmp/klax-attach-456/a.txt\n/tmp/klax-attach-456/b.txt",
	}
	for _, prompt := range cases {
		if Digest(prompt) != Digest(Canonical(prompt)) {
			t.Fatalf("digest not canonical for %q", prompt)
		}
	}
	if Digest("x\n") == Digest("x") {
		t.Fatal("trailing newline must remain meaningful")
	}
}
