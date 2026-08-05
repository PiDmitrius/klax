package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/transport"
)

// One platform being down must not hold up anything else. The call under test runs in a goroutine so
// a synchronous implementation FAILS here rather than hanging the suite.
func TestConnectTransportDoesNotBlockTheCaller(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)

	returned := make(chan struct{})
	go func() {
		connectTransport("down", func() error {
			<-blocked // never returns until the test releases it
			return nil
		}, func() {}, nil)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("connectTransport blocked its caller")
	}
}

func TestConnectTransportRetriesUntilThePlatformAnswers(t *testing.T) {
	orig := connectBackoff
	connectBackoff = func(int) time.Duration { return 5 * time.Millisecond }
	defer func() { connectBackoff = orig }()

	var attempts atomic.Int32
	ready := make(chan struct{})
	connectTransport("flaky", func() error {
		if attempts.Add(1) < 3 {
			return &transport.APIError{Platform: "tg", Code: 502, Description: "Bad Gateway"}
		}
		return nil
	}, func() { close(ready) }, nil)

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("transport never became ready after %d attempts", attempts.Load())
	}
	if got := attempts.Load(); got < 3 {
		t.Fatalf("became ready after %d attempts, want the transient failures to have been retried", got)
	}
}

// A handshake that succeeds after `/transports off` must not start the poller. The retry runs for the
// whole outage, so the gap between the command and the callback is unbounded, not a scheduling skew.
func TestStartPollRespectsDisabledAtTheMomentItStarts(t *testing.T) {
	d := &daemon{
		disabled: map[string]bool{"tg": true},
		sources:  map[string]Source{"tg": &legacySource{name: "tg", poll: func(context.Context) {}}},
		pollCtx:  make(map[string]context.CancelFunc),
	}

	d.startPoll("tg") // the late ready-callback of a background handshake

	d.mu.Lock()
	_, running := d.pollCtx["tg"]
	d.mu.Unlock()
	if running {
		t.Fatal("a disabled transport started polling")
	}

	// Enabling clears the flag first, so the same call now starts it.
	d.mu.Lock()
	delete(d.disabled, "tg")
	d.mu.Unlock()
	d.startPoll("tg")
	d.mu.Lock()
	_, running = d.pollCtx["tg"]
	d.mu.Unlock()
	if !running {
		t.Fatal("an enabled transport did not start polling")
	}
}

// Enabling a transport must re-run its readiness check, not start a raw poll loop: after a permanent
// failure the credentials were never validated, and the handshake is the only thing that validates them.
func TestConnectRevalidatesBeforePolling(t *testing.T) {
	orig := connectBackoff
	connectBackoff = func(int) time.Duration { return 5 * time.Millisecond }
	defer func() { connectBackoff = orig }()

	var shook atomic.Int32
	ready := make(chan struct{})
	d := &daemon{
		disabled:   map[string]bool{},
		connecting: map[string]bool{},
		pollCtx:    make(map[string]context.CancelFunc),
		sources:    map[string]Source{"tg": &legacySource{name: "tg", poll: func(context.Context) { close(ready) }}},
		handshakes: map[string]func() error{"tg": func() error { shook.Add(1); return nil }},
	}

	d.connect("tg")
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("poll never started")
	}
	if shook.Load() != 1 {
		t.Fatalf("handshake ran %d times, want exactly 1 before polling", shook.Load())
	}
}

// The handshake DISCARDS the platform's pending updates, so re-running it against a transport that is
// already polling would eat live messages and move the bot's cursor under its own poll loop.
// `/transports on` for an already-enabled transport must therefore do nothing.
func TestConnectIsIdempotentWhileAlreadyPolling(t *testing.T) {
	var shook atomic.Int32
	ready := make(chan struct{})
	var once sync.Once
	d := &daemon{
		disabled:   map[string]bool{},
		connecting: map[string]bool{},
		pollCtx:    make(map[string]context.CancelFunc),
		sources:    map[string]Source{"tg": &legacySource{name: "tg", poll: func(context.Context) { once.Do(func() { close(ready) }) }}},
		handshakes: map[string]func() error{"tg": func() error { shook.Add(1); return nil }},
	}

	d.connect("tg")
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("poll never started")
	}
	// Wait until the transport is polling AND the in-flight marker has cleared, so the second connect
	// can only be stopped by the "already polling" check — otherwise this would pass on timing alone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		_, polling := d.pollCtx["tg"]
		inFlight := d.connecting["tg"]
		d.mu.Unlock()
		if polling && !inFlight {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.mu.Lock()
	_, polling := d.pollCtx["tg"]
	inFlight := d.connecting["tg"]
	d.mu.Unlock()
	if !polling || inFlight {
		t.Fatalf("setup did not settle: polling=%v inFlight=%v", polling, inFlight)
	}

	d.connect("tg") // a second `/transports on`
	time.Sleep(200 * time.Millisecond)
	if got := shook.Load(); got != 1 {
		t.Fatalf("handshake ran %d times; re-running it drains and discards pending updates", got)
	}
}

