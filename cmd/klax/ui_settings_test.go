package main

import (
	"testing"

	"github.com/PiDmitrius/klax/internal/session"
)

func TestValidateSettingsPatchLocksCWDAfterFirstMessage(t *testing.T) {
	newCWD := t.TempDir()
	cur := &session.Session{CWD: t.TempDir(), Messages: 1}

	_, err := validateSettingsPatch(cur, "claude", false, uiSettingsPatch{CWD: &newCWD})

	uerr, ok := err.(*uiErr)
	if !ok || uerr == nil {
		t.Fatalf("err = %v, want a *uiErr conflict (cwd locked like backend)", err)
	}
	if uerr.status != 409 {
		t.Fatalf("status = %d, want 409 Conflict", uerr.status)
	}
}

func TestValidateSettingsPatchAllowsCWDBeforeFirstMessage(t *testing.T) {
	newCWD := t.TempDir()
	cur := &session.Session{CWD: t.TempDir(), Messages: 0}

	r, err := validateSettingsPatch(cur, "claude", false, uiSettingsPatch{CWD: &newCWD})

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.cwd != newCWD {
		t.Fatalf("resolved cwd = %q, want %q", r.cwd, newCWD)
	}
}

// createUISessionAtomic replaces the WHOLE ScopeDefaults record (Store.AddWithDefaults does
// `*s.scope(chatID) = *defaults`) — an already-set /cwd default must survive confirming a
// Web UI "new session" draft with no explicit cwd override, not get reset to "".
func TestCreateUISessionAtomicPreservesExistingCWDDefault(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	sk := d.sessionKey(chatID)
	existingCWD := t.TempDir()
	d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) { def.CWD = existingCWD })

	sess, err := d.createUISessionAtomic(sk, chatID, uiSettingsPatch{})
	if err != nil {
		t.Fatalf("createUISessionAtomic: %v", err)
	}
	if sess.CWD != existingCWD {
		t.Fatalf("new session CWD = %q, want the existing /cwd default %q", sess.CWD, existingCWD)
	}
	if got := d.scopeDefaults(sk).CWD; got != existingCWD {
		t.Fatalf("scope defaults CWD after atomic create = %q, want it preserved as %q", got, existingCWD)
	}
}
