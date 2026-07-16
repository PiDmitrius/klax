package main

import (
	"context"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/session"
)

func TestSessionNameArg(t *testing.T) {
	if got := sessionNameArg(nil); got != "session" {
		t.Fatalf("sessionNameArg(nil) = %q, want %q", got, "session")
	}
	if got := sessionNameArg([]string{"my", "work"}); got != "my work" {
		t.Fatalf("sessionNameArg = %q, want %q", got, "my work")
	}
}

func TestDeleteInactiveSessions(t *testing.T) {
	const sk = "tg:1"
	st := &session.Store{
		Chats: make(map[string]*session.ChatSessions),
		Scope: make(map[string]*session.ScopeDefaults),
	}
	st.New(sk, "one", "/tmp", session.ScopeDefaults{})
	st.New(sk, "two", "/tmp", session.ScopeDefaults{})
	st.New(sk, "three", "/tmp", session.ScopeDefaults{}) // newest is active

	sessions := st.SessionsFor(sk)
	if len(sessions) != 3 {
		t.Fatalf("setup: got %d sessions, want 3", len(sessions))
	}
	// First session is busy (has an in-flight run cancel handle): /nuke must
	// abort and delete it, not spare it.
	busyCreated := sessions[0].Created
	// Second session is idle but has a leftover runner; deleting it must also
	// drop that runner.
	idleCreated := sessions[1].Created
	activeCreated := sessions[2].Created

	_, cancel := context.WithCancel(context.Background())
	cancelled := false
	d := &daemon{store: st, runners: make(map[runnerKey]*sessionRunner)}
	d.runners[runnerKey{sk: sk, created: busyCreated}] = &sessionRunner{
		runner: runner.New(),
		cancel: func() { cancelled = true; cancel() },
	}
	d.runners[runnerKey{sk: sk, created: idleCreated}] = &sessionRunner{runner: runner.New()}

	deleted, aborted := d.deleteInactiveSessions(sk)
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if aborted != 1 {
		t.Fatalf("aborted = %d, want 1", aborted)
	}
	if !cancelled {
		t.Fatal("busy session run was not cancelled")
	}

	remaining := st.SessionsFor(sk)
	if len(remaining) != 1 {
		t.Fatalf("remaining = %d sessions, want 1 (only the new active session)", len(remaining))
	}
	if !remaining[0].Active || remaining[0].Created != activeCreated {
		t.Fatalf("surviving session = %+v, want the active one", remaining[0])
	}

	if _, ok := d.runners[runnerKey{sk: sk, created: idleCreated}]; ok {
		t.Fatal("runner for deleted idle session was not dropped")
	}
	if _, ok := d.runners[runnerKey{sk: sk, created: busyCreated}]; ok {
		t.Fatal("runner for nuked busy session was not dropped")
	}
}

// A session whose message was dequeued but whose run has not yet installed its
// cancel handle is "processing" with cancel == nil. /nuke must still recognise
// that as live work, flag the runner closing, and report it as aborted.
func TestAbortSessionDetectsProcessingAndFlagsClosing(t *testing.T) {
	const sk = "tg:1"
	st := &session.Store{
		Chats: make(map[string]*session.ChatSessions),
		Scope: make(map[string]*session.ScopeDefaults),
	}
	st.New(sk, "one", "/tmp", session.ScopeDefaults{})
	created := st.SessionsFor(sk)[0].Created

	d := &daemon{store: st, runners: make(map[runnerKey]*sessionRunner)}
	sr := &sessionRunner{runner: runner.New(), processing: true} // dequeued, cancel not set yet
	d.runners[runnerKey{sk: sk, created: created}] = sr

	// Plain /abort cannot stop a run with no cancel handle yet, so it must not
	// claim it did — original IsBusy()-only behaviour, no closing side effect.
	if d.abortSession(sk, created, false) {
		t.Fatal("/abort must not report work for a run with no cancel handle yet")
	}
	sr.mu.Lock()
	flaggedByAbort := sr.closing
	sr.mu.Unlock()
	if flaggedByAbort {
		t.Fatal("/abort must not flag the runner closing")
	}

	// /nuke (closing=true) must recognise the processing run, flag it closing so
	// the starting run bails, and report it as aborted.
	if !d.abortSession(sk, created, true) {
		t.Fatal("/nuke must report work for a processing session")
	}
	sr.mu.Lock()
	closing := sr.closing
	sr.mu.Unlock()
	if !closing {
		t.Fatal("closing flag was not set so a starting run would not bail")
	}
}

func TestCreateSessionUsesUserDefaultCWD(t *testing.T) {
	userCWD := t.TempDir()
	st := &session.Store{
		Chats: make(map[string]*session.ChatSessions),
		Scope: make(map[string]*session.ScopeDefaults),
	}
	d := &daemon{
		cfg: &config.Config{
			DefaultCWD: "/tmp/global",
			Users: []config.UserIdentity{
				{ID: "alice", CWD: userCWD},
			},
		},
		store: st,
	}

	sess, _ := d.createSession("ui:alice", "user:alice", "main")

	if sess.CWD != userCWD {
		t.Fatalf("session cwd = %q, want user default", sess.CWD)
	}
}

