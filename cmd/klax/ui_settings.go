package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/PiDmitrius/klax/internal/modelcatalog"
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
	KlaxID         string `json:"klax_id,omitempty"` // absent for the new-session draft
	Name           string `json:"name"`
	Backend        string `json:"backend"`
	ModelRequested string `json:"model_requested"` // "" = backend default
	Think          string `json:"think"`           // "" = backend default
	// Facts shown as "additional parameters": the model the backend last answered with (may differ
	// from the requested default) and the backend session id. Both empty until the first response.
	ModelUsed     string               `json:"model_used,omitempty"`
	BackendID     string               `json:"backend_id,omitempty"`
	Sandbox       string               `json:"sandbox"`
	TTY           bool                 `json:"tty"`
	CWD           string               `json:"cwd"`           // absolute; the UI abbreviates it with home
	SystemPrompt  string               `json:"system_prompt"` // appended to the backend's system prompt
	Busy          bool                 `json:"busy"`
	BackendLocked bool                 `json:"backend_locked"` // first message already sent
	CWDLocked     bool                 `json:"cwd_locked"`     // first message already sent
	TTYAvailable  bool                 `json:"tty_available"`  // backend == claude
	Backends      []uiSettingsOption   `json:"backends"`
	Models        []modelcatalog.Model `json:"models"`
	// Groups this session belongs to. The set of EXISTING names is deliberately not sent: the client
	// already derives it from the sessions snapshot for the scope menu, and a second derivation here
	// would order Cyrillic differently (Go lowercases and compares bytes; the browser uses
	// localeCompare), so the two surfaces would disagree.
	Groups []string `json:"groups"`
}

// uiSettingsPatch is a partial update: only non-nil fields are applied. The UI
// sends one field per request (apply-on-change), but the handler tolerates any
// combination.
type uiSettingsPatch struct {
	Name           *string `json:"name"`
	Backend        *string `json:"backend"`
	ModelRequested *string `json:"model_requested"`
	Think          *string `json:"think"`
	Sandbox        *string `json:"sandbox"`
	TTY            *bool   `json:"tty"`
	CWD            *string `json:"cwd"`
	SystemPrompt   *string `json:"system_prompt"`
	// Groups is a whole-set replacement (the settings field edits the list as one value), so an
	// empty non-nil slice clears every group.
	Groups *[]string `json:"groups"`
}

// uiErr carries an HTTP status alongside a user-facing message so the settings
// handler can surface a precise reason (busy, locked, bad value) to the dialog.
type uiErr struct {
	status int
	msg    string
}

func (e *uiErr) Error() string { return e.msg }

// settingsFail reports a rejected settings change (invalid-settings) or a failed save.
func settingsFail(w http.ResponseWriter, err error) {
	ue, ok := err.(*uiErr)
	switch {
	case ok && ue.status < http.StatusInternalServerError:
		apiFail(w, ue.status, "invalid-settings", ue.msg)
	case ok:
		apiFail(w, ue.status, "settings-save-failed", ue.msg)
	default:
		apiFail(w, http.StatusInternalServerError, "settings-save-failed", err.Error())
	}
}

// draftHasFields reports whether a new-session draft patch carries any override to
// apply after creation (an all-nil patch means "just create with defaults").
func draftHasFields(p uiSettingsPatch) bool {
	return p.Name != nil || p.Backend != nil || p.ModelRequested != nil || p.Think != nil ||
		p.Sandbox != nil || p.TTY != nil || p.CWD != nil || p.SystemPrompt != nil || p.Groups != nil
}

func validOption(entries []modelEntry, value string) bool {
	for _, e := range entries {
		if e.model == value {
			return true
		}
	}
	return false
}

// uiSessionSettings builds the settings view for one session (by klax_id).
func (d *daemon) uiSessionSettings(sk string, klaxID string) (*uiSettings, bool) {
	sess := d.store.Get(sk, klaxID)
	if sess == nil {
		return nil, false
	}
	def := d.scopeDefaults(sk)
	backend := resolveSessionBackend(sess, def, d.cfg.GetDefaultBackend())
	return &uiSettings{
		KlaxID:         sess.KlaxID,
		Name:           sess.Name,
		Backend:        backend,
		ModelRequested: sess.ModelRequested,
		Think:          sess.Think,
		ModelUsed:      sess.ModelUsed,
		BackendID:      sess.BackendID,
		Sandbox:        effectiveSandboxMode(def, sess),
		TTY:            sess.TTY,
		CWD:            sess.CWD,
		SystemPrompt:   sess.SystemPrompt,
		Busy:           d.isSessionBusy(sk, klaxID),
		BackendLocked:  sess.Messages > 0,
		CWDLocked:      sess.Messages > 0,
		TTYAvailable:   backend == "claude",
		Backends:       []uiSettingsOption{{Value: "claude", Label: "Claude"}, {Value: "codex", Label: "Codex"}},
		Models:         d.models.Models(backend),
		Groups:         sess.Groups,
	}, true
}

