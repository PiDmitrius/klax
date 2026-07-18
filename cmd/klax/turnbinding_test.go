package main

import (
	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/promptcanon"
	"github.com/PiDmitrius/klax/internal/sessfiles"
	"testing"
)

func TestProposeBindingsOrderedAndBounded(t *testing.T) {
	d := promptcanon.Digest("same")
	turns := []sessfiles.Turn{
		{Seq: 1, Backend: "claude", Session: "S", PromptDigest: d, FromEvent: 2},
		{Seq: 2, Backend: "claude", Session: "S", PromptDigest: d, FromEvent: 6},
	}
	items := []history.Item{
		{Role: "user", Event: 3, PromptDigest: promptcanon.Digest("manual"), RecordDigest: "m"},
		{Role: "user", Event: 4, PromptDigest: d, RecordDigest: "a"},
		{Role: "user", Event: 7, PromptDigest: d, RecordDigest: "b"},
	}
	got := proposeBindings(turns, items, "claude", "S", 9)
	if len(got) != 2 || got[0].Seq != 1 || got[0].Event != 4 || got[1].Seq != 2 || got[1].Event != 7 {
		t.Fatalf("proposals: %+v", got)
	}
}

func TestProposeBindingsDoesNotCrossNextRun(t *testing.T) {
	d := promptcanon.Digest("same")
	turns := []sessfiles.Turn{{Seq: 1, Backend: "codex", Session: "S", PromptDigest: d, FromEvent: 1}, {Seq: 2, Backend: "codex", Session: "S", PromptDigest: d, FromEvent: 3}}
	items := []history.Item{{Role: "user", Event: 4, PromptDigest: d, RecordDigest: "late"}}
	got := proposeBindings(turns, items, "codex", "S", 5)
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("older run crossed interval: %+v", got)
	}
}