func TestCreateSessionFallsBackWhenUserDefaultCWDIsInvalid(t *testing.T) {
	globalCWD := t.TempDir()
	st := &session.Store{
		Chats: make(map[string]*session.ChatSessions),
		Scope: make(map[string]*session.ScopeDefaults),
	}
	d := &daemon{
		cfg: &config.Config{
			DefaultCWD: globalCWD,
			Users: []config.UserIdentity{
				{ID: "alice", CWD: "/no/such/klax/cwd"},
			},
		},
		store: st,
	}

	sess, _ := d.createSession("ui:alice", "user:alice", "main")

	if sess.CWD != globalCWD {
		t.Fatalf("session cwd = %q, want global fallback", sess.CWD)
	}
}

func TestEnsureSessionUsesUserDefaultOnlyForNewSession(t *testing.T) {
	userCWD := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	st, err := session.LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	d := &daemon{
		cfg: &config.Config{
			DefaultCWD: "/tmp/global",
			Users: []config.UserIdentity{
				{ID: "alice", CWD: userCWD},
			},
		},
		store: st,
	}

	d.ensureSessionWithCWD("user:alice", "")
	sess := st.Active("user:alice")
	if sess == nil || sess.CWD != userCWD {
		t.Fatalf("new session = %+v, want user cwd", sess)
	}

	st.UpdateActive("user:alice", func(sess *session.Session) {
		sess.CWD = "/tmp/custom-session"
	})
	d.ensureSessionWithCWD("user:alice", "")
	sess = st.Active("user:alice")
	if sess.CWD != "/tmp/custom-session" {
		t.Fatalf("existing session cwd = %q, want preserved custom cwd", sess.CWD)
	}
}

// A brand-new session (none active yet) must prefer an explicit ScopeDefaults.CWD over
// whatever forceCWD the caller derived — otherwise a chat whose sessions were all deleted
// loses its /cwd default the moment the next message recreates one.
func TestEnsureSessionWithCWDPrefersScopeDefaultOverForceCWD(t *testing.T) {
	userCWD := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	st, err := session.LoadStore()
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	d := &daemon{cfg: &config.Config{DefaultCWD: "/tmp/global"}, store: st}
	st.UpdateScopeDefaults("user:alice", func(def *session.ScopeDefaults) { def.CWD = userCWD })

	d.ensureSessionWithCWD("user:alice", "/tmp/some-other-forced-cwd")

	sess := st.Active("user:alice")
	if sess == nil || sess.CWD != userCWD {
		t.Fatalf("new session = %+v, want ScopeDefaults.CWD %q to win over forceCWD", sess, userCWD)
	}
}

func TestNormalizeCommandGroupAliases(t *testing.T) {
	tests := []struct {
		inCmd   string
		wantCmd string
		wantArg string
	}{
		{inCmd: "/group_on", wantCmd: "/groups", wantArg: "on"},
		{inCmd: "/group_off", wantCmd: "/groups", wantArg: "off"},
		{inCmd: "/groups_on", wantCmd: "/groups", wantArg: "on"},
		{inCmd: "/groups_off", wantCmd: "/groups", wantArg: "off"},
	}

	for _, tt := range tests {
		gotCmd, gotArgs := normalizeCommand(tt.inCmd, nil)
		if gotCmd != tt.wantCmd {
			t.Fatalf("%s normalized cmd = %q, want %q", tt.inCmd, gotCmd, tt.wantCmd)
		}
		if len(gotArgs) != 1 || gotArgs[0] != tt.wantArg {
			t.Fatalf("%s normalized args = %v, want [%q]", tt.inCmd, gotArgs, tt.wantArg)
		}
	}
}

func TestNormalizeCommandVerboseAliases(t *testing.T) {
	tests := []struct {
		inCmd   string
		wantCmd string
		wantArg string
	}{
		{inCmd: "/verbose_on", wantCmd: "/verbose", wantArg: "on"},
		{inCmd: "/verbose_off", wantCmd: "/verbose", wantArg: "off"},
	}

	for _, tt := range tests {
		gotCmd, gotArgs := normalizeCommand(tt.inCmd, nil)
		if gotCmd != tt.wantCmd {
			t.Fatalf("%s normalized cmd = %q, want %q", tt.inCmd, gotCmd, tt.wantCmd)
		}
		if len(gotArgs) != 1 || gotArgs[0] != tt.wantArg {
			t.Fatalf("%s normalized args = %v, want [%q]", tt.inCmd, gotArgs, tt.wantArg)
		}
	}
}