// uiDraftSettings builds the settings view for the "new session" draft dialog — a
// session that does not exist yet (no klax_id). It mirrors exactly what createSession
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
	model, think, tty := def.ModelRequested, def.Think, def.TTY
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
		Name:           "",
		Backend:        backend,
		ModelRequested: model,
		Think:          think,
		Sandbox:        effectiveSandboxMode(def, nil),
		TTY:            tty,
		CWD:            d.defaultSessionCWD(chatID, sk),
		TTYAvailable:   backend == "claude",
		Backends:       []uiSettingsOption{{Value: "claude", Label: "Claude"}, {Value: "codex", Label: "Codex"}},
		Models:         d.models.Models(backend),
	}
}

// applyUISessionSettings validates + applies a partial settings change to one session and PERSISTS it
// (save + broadcast). Unlike the messenger /settings handlers it edits ONLY the per-session overrides
// (never the scope defaults that seed new sessions): each UI tab is configured independently. The same
// guards apply — a run-affecting change is refused while the session is busy, and the backend and
// cwd are locked once the first message has been sent (model/think are backend-specific, so
// switching the backend resets them; cwd because a resumed run's transcript lookup is keyed by the
// process's working directory, so changing it under a live session would orphan the resume).
func (d *daemon) applyUISessionSettings(sk string, klaxID string, p uiSettingsPatch) error {
	if err := d.applyUISessionSettingsCore(sk, klaxID, p); err != nil {
		return err
	}
	d.saveStore()
	d.broadcastSessions(sk)
	return nil
}

// applyUISessionSettingsCore runs the validation + in-memory mutation but does NOT persist — the
// caller owns the save + broadcast.
func (d *daemon) applyUISessionSettingsCore(sk string, klaxID string, p uiSettingsPatch) error {
	if d.store.Get(sk, klaxID) == nil {
		return &uiErr{http.StatusNotFound, "Сессия не найдена"}
	}
	def := d.scopeDefaults(sk)
	busy := d.isSessionBusy(sk, klaxID)
	var cwd string
	if p.CWD != nil {
		var err error
		cwd, err = resolveWorkingDir(*p.CWD)
		if err != nil {
			return &uiErr{http.StatusBadRequest, err.Error()}
		}
	}
	// Resolve filesystem paths outside the lock; validate settings against the state being changed.
	var r resolvedPatch
	_, err := d.store.UpdateSessionChecked(sk, klaxID,
		func(cur *session.Session) error {
			if p.CWD != nil && cur.Messages > 0 {
				return &uiErr{http.StatusConflict, "Рабочую директорию нельзя изменить после первого сообщения."}
			}
			check := p
			check.CWD = nil
			if busy && p.CWD != nil {
				return &uiErr{http.StatusConflict, "Сессия занята — параметры запуска нельзя менять до завершения."}
			}
			backend := resolveSessionBackend(cur, def, d.cfg.GetDefaultBackend())
			var err error
			r, err = d.validateSettingsPatch(cur, backend, busy, check)
			r.p.CWD = p.CWD
			if p.CWD != nil {
				r.cwd = cwd
			}
			return err
		},
		func(cur *session.Session) { applySettingsPatch(cur, r) },
	)
	return mapSessionStoreErr(err)
}

// mapSessionStoreErr translates a session.Store sentinel error into the uiErr the HTTP
// handler expects (a bare error otherwise becomes a generic 500) — e.g. the session was
// deleted (/nuke) between the Get above and the store mutation.
func mapSessionStoreErr(err error) error {
	if err == session.ErrSessionNotFound {
		return &uiErr{http.StatusNotFound, "Сессия не найдена"}
	}
	return err
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
	groups            []string
}

