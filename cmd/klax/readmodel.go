package main

import (
	"time"

	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/sessfiles"
)

// uiBlock is one answer block (assistant narration / tool call / system note) under a
// user turn; it is addressed by its index in the turn.
type uiBlock struct {
	Role string `json:"role"` // assistant|tool|system|error
	Text string `json:"text,omitempty"`
	Kind string `json:"kind,omitempty"`
	Time string `json:"time,omitempty"` // RFC3339; a merged answer bubble shows its LAST block's time
}

// uiTurn is one row of the read model. A user row carries the durable turn seq + state
// (the per-turn indicator: enq|run|done|err) and its answer blocks; a standalone
// non-turn row has role != "user" and no seq/state.
type uiTurn struct {
	Seq       int64     `json:"turn_seq,omitempty"` // durable turn_seq (user turns); negative synthetic for legacy markerless; 0 for standalone rows
	Role      string    `json:"role"`               // user|system|assistant|tool|notice
	Text      string    `json:"text,omitempty"`
	Time      string    `json:"time,omitempty"`
	State     string    `json:"state,omitempty"` // user turns: enq|run|done|err
	Kind      string    `json:"kind,omitempty"`  // standalone: error
	Blocks    []uiBlock `json:"blocks,omitempty"`
	CtxUsed   int       `json:"ctx_used,omitempty"`
	CtxWindow int       `json:"ctx_window,omitempty"`
	event     int64     // transcript record of the row's lead; positions turn groups (ord)
	queueOnly bool      // a durable turn the transcript has not recorded
}

// errBlock is the terminal block of an aborted/errored turn, so a reload shows why it
// stopped (mirrors the messenger "❌ Прервано") instead of a silently-frozen turn. A cancelled
// turn is the user's own choice, not a failure: the same block in a neutral kind.
func errBlock(reason string) uiBlock {
	if reason == turnErrCancelled {
		return uiBlock{Role: "system", Kind: "cancelled", Text: "Отменено"}
	}
	switch reason {
	case "", turnErrAborted:
		reason = "Прервано"
	case turnErrAttachmentsMissing:
		reason = "Вложения недоступны, сообщение не обработано"
	case turnErrRunStartFailed:
		reason = "Не удалось зафиксировать запуск, сообщение не обработано"
	case turnErrAuditStartFailed:
		reason = "Ошибка инфраструктуры аудита: запрос не был запущен"
	case turnErrBackendFailed:
		reason = "Ошибка backend"
	}
	return uiBlock{Role: "error", Text: reason}
}

func appendHookWarnings(blocks []uiBlock, failures []sessfiles.HookFailure) []uiBlock {
	for _, failure := range failures {
		if failure.Hook != "audit.turn.finish" || failure.Status != "error" {
			continue
		}
		text := turnWarnAuditFinishText
		blocks = append(blocks, uiBlock{Role: "system", Text: text, Kind: "error", Time: time.Unix(0, failure.TS).Format(time.RFC3339)})
	}
	return blocks
}

// resolveTurnState maps a durable queue Last + liveness to the per-turn render state.
// A `run` record only renders `run` for the session's single newest running turn while
// the session is actually busy; an older/stale `run` (a missed MarkDone) or any `run`
// on an idle session resolves to `done`, so a dropped MarkDone can't spin forever.
func resolveTurnState(last string, busy, isNewestRun bool) string {
	switch last {
	case "enq":
		return "enq"
	case "run":
		if busy && isNewestRun {
			return "run"
		}
		return "done"
	case "err":
		return "err"
	default:
		return "done"
	}
}

func resolvedTurnState(t sessfiles.Turn, busy bool, newestRun int64, legacyMatched bool) string {
	confirmed := t.Bound || legacyMatched
	if !confirmed && !busy && (t.Last == "run" || t.Last == "done") {
		return "unknown"
	}
	return resolveTurnState(t.Last, busy, t.Seq == newestRun)
}

func newestRunSeq(turns []sessfiles.Turn) int64 {
	var m int64
	for _, t := range turns {
		if t.Last == "run" && t.Seq > m {
			m = t.Seq
		}
	}
	return m
}

// groupedTurn is one top-level unit before the read model is built: a user turn with its
// answer blocks, or a standalone notice (lead.Role != "user", no blocks). Pagination
// counts these units, so a turn with hundreds of tool blocks is still ONE page unit and
// its user message can never scroll off the top of a page.
type groupedTurn struct {
	lead   history.Item
	blocks []history.Item
}

