package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/sessfiles"
)

// uiBlock is one answer block (assistant narration / tool call / system note) under a
// user turn. ID is a STABLE content-derived dedup key (blockID): the same block hashes
// identically whether it arrives in a reload's transcript or a live poll event, so the
// client can drop the duplicate across the reload-read/poll race.
// EventSeq is filled by the client on live apply (= the event's seq); absent on reload.
type uiBlock struct {
	ID    string             `json:"id"`
	Role  string             `json:"role"` // assistant|tool|system
	Text  string             `json:"text,omitempty"`
	Tools []history.ToolCall `json:"tools,omitempty"`
	Kind  string             `json:"kind,omitempty"`
	Time  string             `json:"time,omitempty"` // RFC3339; a merged answer bubble shows its LAST block's time
}

// uiTurn is one row of the read model. A user row carries the durable turn seq + state
// (the per-turn indicator: enq|run|done|err) and its answer blocks; a standalone
// non-turn row has role != "user" and no seq/state.
type uiTurn struct {
	Seq       int64     `json:"seq,omitempty"` // durable turn_seq (user turns); negative synthetic for legacy markerless; 0 for standalone rows
	Role      string    `json:"role"`          // user|system|assistant|tool|notice
	Text      string    `json:"text,omitempty"`
	Time      string    `json:"time,omitempty"`
	State     string    `json:"state,omitempty"` // user turns: enq|run|done|err
	Kind      string    `json:"kind,omitempty"`  // standalone: error
	Blocks    []uiBlock `json:"blocks,omitempty"`
	CtxUsed   int       `json:"ctx_used,omitempty"`
	CtxWindow int       `json:"ctx_window,omitempty"`
}

// blockID hashes CANONICAL block content (role/text/tools) — callers must hash BEFORE any
// per-response /api/file capability-ref rewriting (refs change every render), so a block
// produced live and the same block re-read from the transcript share one id.
func blockID(seq int64, role, text string, tools []history.ToolCall) string {
	text = strings.TrimSpace(text) // history.Load trims assistant text; hash the same canonical form
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s", seq, role, text)
	for _, t := range tools {
		fmt.Fprintf(h, "\x00%s\x00%s", t.Name, t.Label)
	}
	return fmt.Sprintf("%d:%x", seq, h.Sum(nil)[:8])
}

