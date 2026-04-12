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
	}
}

func TestModelTextHighlightsSelectedModelWithoutDefaultSuffix(t *testing.T) {
	d := newTestDaemon()
	chatID := "tg:test"
	d.store.UpdateScopeDefaults(chatID, func(def *session.ScopeDefaults) {
		def.Backend = "codex"
		def.Model = "gpt-5.4"
	})

	text := d.modelText(chatID, &session.Session{ModelOverride: "gpt-5.4"})

	if !strings.Contains(text, "<b>/m_54 GPT-5.4 ✅</b>") {
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
		def.Model = "gpt-5.4"
		def.Think = "high"
	})

	text := d.settingsText(chatID, chatID, &session.Session{
		Backend:       "codex",
		ModelOverride: "gpt-5.4",
		ThinkOverride: "high",
	})

	for _, want := range []string{
		"⚙️ Движок:",
		"🤖 Модель:",
		"🧠 Мышление:",
		"<b>/backend_codex ✅</b>",
		"<b>/m_54 GPT-5.4 ✅</b>",
		"<b>/t_high High ✅</b>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("settings text missing %q in %q", want, text)
		}
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
		def.Model = "gpt-5.4"
		def.Think = "high"
	})

	text := d.settingsText(chatID, chatID, &session.Session{
		Backend:       "codex",
		ModelOverride: "gpt-5.4",
		ThinkOverride: "high",
	})

	for _, want := range []string{
		"<b>Режим группы</b>",
		"<b>/group_on ✅</b>",
		"/group_off",
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
			ModelOverride: "gpt-5.4",
			ThinkOverride: "high",
		},
	)

	if !strings.Contains(text, "/settings — донастроить сессию") {
		t.Fatalf("created text should include settings hint: %q", text)
	}
	if !strings.Contains(text, "📂 <code>~/work</code>") {
		t.Fatalf("created text should use tilde path: %q", text)
	}
}
