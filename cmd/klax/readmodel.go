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
// client can drop the duplicate across the reload-read/poll race (REFACTOR_PLAN §A3).
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
// (the per-turn indicator: enq|run|done|err) and its answer blocks; a standalone row
// (system/compact/error notice between turns) has role != "user" and no seq/state.
type uiTurn struct {
	Seq    int64     `json:"seq,omitempty"` // durable turn_seq (user turns); negative synthetic for legacy markerless; 0 for standalone rows
	Role   string    `json:"role"`          // user|system|compact|assistant|tool
	Text   string    `json:"text,omitempty"`
	Time   string    `json:"time,omitempty"`
	State  string    `json:"state,omitempty"` // user turns: enq|run|done|err
	Kind   string    `json:"kind,omitempty"`  // standalone: compact|error
	Blocks []uiBlock `json:"blocks,omitempty"`
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
		reason = "прервано"
	case turnErrAttachmentsMissing:
		reason = "вложения недоступны, сообщение не обработано"
	case turnErrRunStartFailed:
		reason = "не удалось зафиксировать запуск, сообщение не обработано"
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
// the following assistant/tool items become its blocks; a system/compact/error item (or
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
		default: // system | compact | error notice
			out = append(out, groupedTurn{lead: it})
		}
	}
	return out
}

// buildReadModel turns one paginated page of grouped turns into read-model rows: it joins
// each user turn to its durable queue record by marker (assigning seq + state), rewrites
// text to durable text + file thumbnails, and gives every answer block its stable id.
// On the latest page (`latest`) it also appends turns still queued (enq) or just-started
// (run) that the transcript hasn't recorded yet, so a reload shows them. `startOrdinal`
// is the absolute index of the first page unit, used to mint stable legacy ids.
func (d *daemon) buildReadModel(sk string, created int64, page []groupedTurn, queueTurns []sessfiles.Turn, busy bool, startOrdinal int, latest bool) []uiTurn {
	store := d.sessionStore(sk, created)
	byMarker := make(map[string]sessfiles.Turn, len(queueTurns))
	for _, t := range queueTurns {
		if t.Marker != "" {
			byMarker[t.Marker] = t
		}
	}
	newestRun := newestRunSeq(queueTurns)
	seen := make(map[string]bool, len(page))

	turns := make([]uiTurn, 0, len(page))
	for i, g := range page {
		if g.lead.Role != "user" {
			turns = append(turns, uiTurn{Role: g.lead.Role, Text: g.lead.Text, Kind: g.lead.Kind, Time: g.lead.Time})
			continue
		}
		var (
			seq    = -int64(startOrdinal + i + 1) // stable absolute legacy id unless a durable seq is found
			state  = "done"
			text   = g.lead.Text
			reason string
		)
		if m := g.lead.Marker; m != "" {
			seen[m] = true
			if t, ok := byMarker[m]; ok {
				seq = t.Seq
				state = resolveTurnState(t.Last, busy, t.Seq == newestRun)
				reason = t.Reason
				if e := d.inboundText(store, t, sk, created); e != "" {
					text = e
				}
			}
		}
		ut := uiTurn{Seq: seq, Role: "user", Text: text, Time: g.lead.Time, State: state}
		// Split an assistant item's text and each tool into separate blocks, matching the
		// live progress stream (one narration block, one block per tool) so a block's id is
		// computed over the same canonical shape in both the transcript and the live event.
		// The id hashes CANONICAL raw text; the displayed text gets the same outbound
		// file-ref rewrite the live final applies, so reloaded agent files stay sealed refs.
		for _, b := range g.blocks {
			if b.Role == "assistant" {
				if b.Text != "" || len(b.Tools) == 0 {
					ut.Blocks = append(ut.Blocks, uiBlock{ID: blockID(seq, "assistant", b.Text, nil), Role: "assistant", Text: d.rewriteOutboundForUI(sk, created, b.Text), Time: b.Time})
				}
				for _, tc := range b.Tools {
					ut.Blocks = append(ut.Blocks, uiBlock{ID: blockID(seq, "tool", tc.Label, nil), Role: "tool", Text: tc.Label, Time: b.Time})
				}
				continue
			}
			ut.Blocks = append(ut.Blocks, uiBlock{ID: blockID(seq, b.Role, b.Text, b.Tools), Role: b.Role, Text: b.Text, Tools: b.Tools, Kind: b.Kind, Time: b.Time})
		}
		if state == "err" {
			ut.Blocks = append(ut.Blocks, errBlock(seq, reason))
		}
		turns = append(turns, ut)
	}

	if latest {
		var missing []uiTurn
		for _, t := range queueTurns {
			if t.Marker == "" || seen[t.Marker] {
				continue
			}
			ut := uiTurn{
				Seq: t.Seq, Role: "user", Text: d.inboundText(store, t, sk, created),
				Time: time.Unix(0, t.TS).Format(time.RFC3339), State: resolveTurnState(t.Last, busy, t.Seq == newestRun),
			}
			switch t.Last {
			case "enq", "run":
				missing = append(missing, ut)
			case "err": // a queued turn aborted before it ran — show it with why it stopped
				ut.State = "err"
				ut.Blocks = append(ut.Blocks, errBlock(t.Seq, t.Reason))
				missing = append(missing, ut)
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
