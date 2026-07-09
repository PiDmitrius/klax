package sealref

import (
	"testing"
	"time"
)

func TestSealRoundTripTamperExpiry(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	p := Payload{SessionKey: "user:alice", Created: 42, Path: "/x/a.png", ContentType: "image/png", Exp: now.Add(time.Hour).Unix()}
	ref, err := s.Seal(p)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip.
	got, err := s.Open(ref, now)
	if err != nil || got != p {
		t.Fatalf("round-trip = %+v, %v; want %+v", got, err, p)
	}
	// No path is exposed in the ref text (it is sealed, not just signed).
	if len(ref) == 0 || containsSub(ref, "a.png") || containsSub(ref, "user:alice") {
		t.Fatalf("ref leaks plaintext: %q", ref)
	}
	// Tampered ref fails.
	if _, err := s.Open(ref+"AA", now); err == nil {
		t.Fatal("tampered ref must fail")
	}
	// A different key cannot open it.
	s2, _ := New()
	if _, err := s2.Open(ref, now); err == nil {
		t.Fatal("foreign key must fail")
	}
	// Expired ref fails.
	pe := p
	pe.Exp = now.Add(-time.Minute).Unix()
	refE, _ := s.Seal(pe)
	if _, err := s.Open(refE, now); err == nil {
		t.Fatal("expired ref must fail")
	}
	// Garbage input fails cleanly (no panic).
	if _, err := s.Open("!!!not-base64!!!", now); err == nil {
		t.Fatal("garbage must fail")
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
