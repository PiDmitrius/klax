package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/PiDmitrius/klax/internal/session"
)

// uiSettingsOption is one selectable value (backend, model, effort) plus its
// human label, as rendered in the session-settings dialog's dropdowns.
type uiSettingsOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// uiSettings is the full per-session settings view the dialog renders from. It
// carries the current values, the option lists for the session's current
// backend, the guards (busy/backend-locked), and live context-window usage.
type uiSettings struct {
	Created       int64              `json:"created"`
	Name          string             `json:"name"`
	Backend       string             `json:"backend"`
	Model         string             `json:"model"` // "" = backend default
	Think         string             `json:"think"` // "" = backend default
	Sandbox       string             `json:"sandbox"`
	TTY           bool               `json:"tty"`
	CWD           string             `json:"cwd"`
	Prompt        string             `json:"prompt"` // append-system-prompt
	Messages      int                `json:"messages"`
	Busy          bool               `json:"busy"`
	BackendLocked bool               `json:"backend_locked"` // first message already sent
	TTYAvailable  bool               `json:"tty_available"`  // backend == claude
	CtxUsed       int                `json:"ctx_used"`
	CtxWindow     int                `json:"ctx_window"`
	Backends      []uiSettingsOption `json:"backends"`
	Models        []uiSettingsOption `json:"models"`
	Efforts       []uiSettingsOption `json:"efforts"`
}

// uiSettingsPatch is a partial update: only non-nil fields are applied. The UI
// sends one field per request (apply-on-change), but the handler tolerates any
// combination.
type uiSettingsPatch struct {
	Name    *string `json:"name"`
	Backend *string `json:"backend"`
	Model   *string `json:"model"`
	Think   *string `json:"think"`
	Sandbox *string `json:"sandbox"`
	TTY     *bool   `json:"tty"`
	CWD     *string `json:"cwd"`
	Prompt  *string `json:"prompt"`
}

// uiErr carries an HTTP status alongside a user-facing message so the settings
// handler can surface a precise reason (busy, locked, bad value) to the dialog.
type uiErr struct {
	status int
	msg    string
}

func (e *uiErr) Error() string { return e.msg }

func uiSettingsOptions(entries []modelEntry) []uiSettingsOption {
	out := make([]uiSettingsOption, 0, len(entries))
	for _, e := range entries {
		out = append(out, uiSettingsOption{Value: e.model, Label: e.label})
	}
	return out
}

func validOption(entries []modelEntry, value string) bool {
	for _, e := range entries {
		if e.model == value {
			return true
		}
	}
	return false
}

// uiSessionSettings builds the settings view for one session (by Created).
func (d *daemon) uiSessionSettings(sk string, created int64) (*uiSettings, bool) {
	sess := d.store.Get(sk, created)
	if sess == nil {
		return nil, false
	}
	def := d.scopeDefaults(sk)
	backend := resolveSessionBackend(sess, def, d.cfg.GetDefaultBackend())
	return &uiSettings{
		Created:       sess.Created,
		Name:          sess.Name,
		Backend:       backend,
		Model:         sess.ModelOverride,
		Think:         sess.ThinkOverride,
		Sandbox:       effectiveSandboxMode(def, sess),
		TTY:           sess.ClaudeTTY,
		CWD:           sess.CWD,
		Prompt:        sess.AppendSystemPrompt,
		Messages:      sess.Messages,
		Busy:          d.isSessionBusy(sk, created),
		BackendLocked: sess.Messages > 0,
		TTYAvailable:  backend == "claude",
		CtxUsed:       sess.ContextUsed,
		CtxWindow:     sess.ContextWindow,
		Backends:      []uiSettingsOption{{Value: "claude", Label: "Claude"}, {Value: "codex", Label: "Codex"}},
		Models:        uiSettingsOptions(modelsForBackend(backend)),
		Efforts:       uiSettingsOptions(effortsForBackend(backend)),
	}, true
}

