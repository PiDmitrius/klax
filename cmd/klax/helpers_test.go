package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/pathutil"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/ym"
)

// newTestDaemon builds an in-memory daemon for unit tests. It ALWAYS isolates
// KLAX_CONFIG_DIR to a fresh t.TempDir(): several code paths reachable from
// commands (e.g. /groups on -> saveGroupChats -> config.Save) write straight
// to config.Dir()/config.json with no injected override, so without this a
// test exercising them clobbers the real ~/.config/klax/config.json on
// whatever host runs `go test` — which happened for real and took down the
// live klax service (2026-07-16). Every caller must pass its *testing.T so
// t.Setenv can register the restore-on-cleanup.
func newTestDaemon(t *testing.T) *daemon {
	t.Setenv("KLAX_CONFIG_DIR", t.TempDir())
	return &daemon{
		cfg:        &config.Config{DefaultBackend: "codex", DefaultCWD: "/tmp"},
		store:      &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}},
		groupChats: map[string]string{},
		groupVerb:  map[string]bool{},
	}
}

func TestModelTextHighlightsSelectedModelWithoutDefaultSuffix(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Model = "gpt-5.6-sol"
	})

	text := d.modelText(chatID, &session.Session{ModelOverride: "gpt-5.6-sol"})

	if !strings.Contains(text, "<b>/m_sol GPT-5.6 Sol ✅</b>") {
		t.Fatalf("selected model is not highlighted: %q", text)
	}
	if strings.Contains(text, "По умолчанию (") {
		t.Fatalf("default model suffix should be removed: %q", text)
	}
	if strings.Contains(text, "/m_default По умолчанию ✅") {
		t.Fatalf("default option should not be marked as selected: %q", text)
	}
}

func TestThinkTextHighlightsSelectedEffortWithoutDefaultSuffix(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Think = "high"
	})

	text := d.thinkText(chatID, &session.Session{ThinkOverride: "high"})

	if !strings.Contains(text, "<b>/t_high High ✅</b>") {
		t.Fatalf("selected effort is not highlighted: %q", text)
	}
	if strings.Contains(text, "По умолчанию (") {
		t.Fatalf("default effort suffix should be removed: %q", text)
	}
	if strings.Contains(text, "/t_default По умолчанию ✅") {
		t.Fatalf("default option should not be marked as selected: %q", text)
	}
}

func TestModelTextMarksDefaultWhenModelIsEmpty(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Model = ""
	})

	text := d.modelText(chatID, &session.Session{})

	if !strings.Contains(text, "<b>/m_default По умолчанию ✅</b>") {
		t.Fatalf("default model should be highlighted when override is empty: %q", text)
	}
}

func TestThinkTextMarksDefaultWhenThinkIsEmpty(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Think = ""
	})

	text := d.thinkText(chatID, &session.Session{})

	if !strings.Contains(text, "<b>/t_default По умолчанию ✅</b>") {
		t.Fatalf("default think should be highlighted when override is empty: %q", text)
	}
}

func TestVerboseTextDefaultsOn(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:-1001"
	d.groupChats[chatID] = "/tmp/groups/tg_-1001"

	text := d.verboseText(chatID)

	if !strings.Contains(text, "<b>/verbose_on ✅</b>") {
		t.Fatalf("verbose should default to on: %q", text)
	}
}

func TestVerboseTextMarksOff(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:-1001"
	d.groupChats[chatID] = "/tmp/groups/tg_-1001"
	d.groupVerb[chatID] = false

	text := d.verboseText(chatID)

	if !strings.Contains(text, "<b>/verbose_off ✅</b>") {
		t.Fatalf("verbose off should be highlighted: %q", text)
	}
}

