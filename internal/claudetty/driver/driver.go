// Package driver spawns the interactive `claude` TUI under a PTY, types the
// prompt, waits for the Stop hook, and re-emits the turn as
// `claude -p --output-format stream-json` wire format on the given writer.
//
// The PTY path exists so that `claude` itself classifies the session as
// interactive (entrypoint=cli) — nothing here forces or spoofs that label.
package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/PiDmitrius/klax/internal/claudetty/hook"
	"github.com/PiDmitrius/klax/internal/claudetty/pty"
	"github.com/PiDmitrius/klax/internal/claudetty/stream"
	"github.com/PiDmitrius/klax/internal/claudetty/term"
	"github.com/PiDmitrius/klax/internal/claudetty/transcript"
)

// Options is the claude flag subset klax actually uses, plus test hooks.
type Options struct {
	Prompt             string
	Model              string
	Effort             string
	PermissionMode     string // e.g. "bypassPermissions"
	AppendSystemPrompt string
	DisallowedTools    string
	Resume             string // session id to resume
	Cwd                string

	ClaudePath string // override binary (testing); default "claude"
	// Timeout caps the whole turn. Zero means no cap — matching klax's
	// claude -p path, which waits for process exit however long the turn
	// runs. Submission failures are still detected via the stall guard.
	Timeout    time.Duration
	Rows, Cols uint16
	Debug      io.Writer // nil = no tracing
}

// Ink timing constants, tuned in claude-p against the real TUI.
const (
	inkQuiescence    = 80 * time.Millisecond  // output silence = render done
	inkMaxWait       = 2 * time.Second        // give up waiting and type anyway
	inkEnterDebounce = 120 * time.Millisecond // gap so Enter is a second event
	postStopDrain    = 20                     // post-Stop pump rounds...
	postStopTick     = 20 * time.Millisecond  // ...at this interval
	// postEchoIdle caps a turn whose prompt was accepted but then went silent
	// — a stuck network retry that writes nothing and never exits. It keys on
	// PTY output, not the transcript: a long legit tool call (a 30-min build)
	// produces no transcript lines but Ink keeps repainting its elapsed-time
	// indicator ~1/s, so the PTY is never silent this long while claude lives.
	postEchoIdle = 120 * time.Second
	// fifoReadTimeout bounds each hook-FIFO read so a writer that holds the
	// pipe open without sending data can't park the loop goroutine (Go's
	// poller blocks on a connected-but-silent O_NONBLOCK FIFO) and stall the
	// stall/idle/exit guards that live further down the loop body.
	fifoReadTimeout = 20 * time.Millisecond
	// rearmMinWait is the minimum age of a submitted-but-unechoed prompt
	// before a fresh SessionStart may re-arm it. Auto-compact's second
	// SessionStart lands tens of seconds after we type, so this never blocks
	// the legitimate case; it only rules out re-typing in the millisecond
	// window where the echo is written but not yet pumped.
	rearmMinWait = 2 * time.Second
)