// errBlock is the terminal block of an aborted/errored turn, so a reload shows why it
// stopped (mirrors the messenger "❌ Прервано") instead of a silently-frozen turn.
func errBlock(seq int64, reason string) uiBlock {
	switch reason {
	case "", turnErrAborted:
		reason = "Прервано"
	case turnErrAttachmentsMissing:
		reason = "Вложения недоступны, сообщение не обработано"
	case turnErrRunStartFailed:
		reason = "Не удалось зафиксировать запуск, сообщение не обработано"
	}
	return uiBlock{ID: blockID(seq, "error", reason, nil), Role: "error", Text: reason}
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

// groupTurns folds a flat transcript into top-level units: a user item starts a turn and
// the following assistant/tool items become its blocks; a system/error item (or
// an answer block with no preceding user) is a standalone unit.
func groupTurns(items []history.Item) []groupedTurn {
	var out []groupedTurn
	for _, it := range items {
		switch it.Role {
		case "user":
			out = append(out, groupedTurn{lead: it})
		case "assistant", "tool":
			if n := len(out); n > 0 && out[n-1].lead.Role == "user" {
				out[n-1].blocks = append(out[n-1].blocks, it)
				continue
			}
			out = append(out, groupedTurn{lead: it})
		default: // system | error notice
			out = append(out, groupedTurn{lead: it})
		}
	}
	return out
}

// transcriptPresence returns durable seqs whose user record is present in this
// transcript projection. It uses durable coordinates, legacy markers, and the
// same matcher as reconciliation for not-yet-bound records.
func transcriptPresence(items []history.Item, queueTurns []sessfiles.Turn) map[int64]bool {
	present := make(map[int64]bool)
	byCoord := make(map[string]sessfiles.Turn)
	byMarker := make(map[string]sessfiles.Turn)
	transcripts := make(map[string][2]string)
	var end int64
	for _, t := range queueTurns {
		if t.Bound {
			byCoord[coordinateKey(t.Backend, t.Session, t.Event)] = t
		}
		if t.Marker != "" {
			byMarker[t.Marker] = t
		}
	}
	for _, it := range items {
		if it.Event >= end {
			end = it.Event + 1
		}
		if it.Backend != "" && it.Session != "" {
			transcripts[it.Backend+"\x00"+it.Session] = [2]string{it.Backend, it.Session}
		}
		if t, ok := byCoord[coordinateKey(it.Backend, it.Session, it.Event)]; ok && t.RecordDigest == it.RecordDigest {
			present[t.Seq] = true
		}
		if it.Marker != "" {
			if t, ok := byMarker[it.Marker]; ok {
				present[t.Seq] = true
			}
		}
	}
	for _, bs := range transcripts {
		for _, p := range proposeBindings(queueTurns, items, bs[0], bs[1], end) {
			present[p.Seq] = true
		}
	}
	return present
}

// buildReadModel turns one paginated page of grouped turns into read-model rows: it joins
// each user turn to its durable coordinate binding (or legacy marker), rewrites
// text to durable text + file thumbnails, and gives every answer block its stable id.
// On the latest page (`latest`) it also appends turns still queued (enq) or just-started
// (run) that the transcript hasn't recorded yet, so a reload shows them. `startOrdinal`
// startOrdinal is retained in the internal signature for page callers; physical
// transcript event numbers mint stable native/legacy ids.
func (d *daemon) buildReadModel(sk string, created int64, page []groupedTurn, queueTurns []sessfiles.Turn, globalPresence map[int64]bool, busy bool, startOrdinal int, latest bool, ctxWindow int) []uiTurn {
	store := d.sessionStore(sk, created)
	byMarker := make(map[string]sessfiles.Turn, len(queueTurns))
	byCoord := make(map[string]sessfiles.Turn, len(queueTurns))
	for _, t := range queueTurns {
		if t.Marker != "" {
			byMarker[t.Marker] = t
		}
		if t.Bound {
			byCoord[coordinateKey(t.Backend, t.Session, t.Event)] = t
		}
	}
	// The only non-durable association allowed here is the newest active run,
	// using the exact same matcher as persistence while its bind fsync is pending.
	var pageItems []history.Item
	var pageEnd int64
	for _, g := range page {
		pageItems = append(pageItems, g.lead)
		pageItems = append(pageItems, g.blocks...)
		if g.lead.Event >= pageEnd {
			pageEnd = g.lead.Event + 1
		}
	}
	pagePresence := transcriptPresence(pageItems, queueTurns)
	if globalPresence == nil {
		globalPresence = pagePresence
	}
	newestRun := newestRunSeq(queueTurns)
	suppressNative := make(map[string]bool)
	transcripts := make(map[string][2]string)
	for _, it := range pageItems {
		if it.Backend != "" && it.Session != "" {
			transcripts[it.Backend+"\x00"+it.Session] = [2]string{it.Backend, it.Session}
		}
	}
	for _, bs := range transcripts {
		for _, p := range proposeBindings(queueTurns, pageItems, bs[0], bs[1], pageEnd) {
			if busy && p.Seq == newestRun {
				for _, t := range queueTurns {
					if t.Seq == p.Seq {
						t.Bound, t.Event, t.RecordDigest = true, p.Event, p.RecordDigest
						byCoord[coordinateKey(p.Backend, p.Session, p.Event)] = t
					}
				}
			} else if latest {
				// Idle/recovered turns are never provisionally promoted to a durable
				// positive seq. On the latest page, where its queue replacement is
				// emitted, suppress the duplicate native row. Historical pages keep
				// the transcript row because they do not append queue-only turns.
				suppressNative[coordinateKey(p.Backend, p.Session, p.Event)] = true
			}
		}
	}
	seen := make(map[int64]bool, len(page))

	turns := make([]uiTurn, 0, len(page))
	for _, g := range page {
		if g.lead.Role != "user" {
			turns = append(turns, uiTurn{Role: g.lead.Role, Text: g.lead.Text, Kind: g.lead.Kind, Time: g.lead.Time})
			continue
		}
		if suppressNative[coordinateKey(g.lead.Backend, g.lead.Session, g.lead.Event)] {
			continue
		}
		var (
			seq    = -(g.lead.Event + 1) // stable physical-record id unless a durable seq is found
			state  = "done"
			text   = g.lead.Text
			turnAt = g.lead.Time
			reason string
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
			t := matched
			seen[t.Seq] = true
			seq = t.Seq
			state = resolvedTurnState(t, busy, newestRun, g.lead.Marker != "")
			reason = t.Reason
			// The durable accept time is the ONE user-message timestamp. Before the backend transcript
			// records this turn it is already shown from queue.jsonl; switching later to the transcript's
			// slightly different timestamp changed the bubble signature and rebuilt an unchanged image.
			turnAt = time.Unix(0, t.TS).Format(time.RFC3339)
			if e := d.inboundText(store, t, sk, created); e != "" {
				text = e
			}
		}
		ut := uiTurn{Seq: seq, Role: "user", Text: text, Time: turnAt, State: state}
		// Split an assistant item's text and each tool into separate blocks, matching the
		// live progress stream (one narration block, one block per tool) so a block's id is
		// computed over the same canonical shape in both the transcript and the live event.
		// The id hashes CANONICAL raw text; the displayed text gets the same outbound
		// file-ref rewrite the live final applies, so reloaded agent files stay sealed refs.
		for _, b := range g.blocks {
			if b.Role == "assistant" {
				if b.Text != "" || len(b.Tools) == 0 {
					ut.Blocks = append(ut.Blocks, uiBlock{
						ID: blockID(seq, "assistant", b.Text, nil), Role: "assistant",
						Text: d.rewriteOutboundForUI(sk, created, b.Text), Time: b.Time,
					})
				}
				for _, tc := range b.Tools {
					ut.Blocks = append(ut.Blocks, uiBlock{ID: blockID(seq, "tool", tc.Label, nil), Role: "tool", Text: tc.Label, Time: b.Time})
				}
				// The per-turn context "cut line" comes from the last assistant block's usage —
				// including a tool-only block (a codex turn whose final token_count lands on a
				// trailing tool call), so this lives outside the text-block branch above. The
				// block's own window wins; else fall back to the session window (Claude has none).
				if b.CtxUsed > 0 {
					ut.CtxUsed = b.CtxUsed
					ut.CtxWindow = b.CtxWindow
					if ut.CtxWindow == 0 {
						ut.CtxWindow = ctxWindow
					}
				}
				continue
			}
			ut.Blocks = append(ut.Blocks, uiBlock{ID: blockID(seq, b.Role, b.Text, b.Tools), Role: b.Role, Text: b.Text, Tools: b.Tools, Kind: b.Kind, Time: b.Time})
		}
		if state == "err" {
			ut.Blocks = append(ut.Blocks, errBlock(seq, reason))
		}
		// While the turn is still RUNNING, hold back only the most-recent assistant text block:
		// the message currently being generated is represented by the working dots, not shown as
		// a settled bubble. Tool/progress blocks are already discrete events and must remain
		// visible immediately, including compaction.
		if state == "run" && len(ut.Blocks) > 0 && ut.Blocks[len(ut.Blocks)-1].Role == "assistant" {
			ut.Blocks = ut.Blocks[:len(ut.Blocks)-1]
		}
		turns = append(turns, ut)
	}

	if latest {
		var missing []uiTurn
		for _, t := range queueTurns {
			if seen[t.Seq] {
				continue
			}
			if globalPresence[t.Seq] && !pagePresence[t.Seq] {
				continue // its real transcript row lives on another page
			}
			ut := uiTurn{
				Seq: t.Seq, Role: "user", Text: d.inboundText(store, t, sk, created),
				Time: time.Unix(0, t.TS).Format(time.RFC3339), State: resolvedTurnState(t, busy, newestRun, false),
			}
			switch t.Last {
			case "enq", "run":
				missing = append(missing, ut)
			case "err": // a queued turn aborted before it ran — show it with why it stopped
				ut.State = "err"
				ut.Blocks = append(ut.Blocks, errBlock(t.Seq, t.Reason))
				missing = append(missing, ut)
			case "done":
				if !t.Bound {
					ut.State = "unknown"
					missing = append(missing, ut)
				}
			}
		}
		turns = mergeQueueOnlyTurns(turns, missing)
	}
	return turns
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
		for bi := range t.Blocks {
			if t.Seq > throughTurn || (t.Seq == throughTurn && bi > throughBlock) {
				n++
			}
		}
	}
	return n
}