// groupTurns folds a flat transcript into top-level units: a user item starts a turn and the
// following assistant/tool items become its blocks, as does an error row — an error happens
// inside the turn that produced it, whatever its role. A standalone system notice (or an answer
// block with no preceding user) is its own unit. A turn's blocks are the contiguous items after
// its lead, so they share the transcript's backing array instead of copying it.
func groupTurns(items []history.Item) []groupedTurn {
	var out []groupedTurn
	lead := 0
	for i, it := range items {
		switch {
		case it.Role == "user":
			out = append(out, groupedTurn{lead: it})
			lead = i
		case it.Role == "assistant", it.Role == "tool", it.Kind == "error":
			if n := len(out); n > 0 && out[n-1].lead.Role == "user" {
				out[n-1].blocks = items[lead+1 : i+1 : i+1]
				continue
			}
			out = append(out, groupedTurn{lead: it})
		default: // system notice
			out = append(out, groupedTurn{lead: it})
		}
	}
	return out
}

// buildReadModel turns a session's grouped transcript into read-model rows: it joins each user
// turn to its durable coordinate binding (or legacy marker), rewrites text to durable text + file
// thumbnails, and nests its answer blocks. It also places turns the transcript hasn't recorded
// (queued, just-started, cancelled or aborted before running) by turn_seq, so a reload shows them.
func (d *daemon) buildReadModel(sk string, klaxID string, grouped []groupedTurn, queueTurns []sessfiles.Turn, busy bool, memo *rowMemo) []uiTurn {
	store := d.sessionStore(sk, klaxID)
	byMarker := make(map[string]sessfiles.Turn, len(queueTurns))
	byCoord := make(map[string]sessfiles.Turn, len(queueTurns))
	for _, t := range queueTurns {
		if t.Marker != "" {
			byMarker[t.Marker] = t
		}
		if t.Bound {
			byCoord[coordinateKey(t.Backend, t.BackendID, t.Event)] = t
		}
	}
	// The only non-durable association allowed here is the newest active run,
	// using the exact same matcher as persistence while its bind fsync is pending.
	leads := make([]history.Item, 0, len(grouped)) // presence and binding read only user leads
	var end int64
	for _, g := range grouped {
		leads = append(leads, g.lead)
		if g.lead.Event >= end {
			end = g.lead.Event + 1
		}
	}
	newestRun := newestRunSeq(queueTurns)
	suppressNative := make(map[string]bool)
	transcripts := make(map[string][2]string)
	for _, it := range leads {
		if it.Backend != "" && it.Session != "" {
			transcripts[it.Backend+"\x00"+it.Session] = [2]string{it.Backend, it.Session}
		}
	}
	for _, bs := range transcripts {
		for _, p := range proposeBindings(queueTurns, leads, bs[0], bs[1], end) {
			if busy && p.Seq == newestRun {
				for _, t := range queueTurns {
					if t.Seq == p.Seq {
						t.Bound, t.Event, t.RecordDigest = true, p.Event, p.RecordDigest
						byCoord[coordinateKey(p.Backend, p.Session, p.Event)] = t
					}
				}
			} else {
				// Idle/recovered turns are never provisionally promoted to a durable positive
				// seq: the queue row stands in for them, so the duplicate native row is dropped.
				suppressNative[coordinateKey(p.Backend, p.Session, p.Event)] = true
			}
		}
	}
	seen := make(map[int64]bool, len(grouped))

	turns := make([]uiTurn, 0, len(grouped))
	for _, g := range grouped {
		if g.lead.Role != "user" {
			turns = append(turns, uiTurn{Role: g.lead.Role, Text: g.lead.Text, Kind: g.lead.Kind, Time: g.lead.Time, event: g.lead.Event})
			continue
		}
		if suppressNative[coordinateKey(g.lead.Backend, g.lead.Session, g.lead.Event)] {
			continue
		}
		var (
			seq    = -(g.lead.Event + 1) // stable physical-record id unless a durable seq is found
			state  = "done"
			reason string
			hooks  int
		)
		var matched sessfiles.Turn
		var ok bool
		if t, found := byCoord[coordinateKey(g.lead.Backend, g.lead.Session, g.lead.Event)]; found && t.RecordDigest == g.lead.RecordDigest {
			matched, ok = t, true
		}
		if !ok && g.lead.Marker != "" {
			matched, ok = byMarker[g.lead.Marker]
		}
		if ok {
			seen[matched.Seq] = true
			seq = matched.Seq
			state = resolvedTurnState(matched, busy, newestRun, g.lead.Marker != "")
			reason = matched.Reason
			hooks = len(matched.HookFailures)
		}
		key := rowKey{event: g.lead.Event, seq: seq, blocks: len(g.blocks), state: state, reason: reason, hooks: hooks, turnWindow: matched.CtxWindow}
		if n := len(g.blocks); n > 0 {
			last := g.blocks[n-1]
			key.lastCtx, key.lastCtxWindow, key.lastTime = last.CtxUsed, last.CtxWindow, last.Time
		}
		ut, keep := memo.get(key)
		if !keep {
			ut, keep = d.userRow(store, sk, klaxID, g, matched, ok, seq, state, reason)
		}
		if keep {
			memo.put(key, ut)
		} else {
			memo.degrade()
		}
		ut.event = g.lead.Event
		turns = append(turns, ut)
	}

	var missing []uiTurn
	for _, t := range queueTurns {
		if seen[t.Seq] {
			continue
		}
		text, published := d.inboundText(store, t, sk, klaxID)
		if !published {
			memo.degrade()
		}
		ut := uiTurn{
			Seq: t.Seq, Role: "user", Text: text, queueOnly: true,
			Time: time.Unix(0, t.TS).Format(time.RFC3339), State: resolvedTurnState(t, busy, newestRun, false),
		}
		switch t.Last {
		case "enq", "run":
			missing = append(missing, ut)
		case "err": // a queued turn aborted before it ran — show it with why it stopped
			ut.Blocks = append(ut.Blocks, errBlock(t.Reason))
			missing = append(missing, ut)
		case "done":
			if !t.Bound {
				ut.Blocks = appendHookWarnings(ut.Blocks, t.HookFailures)
				missing = append(missing, ut)
			}
		}
	}
	return mergeQueueOnlyTurns(turns, missing)
}