// A permanent error abandons one transport's connection attempt and leaves the rest running.
func TestConnectTransportGivesUpOnAPermanentError(t *testing.T) {
	var attempts atomic.Int32
	var readyCalled atomic.Bool
	connectTransport("badtoken", func() error {
		attempts.Add(1)
		return &transport.APIError{Platform: "tg", Code: 401, Description: "Unauthorized"}
	}, func() { readyCalled.Store(true) }, nil)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && attempts.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // long enough for a retry, if one were coming
	if got := attempts.Load(); got != 1 {
		t.Fatalf("permanent failure was attempted %d times, want exactly 1", got)
	}
	if readyCalled.Load() {
		t.Fatal("a permanently failed transport must not start its poll loop")
	}
}

// Fixtures here are synthetic and must stay synthetic: redaction matches on shape, not value.
func TestRedactSecretsStripsCredentials(t *testing.T) {
	const token = "1234567890:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	cases := []string{
		`Post "https://api.telegram.org/bot` + token + `/getUpdates": context deadline exceeded`,
		`Get "https://api.vk.com/method/groups.getById?access_token=` + token + `&v=5.199"`,
	}
	for _, in := range cases {
		out := redactSecrets(in)
		if strings.Contains(out, token) {
			t.Fatalf("token survived redaction:\n in=%q\nout=%q", in, out)
		}
		if !strings.Contains(out, "<redacted>") {
			t.Fatalf("redaction marker missing: %q", out)
		}
	}
	// The bot ID is public identity, not a credential, so it survives.
	if got := redactSecrets(`https://api.telegram.org/bot1234567890:` + strings.Repeat("A", 30) + `/getMe`); !strings.Contains(got, "bot1234567890:<redacted>") {
		t.Fatalf("bot id should survive redaction: %q", got)
	}

	// Ordinary text must be untouched.
	for _, plain := range []string{
		"tg API: Bad Gateway",
		"resolving bottlenecks in the read model",
		"robot arm token bucket refilled",
	} {
		if got := redactSecrets(plain); got != plain {
			t.Fatalf("redaction altered a clean message:\n in=%q\nout=%q", plain, got)
		}
	}
}

// Redaction lives on the logger's sink, so it holds for any call site.
func TestLogSinkRedactsWhateverIsWrittenThroughIt(t *testing.T) {
	var buf bytes.Buffer
	w := redactingWriter{w: &buf}
	lg := log.New(w, "klax: ", 0)

	const secret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	lg.Printf(`tg: getUpdates error: Post "https://api.telegram.org/bot1234567890:%s/getUpdates": timeout`, secret)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret reached the log sink: %q", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Fatalf("nothing was redacted: %q", out)
	}
	// A short count reads as a write error to log.Output.
	n, err := w.Write([]byte("bot1234567890:" + secret + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := len("bot1234567890:" + secret + "\n"); n != want {
		t.Fatalf("Write reported %d bytes, want the caller's %d", n, want)
	}
}

// Nothing on the startup path may wait for the announcement: a send retries for up to sendTimeout
// per user against an unreachable platform.
func TestStartupAnnouncementDoesNotBlockStartup(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	d := newTestDeliveryDaemon(&blockingTransport{until: blocked})
	d.cfg = &config.Config{AllowedUsers: []int64{1}}
	d.disabled = map[string]bool{}

	// The production call itself must return; wrapping it in a goroutine here would test the wrapper.
	returned := make(chan struct{})
	go func() { d.announceStartup("klax обновился"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the startup announcement blocked its caller")
	}

	// And the stand-in must really be blocking, or the assertion above is vacuous.
	stuck := make(chan struct{})
	go func() { d.notifyAllUsers("direct"); close(stuck) }()
	select {
	case <-stuck:
		t.Fatal("the stand-in transport did not block, so this test proves nothing")
	case <-time.After(200 * time.Millisecond):
	}
}

// blockingTransport stands in for an unreachable platform.
type blockingTransport struct {
	fakeTransport
	until <-chan struct{}
}

func (b *blockingTransport) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	<-b.until
	return "", nil
}

func (b *blockingTransport) SendMessage(chatID, text, replyTo, format string) error {
	<-b.until
	return nil
}

func TestIsPermanentStartupErrorTreatsGatewayFailuresAsTransient(t *testing.T) {
	transient := []error{
		&transport.APIError{Platform: "tg", Code: 502, Description: "Bad Gateway"},
		&transport.APIError{Platform: "tg", Code: 429, Description: "Too Many Requests"},
		&transport.APIError{Platform: "tg", Code: 500, Description: "Internal Server Error"},
		errors.New("context deadline exceeded"),
	}
	for _, err := range transient {
		if isPermanentStartupError(err) {
			t.Fatalf("%v classified permanent: the transport would be abandoned during an outage", err)
		}
	}
	if !isPermanentStartupError(&transport.APIError{Platform: "tg", Code: 401, Description: "Unauthorized"}) {
		t.Fatal("401 must be permanent")
	}
}