func TestTildePathsInTextHandlesQuotesAndSpaces(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	in := `cd "` + home + `/My Project" && cat ` + home + `/notes.txt`
	got := pathutil.TildePathsInText(in)

	for _, want := range []string{`"~/My Project"`, `~/notes.txt`} {
		if !strings.Contains(got, want) {
			t.Fatalf("TildePathsInText missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, home) {
		t.Fatalf("TildePathsInText should replace home path in %q", got)
	}
}

func TestSettingsTextContainsBackendModelAndThinkSections(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Model = "gpt-5.6-sol"
		def.Think = "high"
	})

	text := d.settingsText(chatID, chatID, &session.Session{
		Backend:       "codex",
		ModelOverride: "gpt-5.6-sol",
		ThinkOverride: "high",
		Sandbox:       "on",
	})

	for _, want := range []string{
		"⚙️ Движок:",
		"🤖 Модель:",
		"🧠 Мышление:",
		"🔒 Sandbox:",
		"<b>/backend_codex ✅</b>",
		"<b>/m_sol GPT-5.6 Sol ✅</b>",
		"<b>/t_high High ✅</b>",
		"<b>/sandbox_on ✅</b>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("settings text missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "🗣 Verbose:") {
		t.Fatalf("personal settings should not show group verbose: %q", text)
	}
	for _, want := range []string{
		"✅</b>\n\n🤖",
		"По умолчанию\n\n🧠",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("settings text should contain a single blank line between sections, missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "\n\n\n") {
		t.Fatalf("settings text should not contain extra blank lines: %q", text)
	}
}

func TestSettingsTextShowsGroupModeSectionInGroupChat(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:-1001"
	d.groupChats[chatID] = "/tmp/groups/tg_-1001"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Model = "gpt-5.6-sol"
		def.Think = "high"
	})

	text := d.settingsText(chatID, chatID, &session.Session{
		Backend:       "codex",
		ModelOverride: "gpt-5.6-sol",
		ThinkOverride: "high",
		Sandbox:       "off",
	})

	for _, want := range []string{
		"<b>Режим группы</b>",
		"<b>/group_on ✅</b>",
		"/group_off",
		"🗣 Verbose:",
		"<b>/verbose_on ✅</b>",
		"/verbose_off",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("group settings missing %q in %q", want, text)
		}
	}
	if strings.Contains(text, "📂 <code>") {
		t.Fatalf("group settings should not show cwd: %q", text)
	}
}

func TestBackendTextDoesNotShowPinnedHint(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
	})

	text := d.backendText(chatID, &session.Session{Backend: "codex", Messages: 3})

	if !strings.Contains(text, "<b>/backend_codex ✅</b>") {
		t.Fatalf("active backend should be highlighted: %q", text)
	}
	if strings.Contains(text, "зафиксирован") {
		t.Fatalf("backend text should not show pinned hint: %q", text)
	}
}

func TestSessionCreatedTextIncludesSettingsHint(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	text := sessionCreatedText(
		&config.Config{DefaultBackend: "codex"},
		"tg:test",
		&session.ScopeDefaults{Backend: "codex"},
		&session.Session{
			Name:          "session",
			CWD:           home + "/work",
			Backend:       "codex",
			ModelOverride: "gpt-5.6-sol",
			ThinkOverride: "high",
			Sandbox:       "off",
		},
	)

	if !strings.Contains(text, "Настроить: /settings") {
		t.Fatalf("created text should include settings hint: %q", text)
	}
	if strings.Contains(text, "📂 <code>") {
		t.Fatalf("created text should not include cwd: %q", text)
	}
	if !strings.Contains(text, "🔒 Sandbox: <code>off</code>") {
		t.Fatalf("created text should include sandbox mode: %q", text)
	}
}

func TestSandboxTextListsOnBeforeOff(t *testing.T) {
	d := newTestDaemon(t)
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Sandbox = "off"
	})

	text := d.sandboxText(chatID, &session.Session{Sandbox: "off"})

	onIdx := strings.Index(text, "/sandbox_on")
	offIdx := strings.Index(text, "/sandbox_off")
	if onIdx == -1 || offIdx == -1 {
		t.Fatalf("sandbox options missing in %q", text)
	}
	if onIdx > offIdx {
		t.Fatalf("sandbox_on should be listed before sandbox_off in %q", text)
	}
}

func TestHtmlToYMMarkdownConvertsKnownTags(t *testing.T) {
	in := "<b>/s6 KLAX-Yandex</b> (активна)\n<code>x</code>\n<pre>func x() {}</pre>\n<a href=\"https://ya.ru\">ya</a>\n<p>next</p>"
	out := htmlToYMMarkdown(in)
	for _, want := range []string{"**/s6 KLAX-Yandex**", "`x`", "```\nfunc x() {}\n```", "[ya](https://ya.ru)"} {
		if !strings.Contains(out, want) {
			t.Errorf("htmlToYMMarkdown(%q) = %q, missing %q", in, out, want)
		}
	}
	if strings.ContainsAny(out, "<>") {
		t.Errorf("htmlToYMMarkdown left raw HTML in %q", out)
	}
}

func TestPlainRenderForChatDivergesYMFromVK(t *testing.T) {
	in := "<b>bold</b>"
	if got := plainRenderForChat("ym:vasya@example.org", in); got != "**bold**" {
		t.Errorf("plainRenderForChat(ym) = %q, want **bold**", got)
	}
	if got := plainRenderForChat("vk:123", in); got != "bold" {
		t.Errorf("plainRenderForChat(vk) = %q, want bold (VK has no formatting)", got)
	}
}

