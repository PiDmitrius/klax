package main

import (
	"os"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/pathutil"
	"github.com/PiDmitrius/klax/internal/session"
)

func newTestDaemon() *daemon {
	return &daemon{
		cfg:        &config.Config{DefaultBackend: "codex", DefaultCWD: "/tmp"},
		store:      &session.Store{Chats: map[string]*session.ChatSessions{}, Scope: map[string]*session.ScopeDefaults{}},
		groupChats: map[string]string{},
		groupVerb:  map[string]bool{},
	}
}

func TestModelTextHighlightsSelectedModelWithoutDefaultSuffix(t *testing.T) {
	d := newTestDaemon()
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
	d := newTestDaemon()
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
	d := newTestDaemon()
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
	d := newTestDaemon()
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
	d := newTestDaemon()
	chatID := "tg:-1001"
	d.groupChats[chatID] = "/tmp/groups/tg_-1001"

	text := d.verboseText(chatID)

	if !strings.Contains(text, "<b>/verbose_on ✅</b>") {
		t.Fatalf("verbose should default to on: %q", text)
	}
}

func TestVerboseTextMarksOff(t *testing.T) {
	d := newTestDaemon()
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
	d := newTestDaemon()
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
	d := newTestDaemon()
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
	d := newTestDaemon()
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
	d := newTestDaemon()
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