// userRow builds one user turn's row: durable text and time, answer blocks, and
// the klax-side error and hook-warning blocks. keep is false when an attachment or a local file
// link could not be published yet, which a later build may still do.
func (d *daemon) userRow(store *sessfiles.Store, sk string, klaxID string, g groupedTurn, matched sessfiles.Turn, ok bool, seq int64, state, reason string) (ut uiTurn, keep bool) {
	keep = true
	text, turnAt := g.lead.Text, g.lead.Time
	if ok {
		// The durable accept time is the ONE user-message timestamp. Before the backend transcript
		// records this turn it is already shown from queue.jsonl; switching later to the transcript's
		// slightly different timestamp changed the bubble signature and rebuilt an unchanged image.
		turnAt = time.Unix(0, matched.TS).Format(time.RFC3339)
		e, published := d.inboundText(store, matched, sk, klaxID)
		keep = keep && published
		if e != "" {
			text = e
		}
	}
	ut = uiTurn{Seq: seq, Role: "user", Text: text, Time: turnAt, State: state}
	// Split an assistant item's text and each tool into separate blocks: one narration block,
	// one block per tool.
	// The displayed text gets the same outbound file-ref rewrite the live final applies, so
	// reloaded agent files stay sealed refs.
	for _, b := range g.blocks {
		if b.Role == "assistant" {
			if b.Text != "" || len(b.Tools) == 0 {
				text, published := d.rewriteOutboundForUI(sk, klaxID, seq, b.Text)
				keep = keep && published
				ut.Blocks = append(ut.Blocks, uiBlock{Role: "assistant", Text: text, Time: b.Time})
			}
			for _, tc := range b.Tools {
				ut.Blocks = append(ut.Blocks, uiBlock{Role: "tool", Text: tc.Label, Time: b.Time})
			}
			// The per-turn context "cut line" comes from the last assistant block's usage —
			// including a tool-only block (a codex turn whose final token_count lands on a
			// trailing tool call), so this lives outside the text-block branch above. The
			// block's own window wins; else the window the turn completed with (Claude's
			// transcript has none). A turn without either shows used tokens only.
			if b.CtxUsed > 0 {
				ut.CtxUsed = b.CtxUsed
				ut.CtxWindow = b.CtxWindow
				if ut.CtxWindow == 0 && ok {
					ut.CtxWindow = matched.CtxWindow
				}
			}
			continue
		}
		ut.Blocks = append(ut.Blocks, uiBlock{Role: b.Role, Text: b.Text, Kind: b.Kind, Time: b.Time})
	}
	if state == "err" && !explainedByTranscript(reason, g.blocks) {
		ut.Blocks = append(ut.Blocks, errBlock(reason))
	}
	if ok {
		ut.Blocks = appendHookWarnings(ut.Blocks, matched.HookFailures)
	}
	// While the turn is still RUNNING, hold back only the most-recent assistant text block:
	// the message currently being generated is represented by the working dots, not shown as
	// a settled bubble. Tool/progress blocks are already discrete events and must remain
	// visible immediately, including compaction.
	if state == "run" && len(ut.Blocks) > 0 && ut.Blocks[len(ut.Blocks)-1].Role == "assistant" {
		ut.Blocks = ut.Blocks[:len(ut.Blocks)-1]
	}
	return ut, keep
}