func TestSanitizeDirNameReplacesUnsafeChars(t *testing.T) {
	got := sanitizeDirName("ym:0/0/1ebc83a5-08e2-466e-ab2f-af7b22161adf")
	want := "ym_0_0_1ebc83a5-08e2-466e-ab2f-af7b22161adf"
	if got != want {
		t.Errorf("sanitizeDirName = %q, want %q", got, want)
	}
}

func TestSessionCWDFlattensYmGroupChatID(t *testing.T) {
	d := newTestDaemon(t)
	d.cfg.DefaultCWD = t.TempDir()
	chatID := "ym:0/0/1ebc83a5-08e2-466e-ab2f-af7b22161adf"

	cwd := d.sessionCWD(chatID)

	want := filepath.Join(d.cfg.DefaultCWD, "groups", "ym_0_0_1ebc83a5-08e2-466e-ab2f-af7b22161adf")
	if cwd != want {
		t.Fatalf("sessionCWD = %q, want %q (a single flat directory, not nested by the embedded /)", cwd, want)
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		t.Fatalf("expected sessionCWD to have created %q: %v", cwd, err)
	}
}

func TestYMThreadChatIDInheritsParentGroupModeOnce(t *testing.T) {
	t.Setenv("KLAX_CONFIG_DIR", t.TempDir())
	d := newTestDaemon(t)
	d.cfg.DefaultCWD = t.TempDir()
	parent := "ym:0/0/1ebc83a5-08e2-466e-ab2f-af7b22161adf"
	parentCWD := d.sessionCWD(parent)
	d.enableGroupChat(parent, parentCWD)
	// The parent's own settings, as if set via /backend, /model, /think,
	// /sandbox, /tty on the group before the thread was ever touched.
	d.store.UpdateScopeDefaults(parent, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Model = "gpt-5.6-sol"
		def.Think = "xhigh"
		def.Sandbox = "off"
		def.ClaudeTTY = true
	})

	threadChatID := d.ymThreadChatID(parent, 1784109957039064)

	if !strings.HasSuffix(threadChatID, "#1784109957039064") {
		t.Fatalf("ymThreadChatID = %q, want a #<thread_id> suffix", threadChatID)
	}
	if !d.isGroupChat(threadChatID) {
		t.Fatalf("thread should inherit group mode from parent, chatID=%q", threadChatID)
	}
	if _, ok := d.groupChats[threadChatID]; ok {
		t.Fatal("inherited thread must not be added to the explicit config-backed group registry")
	}
	for _, gc := range d.cfg.GroupChats {
		if gc.ID == threadChatID {
			t.Fatal("inherited thread must not be serialized into config.json")
		}
	}
	// A thread continues its parent group — same working directory, not a
	// fresh groups/<...> sibling.
	threadCWD := d.groupCWD(threadChatID)
	if threadCWD != parentCWD {
		t.Fatalf("thread should reuse the parent's CWD, got %q (parent %q)", threadCWD, parentCWD)
	}
	threadDef := d.scopeDefaults(threadChatID)
	if threadDef.GroupMode == nil || !*threadDef.GroupMode {
		t.Fatalf("thread group mode must be persisted with its session scope, got %+v", threadDef)
	}
	if threadDef.Backend != "codex" || threadDef.Model != "gpt-5.6-sol" || threadDef.Think != "xhigh" ||
		threadDef.Sandbox != "off" || !threadDef.ClaudeTTY {
		t.Fatalf("thread should inherit the parent's scope defaults, got %+v", threadDef)
	}

	// Even before the thread creates its first session, the durable marker must
	// make inheritance one-shot and preserve an explicit off toggle.
	d.disableGroupChat(threadChatID)
	if d.isGroupChat(d.ymThreadChatID(parent, 1784109957039064)) {
		t.Fatal("thread without a session must not re-inherit after an explicit off")
	}
	d.enableGroupChat(threadChatID, threadCWD)

	// Simulate the thread having actually received its first message.
	d.ensureSessionWithCWD(threadChatID, threadCWD)

	// From here on the thread is fully independent: turning its group mode
	// off must not affect the parent, and re-encountering it must not
	// re-inherit (no bouncing back to "on").
	d.disableGroupChat(threadChatID)
	if def := d.scopeDefaults(threadChatID); def.GroupMode == nil || *def.GroupMode {
		t.Fatalf("disabled thread state must persist independently in session scope, got %+v", def)
	}
	if !d.isGroupChat(parent) {
		t.Fatal("disabling the thread's group mode must not affect the parent")
	}
	if !d.store.Delete(d.sessionKey(threadChatID), 0) {
		t.Fatal("failed to simulate nuking the thread's last session")
	}
	again := d.ymThreadChatID(parent, 1784109957039064)
	if d.isGroupChat(again) {
		t.Fatal("re-encountering an already-independent thread must not re-inherit parent state")
	}
	// Changing the parent's settings AFTER the thread became independent
	// must not leak into the thread either.
	d.store.UpdateScopeDefaults(parent, func(def *session.ScopeDefaults) { def.Backend = "claude" })
	if d.scopeDefaults(threadChatID).Backend != "codex" {
		t.Fatal("thread scope defaults must not track later changes to the parent")
	}
}