func TestNormalizeCommandTTYAliases(t *testing.T) {
	tests := []struct {
		inCmd   string
		wantCmd string
		wantArg string
	}{
		{inCmd: "/tty_on", wantCmd: "/tty", wantArg: "on"},
		{inCmd: "/tty_off", wantCmd: "/tty", wantArg: "off"},
	}

	for _, tt := range tests {
		gotCmd, gotArgs := normalizeCommand(tt.inCmd, nil)
		if gotCmd != tt.wantCmd {
			t.Fatalf("%s normalized cmd = %q, want %q", tt.inCmd, gotCmd, tt.wantCmd)
		}
		if len(gotArgs) != 1 || gotArgs[0] != tt.wantArg {
			t.Fatalf("%s normalized args = %v, want [%q]", tt.inCmd, gotArgs, tt.wantArg)
		}
	}
}

func TestExpandBypassUnderscore(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/bypass_ping", "/bypass ping"},
		{"/bypass_ping arg2", "/bypass ping arg2"},
		{"/bypass_ping@klaxbot arg", "/bypass ping arg"},
		{"/BYPASS_PING", "/bypass PING"},
		{"/bypass ping", "/bypass ping"},
		{"/bypass", "/bypass"},
		{"/bypass_", "/bypass_"},
		{"/sessions@klaxbot", "/sessions@klaxbot"},
		{"/prompt_foo", "/prompt_foo"},
	}
	for _, tt := range tests {
		if got := expandBypassUnderscore(tt.in); got != tt.want {
			t.Errorf("expandBypassUnderscore(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAbortReplyText(t *testing.T) {
	if abortReplyText != "❌ Прерваны все сообщения в сессии." {
		t.Fatalf("abortReplyText = %q", abortReplyText)
	}
}

func TestCWDCommandBecomesNewSessionDefault(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	sk := d.sessionKey(chatID)
	d.store.Ensure(sk, "default", t.TempDir(), d.fallbackScopeDefaults())

	newCWD := t.TempDir()
	d.handleCommand(chatID, "m1", "/cwd "+newCWD)

	if got := d.store.Active(sk).CWD; got != newCWD {
		t.Fatalf("active session CWD = %q, want %q", got, newCWD)
	}
	if got := d.scopeDefaults(sk).CWD; got != newCWD {
		t.Fatalf("scope defaults CWD = %q, want %q — /cwd should become the default for the next session, like /backend", got, newCWD)
	}

	// /new should pick up the /cwd override, not some other stale default.
	sess, _ := d.createSession(chatID, sk, "second")
	if sess.CWD != newCWD {
		t.Fatalf("new session CWD = %q, want the /cwd override %q", sess.CWD, newCWD)
	}
}

// /groups on must resolve cwd the same way createSession does (ScopeDefaults.CWD first) —
// otherwise it resurrects the group's stale original registry cwd over a /cwd override made
// before the group was ever toggled on, desyncing Session.CWD from ScopeDefaults.CWD again.
func TestGroupsOnUsesScopeDefaultsCWDNotStaleRegistry(t *testing.T) {
	d := newTestDaemon(t)
	d.cfg.DefaultCWD = t.TempDir()
	chatID := "tg:-1001"
	sk := d.sessionKey(chatID)

	originalCWD := d.sessionCWD(chatID)
	d.enableGroupChat(chatID, originalCWD)
	d.store.Ensure(sk, "default", originalCWD, d.fallbackScopeDefaults())

	// Simulate /cwd <newdir> before the first message: Session.CWD and ScopeDefaults.CWD change,
	// the groupChats registry (only ever seeded once) is left holding the stale original.
	newCWD := t.TempDir()
	d.store.UpdateActive(sk, func(sess *session.Session) { sess.CWD = newCWD })
	d.store.UpdateScopeDefaults(sk, func(def *session.ScopeDefaults) { def.CWD = newCWD })

	d.handleCommand(chatID, "m1", "/groups on")

	if got := d.groupCWD(chatID); got != newCWD {
		t.Fatalf("group registry CWD = %q after /groups on, want the /cwd override %q (not stale %q)", got, newCWD, originalCWD)
	}
	if got := d.store.Active(sk).CWD; got != newCWD {
		t.Fatalf("active session CWD = %q after /groups on, want %q — must not resurrect the stale registry cwd", got, newCWD)
	}
}

func TestCWDCommandLocksAfterFirstMessage(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	sk := d.sessionKey(chatID)
	originalCWD := t.TempDir()
	d.store.Ensure(sk, "default", originalCWD, d.fallbackScopeDefaults())
	d.store.UpdateActive(sk, func(sess *session.Session) { sess.Messages = 1 })

	d.handleCommand(chatID, "m1", "/cwd "+t.TempDir())

	if got := d.store.Active(sk).CWD; got != originalCWD {
		t.Fatalf("session CWD = %q after /cwd on a started session, want it to stay %q (locked, like /backend)", got, originalCWD)
	}
}