// applyUISessionSettings applies a partial settings change to one session.
// Unlike the messenger /settings handlers it edits ONLY the per-session
// overrides (never the scope defaults that seed new sessions): each UI tab is
// configured independently. The same guards apply — a run-affecting change is
// refused while the session is busy, and the backend is locked once the first
// message has been sent (model/think are backend-specific, so switching the
// backend resets them).
func (d *daemon) applyUISessionSettings(sk string, created int64, p uiSettingsPatch) error {
	sess := d.store.Get(sk, created)
	if sess == nil {
		return &uiErr{http.StatusNotFound, "сессия не найдена"}
	}
	// Validate the WHOLE patch against current state BEFORE mutating anything, so a
	// partially-valid combined patch (e.g. {backend:"codex", tty:true}) is rejected
	// cleanly instead of leaving the session half-changed.
	newName := sess.Name
	if p.Name != nil {
		newName = strings.TrimSpace(*p.Name)
		if newName == "" {
			return &uiErr{http.StatusBadRequest, "имя не может быть пустым"}
		}
	}
	touchesRun := p.Backend != nil || p.Model != nil || p.Think != nil || p.Sandbox != nil || p.TTY != nil || p.CWD != nil || p.Prompt != nil
	if touchesRun && d.isSessionBusy(sk, created) {
		return &uiErr{http.StatusConflict, "Сессия занята — параметры запуска нельзя менять до завершения."}
	}
	backend := resolveSessionBackend(sess, d.scopeDefaults(sk), d.cfg.GetDefaultBackend())
	backendChanged := false
	if p.Backend != nil && *p.Backend != backend {
		if *p.Backend != "claude" && *p.Backend != "codex" {
			return &uiErr{http.StatusBadRequest, "движок: claude или codex"}
		}
		if sess.Messages > 0 {
			return &uiErr{http.StatusConflict, "Движок нельзя изменить после первого сообщения."}
		}
		backend = *p.Backend
		backendChanged = true
	}
	// model/think are validated against the EFFECTIVE (possibly new) backend.
	if p.Model != nil && *p.Model != "" && !validOption(modelsForBackend(backend), *p.Model) {
		return &uiErr{http.StatusBadRequest, "неизвестная модель"}
	}
	if p.Think != nil && *p.Think != "" && !validOption(effortsForBackend(backend), *p.Think) {
		return &uiErr{http.StatusBadRequest, "неизвестный уровень мышления"}
	}
	if p.Sandbox != nil && *p.Sandbox != "on" && *p.Sandbox != "off" {
		return &uiErr{http.StatusBadRequest, "sandbox: on или off"}
	}
	if p.TTY != nil && *p.TTY && backend != "claude" {
		return &uiErr{http.StatusBadRequest, "TTY доступен только для claude"}
	}
	newCWD := sess.CWD
	if p.CWD != nil {
		newCWD = strings.TrimSpace(*p.CWD)
		if newCWD == "" {
			return &uiErr{http.StatusBadRequest, "рабочий каталог не может быть пустым"}
		}
	}
	newPrompt := sess.AppendSystemPrompt
	if p.Prompt != nil {
		newPrompt = strings.TrimSpace(*p.Prompt) // empty clears the append-prompt
	}

	// All checks passed — apply atomically in a single update.
	d.store.UpdateSession(sk, created, func(cur *session.Session) {
		cur.Name = newName
		if backendChanged {
			cur.Backend = backend
			// model/think are backend-specific — reset unless this same patch sets
			// them; TTY exists only for claude.
			if p.Model == nil {
				cur.ModelOverride = ""
			}
			if p.Think == nil {
				cur.ThinkOverride = ""
			}
			if backend != "claude" {
				cur.ClaudeTTY = false
			}
		}
		if p.Model != nil {
			cur.ModelOverride = *p.Model
		}
		if p.Think != nil {
			cur.ThinkOverride = *p.Think
		}
		if p.Sandbox != nil {
			cur.Sandbox = *p.Sandbox
		}
		if p.TTY != nil {
			cur.ClaudeTTY = *p.TTY
		}
		if p.CWD != nil {
			cur.CWD = newCWD
		}
		if p.Prompt != nil {
			cur.AppendSystemPrompt = newPrompt
		}
	})
	d.saveStore()
	d.broadcastSessions(sk)
	return nil
}

// handleSettings serves the per-session settings dialog: GET returns the view
// for ?session=<created>; POST applies a uiSettingsPatch and returns the
// refreshed view (so the dialog re-renders from the authoritative state — e.g.
// the new backend's model list after a backend switch).
func (s *uiServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	switch r.Method {
	case http.MethodGet:
		created, _ := strconv.ParseInt(r.URL.Query().Get("session"), 10, 64)
		settings, ok := s.d.uiSessionSettings(sk, created)
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(settings)
	case http.MethodPost:
		var body struct {
			Session int64 `json:"session"`
			uiSettingsPatch
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Session <= 0 {
			http.Error(w, "a positive session is required", http.StatusBadRequest)
			return
		}
		if err := s.d.applyUISessionSettings(sk, body.Session, body.uiSettingsPatch); err != nil {
			if ue, ok := err.(*uiErr); ok {
				http.Error(w, ue.msg, ue.status)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		settings, ok := s.d.uiSessionSettings(sk, body.Session)
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(settings)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