// validateSettingsPatch validates `p` against `cur`'s current state (`backend` = its effective
// backend, `busy` gates run-affecting changes) and resolves the derived values. It performs the cwd
// filesystem check here so the later mutation holds no lock during I/O. It never mutates `cur`.
func (d *daemon) validateSettingsPatch(cur *session.Session, backend string, busy bool, p uiSettingsPatch) (resolvedPatch, error) {
	r := resolvedPatch{p: p, name: cur.Name, cwd: cur.CWD, prompt: cur.SystemPrompt, backend: backend}
	if p.Name != nil {
		r.name = strings.TrimSpace(*p.Name)
		if r.name == "" {
			return r, &uiErr{http.StatusBadRequest, "Имя не может быть пустым"}
		}
	}
	touchesRun := p.Backend != nil || p.ModelRequested != nil || p.Think != nil || p.Sandbox != nil || p.TTY != nil || p.CWD != nil || p.SystemPrompt != nil
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
	if p.ModelRequested != nil && *p.ModelRequested != "" && (r.backendChanged || *p.ModelRequested != cur.ModelRequested) && !validOption(d.modelsForBackend(r.backend), *p.ModelRequested) {
		return r, &uiErr{http.StatusBadRequest, "Неизвестная модель"}
	}
	model := cur.ModelRequested
	if r.backendChanged {
		model = ""
	}
	if p.ModelRequested != nil {
		model = *p.ModelRequested
	}
	efforts := d.effortsForModel(r.backend, model)
	if p.Think != nil && *p.Think != "" && (r.backendChanged || model != cur.ModelRequested || *p.Think != cur.Think) && !validOption(efforts, *p.Think) {
		return r, &uiErr{http.StatusBadRequest, "Неизвестный уровень мышления"}
	}
	if p.Think == nil && p.ModelRequested != nil && model != cur.ModelRequested && !validOption(efforts, cur.Think) {
		empty := ""
		r.p.Think = &empty
	}
	if p.Sandbox != nil && *p.Sandbox != "on" && *p.Sandbox != "off" {
		return r, &uiErr{http.StatusBadRequest, "sandbox: on или off"}
	}
	if p.TTY != nil && *p.TTY && r.backend != "claude" {
		return r, &uiErr{http.StatusBadRequest, "TTY доступен только для claude"}
	}
	if p.CWD != nil {
		if cur.Messages > 0 {
			return r, &uiErr{http.StatusConflict, "Рабочую директорию нельзя изменить после первого сообщения."}
		}
		cwd, err := resolveWorkingDir(*p.CWD)
		if err != nil {
			return r, &uiErr{http.StatusBadRequest, err.Error()}
		}
		r.cwd = cwd
	}
	if p.SystemPrompt != nil {
		r.prompt = strings.TrimSpace(*p.SystemPrompt) // empty clears the append-prompt
	}
	if p.Groups != nil {
		// A group is a view label, not a run parameter: free to change at any time, busy or not.
		groups, err := session.NormalizeGroups(*p.Groups)
		if err != nil {
			return r, &uiErr{http.StatusBadRequest, err.Error()}
		}
		r.groups = groups
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
		if p.ModelRequested == nil {
			cur.ModelRequested = ""
		}
		if p.Think == nil {
			cur.Think = ""
		}
		if r.backend != "claude" {
			cur.TTY = false
		}
	}
	if p.ModelRequested != nil {
		cur.ModelRequested = *p.ModelRequested
	}
	if p.Think != nil {
		cur.Think = *p.Think
	}
	if p.Sandbox != nil {
		cur.Sandbox = *p.Sandbox
	}
	if p.TTY != nil {
		cur.TTY = *p.TTY
	}
	if p.CWD != nil {
		cur.CWD = r.cwd
	}
	if p.SystemPrompt != nil {
		cur.SystemPrompt = r.prompt
	}
	if p.Groups != nil {
		cur.Groups = r.groups
	}
}

// handleSettings serves the per-session settings dialog: GET returns the view
// for ?klax_id=<klax_id> (no klax_id: the new-session draft); POST applies a uiSettingsPatch and returns the
// refreshed view (so the dialog re-renders from the authoritative state — e.g.
// the new backend's model list after a backend switch).
func (s *uiServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		apiFail(w, http.StatusUnauthorized, "unauthorized", "Требуется авторизация")
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	switch r.Method {
	case http.MethodGet:
		if !r.URL.Query().Has("klax_id") { // the "new session" draft view: no session exists yet
			w.Header().Set("Content-Type", "application/json")
			settings := s.d.uiDraftSettings(sk, s.chatID(user), r.URL.Query().Get("backend"))
			_ = json.NewEncoder(w).Encode(settings)
			return
		}
		settings, ok := s.d.uiSessionSettings(sk, r.URL.Query().Get("klax_id"))
		if !ok {
			writeAPIError(w, apiFailure("session-not-found"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(settings)
	case http.MethodPost:
		var body struct {
			KlaxID string `json:"klax_id"`
			uiSettingsPatch
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apiFail(w, http.StatusBadRequest, "bad-request", "Некорректный запрос")
			return
		}
		if body.KlaxID == "" {
			writeAPIError(w, apiFailure("session-not-found"))
			return
		}
		if !s.requireSession(w, sk, body.KlaxID) {
			return
		}
		if err := s.d.applyUISessionSettings(sk, body.KlaxID, body.uiSettingsPatch); err != nil {
			settingsFail(w, err)
			return
		}
		settings, ok := s.d.uiSessionSettings(sk, body.KlaxID)
		if !ok {
			writeAPIError(w, apiFailure("session-not-found"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(settings)
	default:
		apiFail(w, http.StatusMethodNotAllowed, "method-not-allowed", "Метод не поддерживается")
	}
}
