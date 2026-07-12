package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/PiDmitrius/klax/internal/pathutil"
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
// backend, and the guards (busy/backend-locked). Context usage is NOT here — it
// lives inline in the chat, so the dialog no longer duplicates it.
type uiSettings struct {
	Created int64  `json:"created"`
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Model   string `json:"model"` // "" = backend default
	Think   string `json:"think"` // "" = backend default
	// Read-only facts shown as "additional parameters" (settings dialog): the model the
	// backend ACTUALLY answered with last (may differ from the selected default) and the
	// resolved session UUID. Both empty until the first response lands.
	AssignedModel string             `json:"assigned_model,omitempty"`
	SessionID     string             `json:"session_id,omitempty"`
	Sandbox       string             `json:"sandbox"`
	TTY           bool               `json:"tty"`
	CWD           string             `json:"cwd"`    // ~-abbreviated for display; the server re-expands ~ on save
	Prompt        string             `json:"prompt"` // append-system-prompt
	Busy          bool               `json:"busy"`
	BackendLocked bool               `json:"backend_locked"` // first message already sent
	TTYAvailable  bool               `json:"tty_available"`  // backend == claude
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

// draftHasFields reports whether a new-session draft patch carries any override to
// apply after creation (an all-nil patch means "just create with defaults").
func draftHasFields(p uiSettingsPatch) bool {
	return p.Name != nil || p.Backend != nil || p.Model != nil || p.Think != nil ||
		p.Sandbox != nil || p.TTY != nil || p.CWD != nil || p.Prompt != nil
}

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
		AssignedModel: sess.Model,
		SessionID:     sess.ID,
		Sandbox:       effectiveSandboxMode(def, sess),
		TTY:           sess.ClaudeTTY,
		CWD:           pathutil.TildePathsInText(sess.CWD),
		Prompt:        sess.AppendSystemPrompt,
		Busy:          d.isSessionBusy(sk, created),
		BackendLocked: sess.Messages > 0,
		TTYAvailable:  backend == "claude",
		Backends:      []uiSettingsOption{{Value: "claude", Label: "Claude"}, {Value: "codex", Label: "Codex"}},
		Models:        uiSettingsOptions(modelsForBackend(backend)),
		Efforts:       uiSettingsOptions(effortsForBackend(backend)),
	}, true
}

// uiDraftSettings builds the settings view for the "new session" draft dialog — a
// session that does not exist yet (Created:0). It mirrors exactly what createSession
// would seed (scope-default backend/model/think/sandbox/tty + the default cwd), so
// "confirm with no changes" produces the same session the old immediate-create did.
// backendOverride previews a different backend's option lists while the draft is open;
// switching backends resets the model/think choices (they are backend-specific).
func (d *daemon) uiDraftSettings(sk, chatID, backendOverride string) *uiSettings {
	// Seed the draft from the SCOPE DEFAULTS — the durable per-chat "new session template" that the
	// messenger also uses (store.New reads it; /backend, /model, … and UI session creation write it).
	// This is what makes a draft inherit the last-configured session GENERALLY: the template survives
	// deleting that session, so a new draft never falls back to some other surviving tab's settings.
	def := d.scopeDefaults(sk)
	backend := resolveSessionBackend(nil, def, d.cfg.GetDefaultBackend())
	model, think, tty := def.Model, def.Think, def.ClaudeTTY
	if backendOverride == "claude" || backendOverride == "codex" {
		if backendOverride != backend {
			// A previewed backend that differs: its model/think lists don't apply, so reset
			// those (mirrors the server's backend-switch reset for a real session).
			model, think, tty = "", "", false
		}
		backend = backendOverride
	}
	if backend != "claude" {
		tty = false
	}
	return &uiSettings{
		Created:      0,
		Name:         "",
		Backend:      backend,
		Model:        model,
		Think:        think,
		Sandbox:      effectiveSandboxMode(def, nil),
		TTY:          tty,
		CWD:          pathutil.TildePathsInText(d.defaultSessionCWD(chatID, sk)),
		TTYAvailable: backend == "claude",
		Backends:     []uiSettingsOption{{Value: "claude", Label: "Claude"}, {Value: "codex", Label: "Codex"}},
		Models:       uiSettingsOptions(modelsForBackend(backend)),
		Efforts:      uiSettingsOptions(effortsForBackend(backend)),
	}
}

// applyUISessionSettings validates + applies a partial settings change to one session and PERSISTS it
// (save + broadcast). Unlike the messenger /settings handlers it edits ONLY the per-session overrides
// (never the scope defaults that seed new sessions): each UI tab is configured independently. The same
// guards apply — a run-affecting change is refused while the session is busy, and the backend is
// locked once the first message has been sent (model/think are backend-specific, so switching the
// backend resets them).
func (d *daemon) applyUISessionSettings(sk string, created int64, p uiSettingsPatch) error {
	if err := d.applyUISessionSettingsCore(sk, created, p); err != nil {
		return err
	}
	d.saveStore()
	d.broadcastSessions(sk)
	return nil
}