// stateCode is the one-letter form of a turn's state carried in the tail cursor. It lets a pure
// state transition (enq→run when the backend is still "thinking", so no new block yet) advance the
// cursor and re-deliver the turn ONCE — edge-triggered, so the long-poll neither misses the change
// (the bubble would stay "queued" until the first block or a reload) nor spins on it.
func stateCode(state string) string {
	switch state {
	case "run":
		return "r"
	case "done":
		return "d"
	case "err":
		return "x"
	default:
		return "e" // enq / unknown
	}
}

// tailFrom returns the live "tail" of a session's read model past a per-session
// (turn,block,state,trail,head) cursor — the boundary turn (refreshed, so a grown OR state-changed
// last turn re-syncs) plus every later turn AND trailing standalone row. It returns nil when nothing
// is new (the boundary turn has not grown, its state is unchanged, no later turn exists past `head`,
// AND no standalone was appended after the last durable turn), so a long-poll keeps holding rather
// than spinning. The client replaces its own tail from `throughTurn` with these rows — ONE path,
// shared with reload, so live delivery and reload converge (no event synthesis, no in-memory ring).
// `throughState` is the boundary turn's state code the client last saw; `throughTrail` is the count
// of standalone rows it last saw trailing after the last durable turn — this is how a non-durable
// standalone appended AFTER the last turn (which has no durable position of its own) is delivered
// live exactly once instead of only on reload. ("" / 0 on a legacy cursor ⇒ re-syncs once.)
//
// `head` is the newest durable turn the client has already seen. It is normally == throughTurn, but
// when a turn is still RUNNING behind a newer QUEUED one, the boundary anchors on the running turn
// (so its later blocks + completion are delivered) while `head` stays on the newest turn — so the
// already-seen queued turn is NOT re-flagged as "new" on every poll (which would busy-loop the
// long-poll). A whole new turn is one past `head`, not past the boundary.
func tailFrom(turns []uiTurn, throughTurn int64, throughBlock int, throughState string, throughTrail int, head int64) []uiTurn {
	boundary := -1
	fresh := false
	trail := 0
	for i, t := range turns {
		if t.Role != "user" || t.Seq <= 0 {
			trail++ // a standalone / non-durable row; reset below when a later durable turn is seen
			continue
		}
		trail = 0
		if t.Seq == throughTurn {
			boundary = i
			if len(t.Blocks) > throughBlock+1 {
				fresh = true // the boundary turn grew past the read block
			}
			if stateCode(t.State) != throughState {
				fresh = true // the boundary turn changed state (e.g. enq→run, run→done) with no new block
			}
			if throughBlock >= 0 && throughBlock >= len(t.Blocks) {
				fresh = true // the boundary turn shrank below the read block — re-sync so live == reload
			}
		} else if t.Seq > head {
			fresh = true // a whole new turn (past everything the client has seen, not just the boundary)
		}
	}
	if trail != throughTrail {
		fresh = true // a standalone was appended/removed after the last durable turn
	}
	if !fresh {
		return nil
	}
	from := boundary
	if from < 0 {
		from = 0 // cursor turn gone / never set → resend from the top; the client reconciles
	}
	return turns[from:]
}