// Run executes one turn and writes stream-json lines to w. Returns the
// process exit code to use. Cancelling ctx (klax /abort sends SIGTERM to the
// wrapper, which the entrypoint turns into a cancel) ends the turn promptly so
// the deferred teardown reaps claude's process group and the temp dir — claude
// runs in its own Setsid session, out of reach of the wrapper's group SIGTERM,
// so this return is what guarantees it dies on abort.
func Run(ctx context.Context, w io.Writer, opts Options) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Prompt == "" {
		return 2, fmt.Errorf("empty prompt")
	}
	if opts.Rows == 0 {
		opts.Rows = 40
	}
	if opts.Cols == 0 {
		opts.Cols = 120
	}
	start := time.Now()
	trace := func(format string, args ...any) {
		if opts.Debug != nil {
			fmt.Fprintf(opts.Debug, "[claudetty +%dms] %s\n",
				time.Since(start).Milliseconds(), fmt.Sprintf(format, args...))
		}
	}

	h, err := hook.New()
	if err != nil {
		return 1, err
	}
	defer h.Close()
	trace("hook harness ready (FIFO + relay script)")

	// Open the FIFO read side BEFORE spawning so the child's hook never
	// blocks opening the write side.
	fifo, err := os.OpenFile(h.FifoPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 1, fmt.Errorf("open fifo: %w", err)
	}
	defer fifo.Close()

	pt, err := pty.Open(opts.Rows, opts.Cols)
	if err != nil {
		return 1, err
	}
	defer pt.Close()

	bin := opts.ClaudePath
	if bin == "" {
		bin = "claude"
	}
	argv := buildArgv(h.SettingsJSON, opts)
	cmd := exec.Command(bin, argv...)
	cmd.Dir = opts.Cwd
	cmd.Stdin = pt.Slave
	cmd.Stdout = pt.Slave
	cmd.Stderr = pt.Slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true, // new session; the slave becomes the controlling tty
		Setctty: true,
	}
	cmd.Env = childEnv(h.FifoPath)
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("spawn %s: %w", bin, err)
	}
	trace("claude spawned (pid %d); Ink booting", cmd.Process.Pid)
	pt.CloseSlave()

	var (
		lastOutputNS atomic.Int64
		exited       atomic.Bool
		recentMu     sync.Mutex
		recent       []byte
	)
	const recentCap = 8192

	// Reader: pump PTY output, answer DEC queries, keep the rolling buffer.
	// Capture the master handle locally: the deferred pt.Close() nil-s the
	// struct field, and reading it from this never-joined goroutine would be
	// a data race. Closing the fd still unblocks the Read below.
	master := pt.Master
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				lastOutputNS.Store(time.Now().UnixNano())
				if resp := term.RespondToDecQueries(buf[:n], opts.Rows, opts.Cols); len(resp) > 0 {
					master.Write(resp)
				}
				recentMu.Lock()
				recent = append(recent, buf[:n]...)
				if len(recent) > recentCap {
					recent = recent[len(recent)-recentCap:]
				}
				recentMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		cmd.Wait()
		exited.Store(true)
	}()
	// Ensure the child never outlives us, whatever the exit path.
	defer func() {
		if !exited.Load() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			for i := 0; i < 100 && !exited.Load(); i++ {
				time.Sleep(20 * time.Millisecond)
			}
			if !exited.Load() {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}()

	em := stream.NewEmitter(w)
	var (
		fifoBuf        []byte
		fifoReadBuf    = make([]byte, 4096)
		transcriptPath string
		stopPayload    string
		promptSent     bool
		promptSentAt   time.Time
		enterRetries   int
		rearms         int
		promptEchoSeen bool
		lastActivity   = start
		trustDismissed bool
		tailer         *transcript.Tailer
		summary        transcript.Summary
	)
	// A successfully submitted prompt is echoed back as a `user` transcript
	// line; its presence is the proof of submission the retry/re-arm logic
	// keys on.
	needle := promptNeedle(opts.Prompt)
	defer func() {
		if tailer != nil {
			tailer.Close()
		}
	}()
	summary.Model = opts.Model
	summary.ContextWindow = contextWindowForModel(opts.Model)

	// emitReady reports whether transcript lines now belong to this turn and
	// may be summarized/emitted. On a resume the file still holds the prior
	// session's history, so we must wait for THIS prompt's echo to step past
	// it. On a fresh session there is nothing to skip, so output is safe as
	// soon as the prompt is typed — which also keeps a prompt-echo
	// false-negative (an escaping divergence in the needle) from silently
	// swallowing an entire fresh-session turn. Echo detection is a heuristic;
	// never make it load-bearing when there is no history to protect against.
	emitReady := func() bool {
		return promptEchoSeen || (opts.Resume == "" && promptSent)
	}

	pumpTranscript := func() {
		if tailer == nil {
			return
		}
		for _, raw := range tailer.Pump() {
			lastActivity = time.Now()
			l, ok := transcript.Parse(raw)
			if !ok {
				continue
			}
			// Confirm submission when this turn's echo appears: it drives the
			// retry/re-arm/stall/idle guards and freezes the tailer (so a
			// post-echo compaction can't reset the offset and replay already-
			// emitted lines). On a resume the needle is what distinguishes our
			// prompt from the resumed history. On a fresh session there is no
			// history, so the first user line after typing IS our prompt —
			// confirm on it regardless of the needle, otherwise a needle
			// false-negative would leave those guards keyed on a never-set
			// promptEchoSeen (stray Enter resends, and a 3-min-silent turn would
			// stamp "never accepted" over an answer already streamed out).
			if !promptEchoSeen && l.Type == "user" && (needleMatch(needle, l.Raw) || opts.Resume == "") {
				promptEchoSeen = true
				tailer.Freeze()
				trace("prompt echo seen in transcript — submission confirmed")
			}
			if !emitReady() {
				// Still skipping resumed history. One thing must not wait for
				// the echo: an API error is itself the turn's outcome, so
				// surface it (Summary.Add sets IsError + FinalText) rather than
				// discard it and hang until the stall guard.
				if l.IsAPIError {
					summary.Add(l)
				}
				continue
			}
			summary.Add(l)
			em.Line(l, &summary)
		}
	}

	for {
		if ctx.Err() != nil {
			// /abort (or session teardown) cancelled us. End the turn so the
			// deferred claude-group SIGTERM/SIGKILL and temp-dir cleanup run;
			// the wrapper's own SIGTERM would otherwise skip every defer.
			summary.IsError = true
			summary.FinalText = "turn aborted"
			trace("context cancelled — ending turn for teardown")
			break
		}
		if opts.Timeout > 0 && time.Since(start) > opts.Timeout {
			summary.IsError = true
			summary.FinalText = fmt.Sprintf("timeout after %s (prompt sent: %v)", opts.Timeout, promptSent)
			trace("turn timeout after %s", opts.Timeout)
			break
		}
		if exited.Load() && !promptSent {
			summary.IsError = true
			summary.FinalText = "claude exited before becoming ready"
			trace("claude exited before becoming ready")
			break
		}

		// Refresh transcript-derived state (promptEchoSeen, summary) BEFORE
		// acting on FIFO events, so the re-arm / Stop decisions below read the
		// current echo status rather than a stale one (a SessionStart buffered
		// alongside the echo could otherwise re-type an accepted prompt).
		if tailer == nil && transcriptPath != "" {
			if t, err := transcript.OpenTailer(transcriptPath); err == nil {
				tailer = t
				trace("transcript opened for tailing: %s", transcriptPath)
			}
		}
		pumpTranscript()

		// Workspace-trust dialog: shown in unfamiliar directories before
		// SessionStart hooks register; not bypassed by permission flags.
		// Default selection is "Yes, I trust this folder"; Enter accepts.
		if !trustDismissed && !promptSent {
			recentMu.Lock()
			stripped := term.StripEscapes(recent)
			recentMu.Unlock()
			if strings.Contains(string(stripped), "trust") && strings.Contains(string(stripped), "folder") {
				trace("workspace-trust dialog detected — sending Enter")
				pt.Master.Write([]byte("\r"))
				trustDismissed = true
			}
		}

		// Drain the hook FIFO. Bound the read so a hook holding the write end
		// open without data can't park this goroutine and wedge the whole loop
		// (and the guards below it); the deadline makes the read self-unblock.
		fifo.SetReadDeadline(time.Now().Add(fifoReadTimeout))
		n, err := fifo.Read(fifoReadBuf)
		if n > 0 {
			fifoBuf = append(fifoBuf, fifoReadBuf[:n]...)
		}
		_ = err // EAGAIN, EOF, and deadline-exceeded all mean "nothing now"
		for {
			nl := bytes.IndexByte(fifoBuf, '\n')
			if nl < 0 {
				break
			}
			line := string(fifoBuf[:nl])
			fifoBuf = fifoBuf[nl+1:]
			ev, ok := hook.ParseLine(line)
			if !ok {
				continue
			}
			sid, tp, _ := hook.ExtractFields(ev.Payload)
			trace("hook %s fired (session %s)", ev.Name, sid)
			// Until our prompt is accepted, follow the newest transcript path:
			// auto-compact can fire a second SessionStart pointing at a freshly
			// compacted file. Once the echo is in we are committed to the file
			// carrying this turn's output, so stop chasing the path then (H2).
			if !promptEchoSeen && tp != "" && tp != transcriptPath {
				if tailer != nil {
					tailer.Close()
					tailer = nil
				}
				transcriptPath = tp
				trace("transcript path set to %s", tp)
			}
			// First non-empty session id wins and is what klax binds to. A
			// resume keeps the same id by definition, and auto-compact's second
			// SessionStart stays within that same session, so the id is stable
			// across the pre-echo window — we deliberately don't chase a
			// changed id here (only a changed transcript path, above).
			if sid != "" && summary.SessionID == "" {
				summary.SessionID = sid
			}
			switch ev.Name {
			case "SessionStart":
				if promptSent && !promptEchoSeen && rearms < 2 && time.Since(promptSentAt) > rearmMinWait {
					// Ink restarted before accepting our input — auto-compact
					// on resume of a near-full session rewrites the UI and
					// drops anything typed before it. Re-arm and retype. The
					// age guard plus the pump above keep a just-accepted prompt
					// (echo written but not yet pumped) from being re-typed.
					rearms++
					promptSent = false
					enterRetries = 0
					trace("SessionStart again before prompt echo — re-arming (rearm %d)", rearms)
				}
				if !promptSent {
					waitQuiescent(&lastOutputNS, trace)
					// Fast-forward past a resumed session's history before
					// typing: open the tailer now (on --resume the file
					// already exists) and discard its current content.
					if tailer == nil && transcriptPath != "" {
						if t, err := transcript.OpenTailer(transcriptPath); err == nil {
							tailer = t
							trace("transcript opened for tailing: %s", transcriptPath)
						}
					}
					if tailer != nil {
						if skipped := len(tailer.Pump()); skipped > 0 {
							trace("skipped %d pre-turn transcript line(s) (resume history)", skipped)
						}
					}
					// klax must learn the session id before any content.
					em.Init(&summary)
					trace("typing prompt (%d bytes)", len(opts.Prompt))
					// Prompt body and Enter as two events with a gap —
					// Ink's burst heuristic otherwise swallows the \r
					// into the input buffer instead of submitting.
					pt.Master.Write([]byte(opts.Prompt))
					time.Sleep(inkEnterDebounce)
					pt.Master.Write([]byte("\r"))
					trace("prompt + Enter sent; waiting on claude")
					promptSent = true
					promptSentAt = time.Now()
				}
			case "Stop":
				// Honor a Stop only once this turn's output may be emitted (echo
				// seen on a resume; prompt typed on a fresh session). A Stop
				// arriving before that is resume/compaction noise, not our turn —
				// recording it would end the turn on an empty summary and emit a
				// bogus is_error=false success (M1 / empty-success regression).
				if emitReady() {
					stopPayload = ev.Payload
				} else {
					trace("Stop before turn ready — ignoring")
				}
			}
		}

		if summary.IsError && summary.FinalText != "" {
			trace("API-error transcript line received; ending turn without Stop hook")
			break
		}

		// A submitted prompt is echoed as a user transcript line within
		// moments; no echo means Ink swallowed the Enter (huge resumed
		// sessions keep rendering past the quiescence cap). Retype it,
		// bounded.
		if promptSent && !promptEchoSeen && stopPayload == "" && enterRetries < 3 &&
			time.Since(promptSentAt) > 5*time.Second*time.Duration(enterRetries+1) {
			enterRetries++
			trace("no prompt echo %.0fs after submit — resending Enter (retry %d)",
				time.Since(promptSentAt).Seconds(), enterRetries)
			pt.Master.Write([]byte("\r"))
		}

		// Stall guard, replacing the old blanket 10-minute cap: only a turn
		// that never got submitted is hung; a submitted turn may legally run
		// for an hour. Quiet transcript + exhausted retries + no echo = the
		// UI is not going to take our input.
		if promptSent && !promptEchoSeen && stopPayload == "" && enterRetries >= 3 &&
			time.Since(promptSentAt) > 3*time.Minute && time.Since(lastActivity) > 3*time.Minute {
			summary.IsError = true
			summary.FinalText = fmt.Sprintf("prompt was typed but never accepted (no transcript echo after %s)",
				time.Since(promptSentAt).Round(time.Second))
			trace("stall guard: prompt never accepted")
			break
		}

		// Post-echo idle cap: the prompt was accepted but the PTY has gone
		// silent — a stuck network retry that writes nothing and never exits.
		// Keyed on PTY output, not transcript activity, so a long quiet tool
		// call (which keeps Ink's timer repainting) is left alone.
		if promptEchoSeen && stopPayload == "" {
			if last := lastOutputNS.Load(); last != 0 &&
				time.Since(time.Unix(0, last)) > postEchoIdle {
				if summary.FinalText == "" {
					// Silent with no answer: a genuine hang.
					summary.IsError = true
					summary.FinalText = "claude stopped producing output (likely a stuck network retry)"
					trace("post-echo idle cap: no PTY output for %s, no answer — failing", postEchoIdle)
				} else {
					// We already have the assistant's answer; the Stop hook was
					// just lost. Emit what we have rather than clobber it.
					trace("post-echo idle cap: no PTY output for %s; emitting buffered answer", postEchoIdle)
				}
				break
			}
		}

		// claude dying mid-turn would otherwise leave us waiting forever for
		// a Stop hook that cannot come.
		if exited.Load() && stopPayload == "" {
			if !(summary.IsError && summary.FinalText != "") {
				summary.IsError = true
				if summary.FinalText == "" {
					summary.FinalText = "claude exited unexpectedly before finishing the turn"
				}
			}
			trace("claude exited without Stop hook; ending turn")
			break
		}

		if stopPayload != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stopPayload != "" {
		trace("Stop hook fired; draining transcript")
	} else {
		trace("turn ended without Stop hook (%s); draining transcript", terminationReason(&summary))
	}

	// The Stop hook can fire before claude flushes the final transcript
	// lines. Drain for a bounded window — but skip it on abort, where the turn
	// is discarded and every millisecond spent here delays claude's teardown
	// against the runner's 3s SIGKILL deadline.
	if ctx.Err() == nil {
		for i := 0; i < postStopDrain; i++ {
			if tailer == nil && transcriptPath != "" {
				if t, err := transcript.OpenTailer(transcriptPath); err == nil {
					tailer = t
				}
			}
			pumpTranscript()
			time.Sleep(postStopTick)
		}
	}

	// Fall back to the Stop payload's last_assistant_message — but only once
	// the turn is emit-ready, so we never surface a resumed session's previous
	// answer (pre-echo) as this turn's result.
	if summary.FinalText == "" && emitReady() {
		if _, _, last := hook.ExtractFields(stopPayload); last != "" {
			summary.FinalText = last
		}
	}

	// Every exit path funnels here so the consumer always gets a terminal
	// result envelope. Init self-guards: idempotent, and a no-op while the
	// session id is unknown — a turn that died before any SessionStart
	// (claude crashed at startup) emits no bogus empty-id init. A terminal
	// result with no preceding init is fine for that error case.
	em.Init(&summary)
	em.Result(&summary, time.Since(start))
	trace("result envelope emitted")
	if summary.IsError {
		return 1, nil
	}
	return 0, nil
}

// terminationReason describes a Stop-less exit for tracing.
func terminationReason(s *transcript.Summary) string {
	if s.IsError {
		return "error"
	}
	return "completed"
}

// contextWindowForModel estimates the context window from the --model value
// klax launched claude with. claude -p computes the authoritative number from
// its internal model table, but the interactive CLI has no surface to report
// it and the transcript never carries it — without an estimate the tty path
// would report 0 and klax's context gauge would never update. The rule
// mirrors claude's own model table: fable (claude-fable-5) is natively 1M —
// the CLI reports contextWindow 1000000 for it and resolves "fable[1m]" to
// the same model; for the older families a "[1m]" alias opts into the 1M
// window; everything else runs the standard 200k.
func contextWindowForModel(model string) int {
	if strings.Contains(model, "[1m]") || strings.Contains(model, "fable") {
		return 1_000_000
	}
	return 200_000
}

// promptNeedle returns a short search key proving the prompt reached the
// transcript: the JSON-encoded form of its head, as claude writes it into
// the user line. HTML escaping is off to match JS JSON.stringify.
func promptNeedle(prompt string) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(prompt); err != nil {
		return ""
	}
	s := strings.TrimSpace(sb.String())
	if len(s) < 2 { // strip surrounding quotes
		return ""
	}
	s = s[1 : len(s)-1]
	const max = 48
	if len(s) > max {
		s = s[:max]
		// Never cut inside an escape sequence — a dangling backslash run
		// would make the needle unmatchable. Cutting mid-rune is fine: the
		// transcript encodes the same prompt with the same bytes, so a
		// partial-rune prefix still matches as a byte substring.
		s = strings.TrimRight(s, "\\")
	}
	return s
}