// applyUISessionSettingsCore runs the validation + in-memory mutation but does NOT persist — the
// caller owns the save + broadcast.
func (d *daemon) applyUISessionSettingsCore(sk string, created int64, p uiSettingsPatch) error {
	sess := d.store.Get(sk, created)
	if sess == nil {
		return &uiErr{http.StatusNotFound, "Сессия не найдена"}
	}
	backend := resolveSessionBackend(sess, d.scopeDefaults(sk), d.cfg.GetDefaultBackend())
	// Validate the WHOLE patch (incl. cwd I/O) BEFORE taking the store lock, then apply the resolved
	// result in a single UpdateSession — so a rejected patch never half-changes the session.
	r, err := validateSettingsPatch(sess, backend, d.isSessionBusy(sk, created), p)
	if err != nil {
		return err
	}
	d.store.UpdateSession(sk, created, func(cur *session.Session) { applySettingsPatch(cur, r) })
	return nil
}

// resolvedPatch is a validated settings patch: the raw patch plus the resolved string values whose
// computation needs I/O or backend context (name, cwd, prompt, effective backend). It is the SINGLE
// bridge between validation (validateSettingsPatch) and mutation (applySettingsPatch), used by both
// the settings-edit path and the atomic new-session path so there is ONE source of validation truth.
type resolvedPatch struct {
	p                 uiSettingsPatch
	name, cwd, prompt string
	backend           string
	backendChanged    bool
}

// validateSettingsPatch validates `p` against `cur`'s current state (`backend` = its effective
// backend, `busy` gates run-affecting changes) and resolves the derived values. It performs the cwd
// filesystem check here so the later mutation holds no lock during I/O. It never mutates `cur`.
func validateSettingsPatch(cur *session.Session, backend string, busy bool, p uiSettingsPatch) (resolvedPatch, error) {
	r := resolvedPatch{p: p, name: cur.Name, cwd: cur.CWD, prompt: cur.AppendSystemPrompt, backend: backend}
	if p.Name != nil {
		r.name = strings.TrimSpace(*p.Name)
		if r.name == "" {
			return r, &uiErr{http.StatusBadRequest, "Имя не может быть пустым"}
		}
	}
	touchesRun := p.Backend != nil || p.Model != nil || p.Think != nil || p.Sandbox != nil || p.TTY != nil || p.CWD != nil || p.Prompt != nil
	if touchesRun && busy {
		return r, &uiErr{http.StatusConflict, "Сессия занята — параметры запуска нельзя менять до завершения."}
	}
	if p.Backend != nil && *p.Backend != backend {
		if *p.Backend != "claude" && *p.Backend != "codex" {
			return r, &uiErr{http.StatusBadRequest, "Движок: claude или codex"}
		}
		if cur.Messages > 0 {
			return r, &uiErr{http.StatusConflict, "Движок нельзя изменить после первого сообщения."}
		}
		r.backend = *p.Backend
		r.backendChanged = true
	}
	// model/think are validated against the EFFECTIVE (possibly new) backend.
	if p.Model != nil && *p.Model != "" && !validOption(modelsForBackend(r.backend), *p.Model) {
		return r, &uiErr{http.StatusBadRequest, "Неизвестная модель"}
	}
	if p.Think != nil && *p.Think != "" && !validOption(effortsForBackend(r.backend), *p.Think) {
		return r, &uiErr{http.StatusBadRequest, "Неизвестный уровень мышления"}
	}
	if p.Sandbox != nil && *p.Sandbox != "on" && *p.Sandbox != "off" {
		return r, &uiErr{http.StatusBadRequest, "sandbox: on или off"}
	}
	if p.TTY != nil && *p.TTY && r.backend != "claude" {
		return r, &uiErr{http.StatusBadRequest, "TTY доступен только для claude"}
	}
	if p.CWD != nil {
		cwd, err := resolveWorkingDir(*p.CWD)
		if err != nil {
			return r, &uiErr{http.StatusBadRequest, err.Error()}
		}
		r.cwd = cwd
	}
	if p.Prompt != nil {
		r.prompt = strings.TrimSpace(*p.Prompt) // empty clears the append-prompt
	}
	return r, nil
}

// applySettingsPatch applies a validated patch to `cur`. Pure (no I/O, no store) so it can run under
// the store lock (edit path) or on a not-yet-inserted session (atomic create).
func applySettingsPatch(cur *session.Session, r resolvedPatch) {
	p := r.p
	cur.Name = r.name
	if r.backendChanged {
		cur.Backend = r.backend
		// model/think are backend-specific — reset unless this same patch sets them; TTY is claude-only.
		if p.Model == nil {
			cur.ModelOverride = ""
		}
		if p.Think == nil {
			cur.ThinkOverride = ""
		}
		if r.backend != "claude" {
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
		cur.CWD = r.cwd
	}
	if p.Prompt != nil {
		cur.AppendSystemPrompt = r.prompt
	}
}

// handleSettings serves the per-session settings dialog: GET returns the view
// for ?session=<created>; POST applies a uiSettingsPatch and returns the
// refreshed view (so the dialog re-renders from the authoritative state — e.g.
// the new backend's model list after a backend switch).
func (s *uiServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	switch r.Method {
	case http.MethodGet:
		created, _ := strconv.ParseInt(r.URL.Query().Get("session"), 10, 64)
		if created <= 0 { // session=0 → the "new session" draft view (no session exists yet)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.d.uiDraftSettings(sk, s.chatID(user), r.URL.Query().Get("backend")))
			return
		}
		settings, ok := s.d.uiSessionSettings(sk, created)
		if !ok {
			http.Error(w, "Session not found", http.StatusNotFound)
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
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		if body.Session <= 0 {
			http.Error(w, "A positive session is required", http.StatusBadRequest)
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
			http.Error(w, "Session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(settings)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