func TestYMThreadChatIDKeepsPreMarkerThreadIndependent(t *testing.T) {
	t.Setenv("KLAX_CONFIG_DIR", t.TempDir())
	d := newTestDaemon(t)
	d.cfg.DefaultCWD = t.TempDir()
	parent := "ym:0/0/1ebc83a5-08e2-466e-ab2f-af7b22161adf"
	parentCWD := d.sessionCWD(parent)
	d.enableGroupChat(parent, parentCWD)

	threadChatID := ym.EncodeThreadChatID(parent, 77)
	d.store.UpdateScopeDefaults(threadChatID, func(def *session.ScopeDefaults) {
		def.Backend = "claude"
		def.CWD = parentCWD
	})
	d.ensureSessionWithCWD(threadChatID, parentCWD)

	d.ymThreadChatID(parent, 77)
	def := d.scopeDefaults(threadChatID)
	if def.GroupMode != nil {
		t.Fatalf("existing pre-marker thread must not be treated as new, got %+v", def)
	}
	if def.Backend != "claude" {
		t.Fatalf("existing pre-marker thread defaults were overwritten: %+v", def)
	}
}

// TestYMThreadChatIDUsesLiveCWDNotStaleGroupRegistry reproduces a real bug:
// /cwd only ever updates the active session's own CWD (commands.go), never
// groupChats[chatID] — so after an explicit /cwd override the registry still
// holds the group's original auto-assigned directory. A new thread must
// continue the group's ACTUAL current workspace, not that stale default.
func TestYMThreadChatIDUsesLiveCWDNotStaleGroupRegistry(t *testing.T) {
	t.Setenv("KLAX_CONFIG_DIR", t.TempDir())
	d := newTestDaemon(t)
	d.cfg.DefaultCWD = t.TempDir()
	parent := "ym:0/0/1ebc83a5-08e2-466e-ab2f-af7b22161adf"
	originalCWD := d.sessionCWD(parent)
	d.enableGroupChat(parent, originalCWD) // registers groupChats[parent] = originalCWD
	d.ensureSessionWithCWD(d.sessionKey(parent), originalCWD)

	// Simulate /cwd <newdir>: Session.CWD and ScopeDefaults.CWD change, groupChats is untouched.
	newCWD := t.TempDir()
	d.store.UpdateActive(d.sessionKey(parent), func(sess *session.Session) { sess.CWD = newCWD })
	d.store.UpdateScopeDefaults(d.sessionKey(parent), func(def *session.ScopeDefaults) { def.CWD = newCWD })
	if d.groupCWD(parent) == newCWD {
		t.Fatal("test setup invalid: groupChats registry should NOT track /cwd")
	}

	threadChatID := d.ymThreadChatID(parent, 999)

	if got := d.groupCWD(threadChatID); got != newCWD {
		t.Fatalf("thread CWD = %q, want the parent's live /cwd override %q (not the stale registry %q)",
			got, newCWD, originalCWD)
	}
	if got := d.scopeDefaults(threadChatID).CWD; got != newCWD {
		t.Fatalf("thread ScopeDefaults.CWD = %q, want the parent's override %q copied over", got, newCWD)
	}

	// The thread's own first session (created once its first message arrives) must
	// also land on the override, not the stale registry value.
	d.ensureSessionWithCWD(d.sessionKey(threadChatID), d.groupCWD(threadChatID))
	if got := d.store.Active(d.sessionKey(threadChatID)).CWD; got != newCWD {
		t.Fatalf("thread session CWD = %q, want the parent's override %q", got, newCWD)
	}
}