// needleMatch reports whether a `user` transcript line carries this turn's
// prompt echo. An empty needle (a degenerate all-backslash prompt) never
// matches — falling through to the stall guard is safer than confirming
// submission on the first arbitrary user line.
func needleMatch(needle string, raw json.RawMessage) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(string(raw), needle)
}

// buildArgv assembles the interactive claude argv. No -p: the whole point
// is a genuine interactive session.
func buildArgv(settingsJSON string, opts Options) []string {
	argv := []string{"--settings", settingsJSON}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	if opts.Effort != "" {
		argv = append(argv, "--effort", opts.Effort)
	}
	if opts.PermissionMode != "" {
		argv = append(argv, "--permission-mode", opts.PermissionMode)
	}
	if opts.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	if opts.DisallowedTools != "" {
		argv = append(argv, "--disallowedTools", opts.DisallowedTools)
	}
	if opts.Resume != "" {
		argv = append(argv, "--resume", opts.Resume)
	}
	return argv
}

// childEnv is the parent env plus FIFO path and TERM, minus
// CLAUDE_CODE_ENTRYPOINT — the child must compute its own entrypoint from
// its genuinely interactive launch, not inherit ours.
func childEnv(fifoPath string) []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		env = append(env, e)
	}
	return append(env,
		"CLAUDETTY_FIFO="+fifoPath,
		"TERM=xterm-256color",
	)
}

func waitQuiescent(lastOutputNS *atomic.Int64, trace func(string, ...any)) {
	waitStart := time.Now()
	for {
		if time.Since(waitStart) > inkMaxWait {
			trace("Ink readiness wait hit max (%s) — typing anyway", inkMaxWait)
			return
		}
		last := lastOutputNS.Load()
		if last != 0 {
			silent := time.Since(time.Unix(0, last))
			if silent > inkQuiescence {
				trace("Ink quiescent (silent %dms, waited %dms)",
					silent.Milliseconds(), time.Since(waitStart).Milliseconds())
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
}
