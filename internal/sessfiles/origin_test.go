package sessfiles

import (
	"testing"

	"github.com/PiDmitrius/klax/internal/inbound"
)

func TestEnqueueOriginSurvivesReplay(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:test", 1)
	origin := inbound.Origin{
		Transport: "ym",
		Chat:      inbound.Chat{ID: "0/0/group", Type: "group", ThreadID: "7"},
		Message:   inbound.Message{ID: "99", SentAt: "2026-07-25T09:34:56Z"},
		Sender:    inbound.Sender{ID: "u1", Username: "ivan@example.org", DisplayName: "Иван"},
	}
	seq, _, _, _, acceptedAt, err := s.EnqueueOrigin(
		"ym:0/0/group#7", "99", "", "effective", "@bot effective", nil, origin,
	)
	if err != nil {
		t.Fatal(err)
	}
	if acceptedAt == 0 {
		t.Fatal("acceptedAt is zero")
	}
	turns, err := s.InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("turn count = %d, want 1", len(turns))
	}
	got := turns[0]
	if got.Seq != seq || got.OriginalText != "@bot effective" || got.Origin != origin {
		t.Fatalf("replayed turn = %+v", got)
	}
}