func TestCWDOverrideSurvivesNextInboundMessageInGroup(t *testing.T) {
	t.Setenv("KLAX_CONFIG_DIR", t.TempDir())
	d := newTestDaemon(t)
	d.cfg.DefaultCWD = t.TempDir()
	group := "ym:0/0/1ebc83a5-08e2-466e-ab2f-af7b22161adf"
	sk := d.sessionKey(group)
	originalCWD := d.sessionCWD(group)
	d.enableGroupChat(group, originalCWD) // "/groups on": registers groupChats[group] = originalCWD
	d.ensureSessionWithCWD(sk, originalCWD)

	// Simulate "/cwd <newdir>": only Session.CWD changes, groupChats registry is untouched.
	newCWD := t.TempDir()
	d.store.UpdateActive(sk, func(sess *session.Session) { sess.CWD = newCWD })

	// Every inbound message in the group re-derives forceCWD from the (now stale)
	// registry and passes it through ensureSessionWithCWD, exactly like handleInbound does.
	d.ensureSessionWithCWD(sk, d.sessionCWD(group))

	if got := d.store.Active(sk).CWD; got != newCWD {
		t.Fatalf("session CWD = %q after next inbound message, want the /cwd override %q to survive (stale registry was %q)",
			got, newCWD, originalCWD)
	}
}

func TestYMThreadChatIDNoInheritWhenParentGroupModeOff(t *testing.T) {
	t.Setenv("KLAX_CONFIG_DIR", t.TempDir())
	d := newTestDaemon(t)
	d.cfg.DefaultCWD = t.TempDir()
	parent := "ym:0/0/1ebc83a5-08e2-466e-ab2f-af7b22161adf" // /groups on never ran here

	threadChatID := d.ymThreadChatID(parent, 42)

	if d.isGroupChat(threadChatID) {
		t.Fatalf("thread must not get group mode when the parent doesn't have it, chatID=%q", threadChatID)
	}
}

// registerSelfMentionTrigger makes an @mention of the bot behave exactly like typing
// "Клакс": both YM and Telegram render a mention as literal "@<bot login/username> "
// text (confirmed against a real raw YM update; Telegram entities are offsets into a
// text that already contains the literal "@username" the same way), so registering
// "@<login>" as one more groupTriggerPrefixes entry is enough for either transport — no
// separate structured-entity parsing needed.
func TestRegisterSelfMentionTriggerMakesMentionAGroupTrigger(t *testing.T) {
	original := append([]string(nil), groupTriggerPrefixes...)
	defer func() { groupTriggerPrefixes = original }()

	registerSelfMentionTrigger("Yndx-Mssngr-jB6XY8NfmC-Bot")

	rest, ok := stripGroupTrigger("@yndx-mssngr-jb6xy8nfmc-bot TEST123")
	if !ok {
		t.Fatal("a YM mention of the bot's own login must be recognized as a group trigger")
	}
	if rest != "TEST123" {
		t.Fatalf("stripped prompt = %q, want %q", rest, "TEST123")
	}
}

func TestRegisterSelfMentionTriggerWorksForTelegramUsername(t *testing.T) {
	original := append([]string(nil), groupTriggerPrefixes...)
	defer func() { groupTriggerPrefixes = original }()

	registerSelfMentionTrigger("klax_dev_bot")

	rest, ok := stripGroupTrigger("@klax_dev_bot TEST123")
	if !ok {
		t.Fatal("a Telegram mention of the bot's own username must be recognized as a group trigger")
	}
	if rest != "TEST123" {
		t.Fatalf("stripped prompt = %q, want %q", rest, "TEST123")
	}
}

func TestRegisterSelfMentionTriggerIgnoresEmptyLogin(t *testing.T) {
	original := append([]string(nil), groupTriggerPrefixes...)
	defer func() { groupTriggerPrefixes = original }()

	registerSelfMentionTrigger("")

	if len(groupTriggerPrefixes) != len(original) {
		t.Fatalf("an empty login must not add a trigger, prefixes = %v", groupTriggerPrefixes)
	}
}

// A mention of some OTHER, unrelated user/bot whose name happens to start with the same
// letters must not false-trigger — stripGroupTrigger's existing separator requirement
// (punctuation or whitespace right after the matched prefix) already guards this; this
// just locks that guarantee in for the mention case specifically.
func TestRegisterSelfMentionTriggerDoesNotMatchUnrelatedNameWithSamePrefix(t *testing.T) {
	original := append([]string(nil), groupTriggerPrefixes...)
	defer func() { groupTriggerPrefixes = original }()

	registerSelfMentionTrigger("klax_dev_bot")

	if _, ok := stripGroupTrigger("@klax_dev_bot_2 TEST123"); ok {
		t.Fatal("a mention of a DIFFERENT bot whose username merely starts with the registered one must not trigger")
	}
}
