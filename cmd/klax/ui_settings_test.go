package main

import (
	"net/http"
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

// applyUISessionSettingsCore must re-check the cwd lock itself, not rely solely on the
// snapshot validateSettingsPatch validated earlier — closing the gap where a message
// starts and finishes between that snapshot and the actual mutation.
func TestApplyUISessionSettingsCoreRejectsCWDOnceMessagesStarted(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	sk := d.sessionKey(chatID)
	sess := d.store.Ensure(sk, "default", t.TempDir(), d.fallbackScopeDefaults())
	d.store.UpdateSession(sk, sess.Created, func(s *session.Session) { s.Messages = 1 })

	newCWD := t.TempDir()
	err := d.applyUISessionSettingsCore(sk, sess.Created, uiSettingsPatch{CWD: &newCWD})

	uerr, ok := err.(*uiErr)
	if !ok || uerr.status != 409 {
		t.Fatalf("err = %v, want a 409 *uiErr conflict", err)
	}
	if got := d.store.Get(sk, sess.Created).CWD; got == newCWD {
		t.Fatal("cwd must not have been applied once Messages > 0")
	}
}

// A session already gone before the request is handled (stale created id) must 404 via
// the top-of-function Get check — this does NOT exercise UpdateSessionChecked's
// ErrSessionNotFound branch (see TestMapSessionStoreErrConvertsErrSessionNotFoundTo404 for
// that; a real "deleted mid-flight, between Get and the store mutation" race can't be
// reproduced deterministically without an injected barrier).
func TestApplyUISessionSettingsCoreReturns404ForAlreadyDeletedSession(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	sk := d.sessionKey(chatID)
	sess := d.store.Ensure(sk, "default", t.TempDir(), d.fallbackScopeDefaults())
	if !d.store.Delete(sk, 0) {
		t.Fatal("test setup: could not delete the session")
	}

	newCWD := t.TempDir()
	err := d.applyUISessionSettingsCore(sk, sess.Created, uiSettingsPatch{CWD: &newCWD})

	uerr, ok := err.(*uiErr)
	if !ok || uerr.status != 404 {
		t.Fatalf("err = %v, want a 404 *uiErr, not a generic error that the HTTP handler would turn into a 500", err)
	}
}

// mapSessionStoreErr is what actually converts UpdateSessionChecked's ErrSessionNotFound
// (a session deleted between the initial Get and the store mutation) into a 404 uiErr —
// tested directly since the real race can't be reproduced deterministically in-process.
func TestMapSessionStoreErrConvertsErrSessionNotFoundTo404(t *testing.T) {
	err := mapSessionStoreErr(session.ErrSessionNotFound)

	uerr, ok := err.(*uiErr)
	if !ok || uerr.status != 404 {
		t.Fatalf("err = %v, want a 404 *uiErr", err)
	}
}

func TestMapSessionStoreErrPassesThroughOtherErrors(t *testing.T) {
	other := &uiErr{http.StatusConflict, "something else"}

	if got := mapSessionStoreErr(other); got != other {
		t.Fatalf("mapSessionStoreErr must pass through non-ErrSessionNotFound errors unchanged, got %v", got)
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