// rowMemo carries a session's user rows from one read-model build to the next, within one
// transcript index generation. A row is a pure function of its rowKey — transcript blocks are
// append-only apart from the last one's usage and time, and the durable text, time and published
// file links of a seq never change — so a transcript append rebuilds only the turns it touched.
// A row whose file link degraded is not kept, and marks the build degraded. A nil memo builds
// every row.
type rowMemo struct {
	prev, next map[rowKey]uiTurn
	degraded   bool
}

type rowKey struct {
	event, seq             int64
	blocks                 int
	lastCtx, lastCtxWindow int
	lastTime               string
	state, reason          string
	hooks, turnWindow      int
}

func (m *rowMemo) get(k rowKey) (uiTurn, bool) {
	if m == nil {
		return uiTurn{}, false
	}
	r, ok := m.prev[k]
	return r, ok
}

func (m *rowMemo) degrade() {
	if m != nil {
		m.degraded = true
	}
}

func (m *rowMemo) put(k rowKey, r uiTurn) {
	if m == nil {
		return
	}
	if m.next == nil {
		m.next = make(map[rowKey]uiTurn)
	}
	m.next[k] = r
}

// explainedByTranscript reports whether the turn already ends with the backend's own account of
// why it stopped. Only backend-failed defers to it: every other reason is klax's own
// classification of a turn the backend never got to fail, and an error row that the turn
// recovered from is not its outcome. It judges the turn's transcript rows, so a klax-side block
// appended later — a hook warning carries the same kind — can never stand in for the backend.
func explainedByTranscript(reason string, blocks []history.Item) bool {
	if reason != turnErrBackendFailed || len(blocks) == 0 {
		return false
	}
	return blocks[len(blocks)-1].Kind == "error"
}

func mergeQueueOnlyTurns(base, missing []uiTurn) []uiTurn {
	if len(missing) == 0 {
		return base
	}
	out := make([]uiTurn, 0, len(base)+len(missing))
	mi := 0
	for _, row := range base {
		if row.Role == "user" && row.Seq > 0 {
			for mi < len(missing) && missing[mi].Seq < row.Seq {
				out = append(out, missing[mi])
				mi++
			}
		}
		out = append(out, row)
	}
	return append(out, missing[mi:]...)
}

// unreadAfter counts unread answer blocks (the "actions" of a turn — narration + tool calls)
// relative to a durable read watermark (turn_seq, block index) — the count the tab badge shows. A
// user turn's block at index bi is unread when (turn.Seq, bi) sorts strictly after (throughTurn,
// throughBlock); a never-read session is (0,0) ⇒ every block unread (uniform for UI- and
// messenger-originated sessions). Only answer blocks count — the user's own bubbles do not, and
// standalone non-durable rows (Seq==0) are skipped, so the count is >0 exactly when a
// divider would show (badge↔line invariant) and a trailing notice can never wedge the badge >0.
func unreadAfter(turns []uiTurn, throughTurn int64, throughBlock int) int {
	n := 0
	for _, t := range turns {
		if t.Role != "user" || t.Seq <= 0 {
			continue
		}
		for bi, b := range t.Blocks {
			if b.Kind == "cancelled" {
				continue // the user's own action, possibly past a still-running turn — never unread
			}
			if t.Seq > throughTurn || (t.Seq == throughTurn && bi > throughBlock) {
				n++
			}
		}
	}
	return n
}
