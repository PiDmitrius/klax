// klax daemon — Telegram bridge for Claude Code.
// Uses claude -p --output-format stream-json for streaming responses.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/max"
	"github.com/PiDmitrius/klax/internal/mdhtml"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/tg"
	"github.com/PiDmitrius/klax/internal/transport"
	"github.com/PiDmitrius/klax/internal/vk"
)

const version = "0.3.77"

func main() {
	log.SetPrefix("klax: ")

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "start":
		fg := len(os.Args) > 2 && os.Args[2] == "--foreground"
		if fg {
			runDaemon()
		} else {
			runServiceStart()
		}
	case "stop":
		runServiceCtl("stop")
	case "restart":
		runServiceCtl("restart")
	case "status":
		runStatus()
	case "install":
		runInstall()
	case "uninstall":
		runUninstall()
	case "update":
		runUpdate()
	case "setup":
		runSetup()
	case "version":
		fmt.Printf("klax %s\n", version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "klax %s — Telegram bridge for Claude Code\n\n", version)
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  setup       Interactive first-time setup")
	fmt.Fprintln(os.Stderr, "  install     Install systemd user service")
	fmt.Fprintln(os.Stderr, "  uninstall   Remove systemd user service")
	fmt.Fprintln(os.Stderr, "  start       Start the service (--foreground to run directly)")
	fmt.Fprintln(os.Stderr, "  stop        Stop the service")
	fmt.Fprintln(os.Stderr, "  restart     Restart the service")
	fmt.Fprintln(os.Stderr, "  update      Build, install, and restart (from source)")
	fmt.Fprintln(os.Stderr, "  status      Show service status")
	fmt.Fprintln(os.Stderr, "  version     Print version")
}

// --- Service control ---

func runServiceStart() {
	cmd := exec.Command("systemctl", "--user", "start", "klax")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "failed: %v\nTry 'klax install' first, or 'klax start --foreground'\n", err)
		os.Exit(1)
	}
	fmt.Println("klax started")
}

func runServiceCtl(action string) {
	cmd := exec.Command("systemctl", "--user", action, "klax")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func runStatus() {
	cmd := exec.Command("systemctl", "--user", "status", "klax", "--no-pager", "-l")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

// --- Restart marker ---

type restartMarker struct {
	Reason    string `json:"reason"`    // "update" or "restart"
	Version   string `json:"version"`   // version before restart
	Timestamp int64  `json:"timestamp"` // unix timestamp
}

func markerPath() string {
	return filepath.Join(session.StoreDir(), "restart.marker")
}

func writeMarker(reason string) error {
	m := restartMarker{
		Reason:    reason,
		Version:   version,
		Timestamp: time.Now().Unix(),
	}
	if err := os.MkdirAll(session.StoreDir(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(), data, 0600)
}

func readMarker() *restartMarker {
	data, err := os.ReadFile(markerPath())
	if err != nil {
		return nil
	}
	var m restartMarker
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

func removeMarker() {
	os.Remove(markerPath())
}

// --- PID file ---

func pidPath() string {
	return filepath.Join(session.StoreDir(), "klax.pid")
}

func writePID() {
	os.MkdirAll(session.StoreDir(), 0700)
	os.WriteFile(pidPath(), []byte(fmt.Sprintf("%d", os.Getpid())), 0600)
}

func removePID() {
	os.Remove(pidPath())
}

// --- Update ---

var versionRe = regexp.MustCompile(`(const version = ")(\d+)\.(\d+)\.(\d+)(")`)

func bumpPatch(srcDir string) error {
	path := filepath.Join(srcDir, "cmd", "klax", "main.go")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := versionRe.FindSubmatchIndex(data)
	if m == nil {
		return fmt.Errorf("version string not found in %s", path)
	}
	patch, _ := strconv.Atoi(string(data[m[8]:m[9]]))
	newVersion := fmt.Sprintf("%s%s.%s.%d%s",
		string(data[m[2]:m[3]]),
		string(data[m[4]:m[5]]),
		string(data[m[6]:m[7]]),
		patch+1,
		string(data[m[10]:m[11]]),
	)
	out := make([]byte, 0, len(data)+4)
	out = append(out, data[:m[0]]...)
	out = append(out, newVersion...)
	out = append(out, data[m[1]:]...)
	fmt.Printf("version: %s.%s.%d → %s.%s.%d\n",
		string(data[m[4]:m[5]]), string(data[m[6]:m[7]]), patch,
		string(data[m[4]:m[5]]), string(data[m[6]:m[7]]), patch+1)
	return os.WriteFile(path, out, 0644)
}

func runUpdate() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot load config: %v\nRun 'klax setup' first.\n", err)
		os.Exit(1)
	}

	// Write restart marker with current (old) version before bumping.
	if err := writeMarker("update"); err != nil {
		log.Printf("warning: could not write restart marker: %v", err)
	}

	srcDir := cfg.SourceDir
	if srcDir == "" {
		// No local source — install from upstream.
		fmt.Println("installing from upstream...")
		goInstall := exec.Command("go", "install", "github.com/PiDmitrius/klax/cmd/klax@latest")
		goInstall.Stdout = os.Stdout
		goInstall.Stderr = os.Stderr
		if err := goInstall.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "go install failed: %v\n", err)
			os.Exit(1)
		}
		// Update systemd unit and restart.
		home, _ := os.UserHomeDir()
		newBin := filepath.Join(home, "go", "bin", "klax")
		install := exec.Command(newBin, "install")
		install.Stdout = os.Stdout
		install.Stderr = os.Stderr
		if err := install.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("daemon will restart via marker")
		return
	}

	// Local source — bump version and build.
	if err := bumpPatch(srcDir); err != nil {
		fmt.Fprintf(os.Stderr, "version bump failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("building in %s...\n", srcDir)
	build := exec.Command("go", "build", "-o", filepath.Join(srcDir, "klax"), "./cmd/klax")
	build.Dir = srcDir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n", err)
		os.Exit(1)
	}

	// Install (copies binary to ~/go/bin/, updates service, writes restart marker)
	install := exec.Command(filepath.Join(srcDir, "klax"), "install")
	install.Stdout = os.Stdout
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
		os.Exit(1)
	}

	// Daemon will pick up the marker and restart via drain.
	fmt.Println("daemon will restart via marker")
}

// --- Setup ---

func runSetup() {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("klax setup")
	fmt.Println("----------")

	cfg := &config.Config{}

	fmt.Print("Telegram bot token: ")
	token, _ := reader.ReadString('\n')
	cfg.TelegramToken = strings.TrimSpace(token)

	fmt.Print("Your Telegram user ID (from @userinfobot): ")
	var uid int64
	fmt.Scan(&uid)
	cfg.AllowedUsers = []int64{uid}

	fmt.Print("Default working directory [~]: ")
	reader.ReadString('\n') // consume newline after Scan
	cwd, _ := reader.ReadString('\n')
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "~" {
		cwd, _ = os.UserHomeDir()
	}
	cfg.DefaultCWD = cwd
	cfg.PermissionMode = "bypassPermissions"

	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Saved to %s\n", filepath.Join(config.Dir(), "config.json"))
	fmt.Println("Next: klax install && klax start")
}

// --- Install ---

func runInstall() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find executable: %v\n", err)
		os.Exit(1)
	}

	home, _ := os.UserHomeDir()
	binDir := filepath.Join(home, "go", "bin")
	os.MkdirAll(binDir, 0755)
	dst := filepath.Join(binDir, "klax")
	if err := copyFile(exe, dst, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot install: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("installed: %s\n", dst)

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	os.MkdirAll(unitDir, 0755)
	unitPath := filepath.Join(unitDir, "klax.service")
	unit := fmt.Sprintf(`[Unit]
Description=klax — Telegram bridge for Claude Code
After=network.target

[Service]
Type=simple
ExecStart=%s start --foreground
Restart=always
RestartSec=5
StartLimitBurst=3
StartLimitIntervalSec=60

[Install]
WantedBy=default.target
`, dst)
	os.WriteFile(unitPath, []byte(unit), 0644)
	fmt.Printf("installed: %s\n", unitPath)

	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "enable", "klax").Run()

	// Write restart marker if not already present (update writes it before build).
	if readMarker() == nil {
		if err := writeMarker("update"); err != nil {
			log.Printf("warning: could not write restart marker: %v", err)
		}
	}

	// Check if the service is currently running.
	out, _ := exec.Command("systemctl", "--user", "is-active", "klax").Output()
	if strings.TrimSpace(string(out)) == "active" {
		fmt.Println("\nInstalled. Service is running — it will restart automatically.")
	} else {
		fmt.Println("\nInstalled. Run: klax start")
	}
}

func runUninstall() {
	exec.Command("systemctl", "--user", "stop", "klax").Run()
	exec.Command("systemctl", "--user", "disable", "klax").Run()
	home, _ := os.UserHomeDir()
	os.Remove(filepath.Join(home, ".config", "systemd", "user", "klax.service"))
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Println("uninstalled")
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Remove old file first to avoid "text file busy" when overwriting a running binary.
	os.Remove(dst)
	return os.WriteFile(dst, data, mode)
}

// --- Daemon ---

// sessionRunner holds a per-session runner and message queue.
// Different sessions run Claude in parallel; within a session, messages are serialized.
type sessionRunner struct {
	runner     *runner.Runner
	mu         sync.Mutex
	queue      []queuedMsg
	processing bool
	cancel     context.CancelFunc // cancels current run (claude process + retry loops)
}

type daemon struct {
	cfg        *config.Config
	transports map[string]transport.Transport // "tg" -> tg.Bot, "mx" -> max.Bot
	formats    map[string]string              // "tg" -> "html", "vk" -> ""
	disabled   map[string]bool                // disabled transports
	pollCtx    map[string]context.CancelFunc  // cancel functions for poll goroutines
	store      *session.Store
	runners    map[string]*sessionRunner // sessionKey -> runner+queue
	runnersMu  sync.Mutex
	mu         sync.Mutex
	draining   bool             // stop accepting new tasks, wait for current to finish
	drainWg    sync.WaitGroup   // tracks active sessionRunners for drain
	identities map[int64]string // telegram userID -> canonical user ID
	maxIdents  map[int64]string // max userID -> canonical user ID
	vkIdents   map[int]string   // vk userID -> canonical user ID
}

// getRunner returns the sessionRunner for the given key, creating one if needed.
func (d *daemon) getRunner(sessionKey string) *sessionRunner {
	d.runnersMu.Lock()
	defer d.runnersMu.Unlock()
	sr, ok := d.runners[sessionKey]
	if !ok {
		sr = &sessionRunner{runner: runner.New()}
		d.runners[sessionKey] = sr
	}
	return sr
}

// transportPrefix extracts the transport prefix from a chatID (e.g. "tg" from "tg:123").
func transportPrefix(chatID string) string {
	if idx := strings.Index(chatID, ":"); idx != -1 {
		return chatID[:idx]
	}
	return "tg" // legacy chatIDs without prefix are telegram
}

// transportFor returns the transport, raw chatID (prefix stripped), and format for a prefixed chatID.
func (d *daemon) transportFor(chatID string) (transport.Transport, string, string) {
	if idx := strings.Index(chatID, ":"); idx != -1 {
		prefix := chatID[:idx]
		raw := chatID[idx+1:]
		if t, ok := d.transports[prefix]; ok {
			return t, raw, d.formats[prefix]
		}
	}
	// Fallback to tg
	if t, ok := d.transports["tg"]; ok {
		return t, chatID, d.formats["tg"]
	}
	return nil, chatID, ""
}

// sessionKey resolves a chatID to the key used for session storage.
// For DMs from known users, maps to canonical user ID so sessions are shared cross-platform.
func (d *daemon) sessionKey(chatID string) string {
	if idx := strings.Index(chatID, ":"); idx != -1 {
		prefix := chatID[:idx]
		raw := chatID[idx+1:]
		switch prefix {
		case "tg":
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
				// Positive IDs are DMs, negative are groups
				if id > 0 {
					if canonical, ok := d.identities[id]; ok {
						return "user:" + canonical
					}
				}
			}
		case "mx":
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
				if id > 0 {
					if canonical, ok := d.maxIdents[id]; ok {
						return "user:" + canonical
					}
				}
			}
		case "vk":
			if id, err := strconv.Atoi(raw); err == nil {
				// VK DMs: peer_id == user_id (< 2000000000), groups: peer_id >= 2000000000
				if id < 2000000000 {
					if canonical, ok := d.vkIdents[id]; ok {
						return "user:" + canonical
					}
				}
			}
		}
	}
	return chatID
}

type queuedMsg struct {
	chatID     string
	msgID      string // user's message ID (for replyTo)
	text       string
	progressID string // ID of "В очереди" message to reuse as progress
}

func runDaemon() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("cannot load config: %v\nRun 'klax setup' first.", err)
	}
	if cfg.TelegramToken == "" && cfg.MaxToken == "" {
		log.Fatal("no tokens configured. Run 'klax setup'.")
	}

	transports := make(map[string]transport.Transport)

	// --- Telegram ---
	var tgBot *tg.Bot
	if cfg.TelegramToken != "" {
		tgBot = tg.New(cfg.TelegramToken)
		for {
			err := tgBot.GetMe()
			if err == nil {
				if err := tgBot.DrainUpdates(); err != nil {
					log.Printf("warning: tg drain updates: %v", err)
				}
				tgBot.SetMyCommands(tgMenuCommands)
				break
			}
			var apiErr *tg.APIError
			if errors.As(err, &apiErr) {
				log.Fatalf("telegram auth failed: %v", err)
			}
			log.Printf("telegram unreachable: %v (retry in 10s)", err)
			time.Sleep(10 * time.Second)
		}
		transports["tg"] = tgBot
		log.Println("[OK] Telegram connected")
	}

	// --- MAX ---
	var maxBot *max.Bot
	if cfg.MaxToken != "" {
		maxBot = max.New(cfg.MaxToken)
		me, err := maxBot.GetMe()
		if err != nil {
			log.Fatalf("MAX auth failed: %v", err)
		}
		if err := maxBot.DrainUpdates(); err != nil {
			log.Printf("warning: max drain updates: %v", err)
		}
		transports["mx"] = maxBot
		log.Printf("[OK] MAX bot: [%d] %s (@%s)", me.UserID, me.Name, me.Username)
	}

	// --- VK ---
	var vkBot *vk.Bot
	if cfg.VKToken != "" {
		vkBot = vk.New(cfg.VKToken)
		group, err := vkBot.GetMe()
		if err != nil {
			log.Fatalf("VK auth failed: %v", err)
		}
		if err := vkBot.DrainUpdates(); err != nil {
			log.Printf("warning: vk drain updates: %v", err)
		}
		transports["vk"] = vkBot
		log.Printf("[OK] VK group: [%d] %s", group.ID, group.Name)
	}

	store, err := session.LoadStore()
	if err != nil {
		log.Fatalf("cannot load sessions: %v", err)
	}

	// Build identity maps from config.
	tgIdents := make(map[int64]string)
	maxIdents := make(map[int64]string)
	vkIdents := make(map[int]string)
	for _, u := range cfg.Users {
		if u.TelegramID != 0 {
			tgIdents[u.TelegramID] = u.ID
		}
		if u.MaxID != 0 {
			maxIdents[u.MaxID] = u.ID
		}
		if u.VKID != 0 {
			vkIdents[int(u.VKID)] = u.ID
		}
	}

	// Migrate legacy flat sessions to first user's canonical ID or tg chatID.
	if len(cfg.AllowedUsers) > 0 {
		uid := cfg.AllowedUsers[0]
		migrateKey := fmt.Sprintf("tg:%d", uid)
		if canonical, ok := tgIdents[uid]; ok {
			migrateKey = "user:" + canonical
		}
		if store.MigrateTo(migrateKey) {
			store.Save()
			log.Printf("migrated legacy sessions to %s", migrateKey)
		}
	}

	// Merge platform-specific session keys into canonical user keys.
	for _, u := range cfg.Users {
		targetKey := "user:" + u.ID
		var oldKeys []string
		if u.TelegramID != 0 {
			oldKeys = append(oldKeys, fmt.Sprintf("tg:%d", u.TelegramID))
			oldKeys = append(oldKeys, fmt.Sprintf("%d", u.TelegramID))
		}
		if u.MaxID != 0 {
			oldKeys = append(oldKeys, fmt.Sprintf("mx:%d", u.MaxID))
			oldKeys = append(oldKeys, fmt.Sprintf("max:%d", u.MaxID)) // legacy prefix
		}
		if u.VKID != 0 {
			oldKeys = append(oldKeys, fmt.Sprintf("vk:%d", u.VKID))
		}
		if store.MergeKeys(targetKey, oldKeys) {
			store.Save()
			log.Printf("merged sessions into %s", targetKey)
		}
	}

	disabled := make(map[string]bool)
	for _, name := range cfg.DisabledTransports {
		disabled[name] = true
	}

	d := &daemon{
		cfg:        cfg,
		transports: transports,
		formats:    map[string]string{"tg": "html", "mx": "html", "vk": ""},
		disabled:   disabled,
		pollCtx:    make(map[string]context.CancelFunc),
		store:      store,
		runners:    make(map[string]*sessionRunner),
		identities: tgIdents,
		maxIdents:  maxIdents,
		vkIdents:   vkIdents,
	}

	writePID()
	log.Printf("klax %s started (pid %d)", version, os.Getpid())

	// Startup notification: if restart marker exists, notify users we're back.
	if m := readMarker(); m != nil {
		var text string
		switch m.Reason {
		case "update":
			text = fmt.Sprintf("✅ klax обновлён (v%s)", version)
		default:
			text = fmt.Sprintf("✅ klax перезапущен (v%s)", version)
		}
		d.notifyAllUsers(text)
		removeMarker()
	}

	// Handle SIGTERM/SIGINT for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, draining...", sig)
		d.startDrain("signal")
	}()

	// Watch for restart marker (runs after startup marker is cleared).
	go d.watchMarker()

	// Start polling loops for enabled transports.
	if tgBot != nil && !disabled["tg"] {
		d.startPoll("tg")
	}
	if maxBot != nil && !disabled["mx"] {
		d.startPoll("mx")
	}
	if vkBot != nil && !disabled["vk"] {
		d.startPoll("vk")
	}

	// Block forever (goroutines do the work).
	select {}
}

// startPoll starts the polling goroutine for the given transport.
func (d *daemon) startPoll(name string) {
	d.mu.Lock()
	// Stop existing poll if running.
	if cancel, ok := d.pollCtx[name]; ok {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.pollCtx[name] = cancel
	d.mu.Unlock()

	switch name {
	case "tg":
		go d.pollTG(ctx)
	case "mx":
		go d.pollMAX(ctx)
	case "vk":
		go d.pollVK(ctx)
	}
	log.Printf("poll started: %s", name)
}

// stopPoll stops the polling goroutine for the given transport.
func (d *daemon) stopPoll(name string) {
	d.mu.Lock()
	if cancel, ok := d.pollCtx[name]; ok {
		cancel()
		delete(d.pollCtx, name)
	}
	d.mu.Unlock()
	log.Printf("poll stopped: %s", name)
}

// notifyAllUsers sends a message to all allowed users on enabled platforms.
// These are self-initiated messages (no replyTo).
func (d *daemon) notifyAllUsers(text string) {
	if _, ok := d.transports["tg"]; ok && !d.disabled["tg"] {
		for _, uid := range d.cfg.AllowedUsers {
			chatID := fmt.Sprintf("tg:%d", uid)
			log.Printf("notify %s", chatID)
			d.sendMessage(chatID, "", text)
		}
	}
	if _, ok := d.transports["mx"]; ok && !d.disabled["mx"] {
		for _, uid := range d.cfg.MaxAllowedUsers {
			chatID := fmt.Sprintf("mx:%d", uid)
			log.Printf("notify %s", chatID)
			d.sendMessage(chatID, "", text)
		}
	}
	if _, ok := d.transports["vk"]; ok && !d.disabled["vk"] {
		for _, uid := range d.cfg.VKAllowedUsers {
			chatID := fmt.Sprintf("vk:%d", uid)
			log.Printf("notify %s", chatID)
			d.sendMessage(chatID, "", text)
		}
	}
}

// startDrain puts the daemon into draining mode.
// Waits for current task AND queued tasks to finish before shutting down.
func (d *daemon) startDrain(reason string) {
	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		return
	}
	d.draining = true
	d.mu.Unlock()

	log.Printf("drain started (reason: %s)", reason)
	if readMarker() == nil {
		writeMarker(reason)
	}
	d.notifyAllUsers("🔄 klax перезапускается...")

	// Kick processing on all session runners that have queued messages.
	d.runnersMu.Lock()
	for _, sr := range d.runners {
		sr.mu.Lock()
		hasItems := len(sr.queue) > 0 && !sr.processing
		sr.mu.Unlock()
		if hasItems {
			go d.processSessionQueue(sr)
		}
	}
	d.runnersMu.Unlock()

	// Wait for all active session runners to finish.
	log.Println("waiting for all sessions to drain...")
	d.drainWg.Wait()
	d.shutdown()
}

func (d *daemon) shutdown() {
	log.Println("shutting down")
	removePID()
	os.Exit(0)
}

func (d *daemon) isDraining() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.draining
}

func (d *daemon) watchMarker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if d.isDraining() {
			return
		}
		if m := readMarker(); m != nil {
			log.Printf("restart marker found (reason: %s)", m.Reason)
			d.startDrain(m.Reason)
			return
		}
	}
}

func (d *daemon) pollTG(ctx context.Context) {
	bot := d.transports["tg"].(*tg.Bot)
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := bot.GetUpdates()
		if err != nil {
			log.Printf("tg: getUpdates error: %v (retry in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, u := range updates {
			msg := u.Message
			if msg == nil {
				continue
			}
			// /id — respond to anyone, even unauthenticated
			if strings.TrimSpace(msg.Text) == "/id" || strings.HasPrefix(msg.Text, "/id@") {
				reply := fmt.Sprintf("user_id: %d\nchat_id: %d", msg.From.ID, msg.Chat.ID)
				bot.SendMessage(fmt.Sprintf("%d", msg.Chat.ID), reply, "", "")
				continue
			}
			if !d.isTGAllowed(msg.From.ID) {
				continue
			}
			chatID := fmt.Sprintf("tg:%d", msg.Chat.ID)
			msgID := fmt.Sprintf("%d", msg.MessageID)
			d.handleMessage(chatID, msgID, msg.Text)
		}
	}
}

func (d *daemon) pollMAX(ctx context.Context) {
	bot := d.transports["mx"].(*max.Bot)
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := bot.GetUpdates()
		if err != nil {
			log.Printf("mx: getUpdates error: %v (retry in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, upd := range updates {
			if upd.UpdateType != "message_created" {
				continue
			}
			senderID := upd.Message.Sender.UserID
			text := upd.Message.Body.Text
			// /id — respond to anyone
			if strings.TrimSpace(text) == "/id" {
				reply := fmt.Sprintf("user_id: %d", senderID)
				if upd.Message.Recipient.ChatID != 0 {
					reply += fmt.Sprintf("\nchat_id: %d", upd.Message.Recipient.ChatID)
				}
				if upd.Message.Recipient.ChatType == "dialog" {
					bot.SendMessage(fmt.Sprintf("%d", senderID), reply, "", "")
				} else {
					bot.SendMessage(fmt.Sprintf("%d", upd.Message.Recipient.ChatID), reply, "", "")
				}
				continue
			}
			if !d.isMAXAllowed(senderID) {
				continue
			}
			if text == "" {
				continue
			}
			var chatID string
			if upd.Message.Recipient.ChatType == "dialog" {
				chatID = fmt.Sprintf("mx:%d", senderID)
			} else {
				chatID = fmt.Sprintf("mx:%d", upd.Message.Recipient.ChatID)
			}
			msgID := upd.Message.Body.Mid
			d.handleMessage(chatID, msgID, text)
		}
	}
}

func (d *daemon) isTGAllowed(id int64) bool {
	for _, uid := range d.cfg.AllowedUsers {
		if uid == id {
			return true
		}
	}
	return false
}

func (d *daemon) pollVK(ctx context.Context) {
	bot := d.transports["vk"].(*vk.Bot)
	for {
		if ctx.Err() != nil {
			return
		}
		updates, err := bot.GetUpdates()
		if err != nil {
			log.Printf("vk: getUpdates error: %v (retry in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, upd := range updates {
			if upd.Type != "message_new" {
				continue
			}
			mn, err := vk.ParseMessageNew(upd)
			if err != nil {
				log.Printf("vk: parse message_new: %v", err)
				continue
			}
			msg := mn.Message
			// /id — respond to anyone
			if strings.TrimSpace(msg.Text) == "/id" {
				reply := fmt.Sprintf("from_id: %d\npeer_id: %d", msg.FromID, msg.PeerID)
				bot.SendMessage(strconv.Itoa(msg.PeerID), reply, "", "")
				continue
			}
			if !d.isVKAllowed(msg.FromID) {
				continue
			}
			if msg.Text == "" {
				continue
			}
			chatID := fmt.Sprintf("vk:%d", msg.PeerID)
			msgID := strconv.Itoa(msg.ID)
			d.handleMessage(chatID, msgID, msg.Text)
		}
	}
}

func (d *daemon) isVKAllowed(id int) bool {
	for _, uid := range d.cfg.VKAllowedUsers {
		if uid == id {
			return true
		}
	}
	return false
}

func (d *daemon) isMAXAllowed(id int64) bool {
	for _, uid := range d.cfg.MaxAllowedUsers {
		if uid == id {
			return true
		}
	}
	return false
}

func (d *daemon) handleMessage(chatID, msgID, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	// Ensure chat has at least one session.
	sk := d.sessionKey(chatID)
	d.ensureSession(sk)

	// Handle built-in commands
	if strings.HasPrefix(text, "/") {
		d.handleCommand(chatID, msgID, text)
		return
	}

	// Queue for Claude
	d.enqueue(chatID, msgID, text)
}

func (d *daemon) ensureSession(sessionKey string) {
	if d.store.Active(sessionKey) != nil {
		return
	}
	cwd := d.cfg.DefaultCWD
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	d.store.New(sessionKey, "default", cwd)
	d.store.Save()
}

func (d *daemon) handleCommand(chatID, msgID, text string) {
	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	// Strip @botname suffix (e.g. /sessions@klax_bot → /sessions)
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}
	sk := d.sessionKey(chatID)

	switch cmd {
	case "/help":
		d.sendMessage(chatID, msgID, helpText())

	case "/status":
		d.sendMessage(chatID, msgID, d.statusText(sk))

	case "/sessions", "/s":
		d.sendMessage(chatID, msgID, d.sessionsText(sk))

	case "/cleanup":
		d.sendMessage(chatID, msgID, d.cleanupText(sk))

	case "/menu":
		prefix := transportPrefix(chatID)
		if prefix != "tg" {
			d.sendMessage(chatID, msgID, "Команда /menu доступна только в Telegram.")
			return
		}
		bot, ok := d.transports["tg"].(*tg.Bot)
		if !ok {
			d.sendMessage(chatID, msgID, "Telegram транспорт недоступен.")
			return
		}
		_, rawID, _ := d.transportFor(chatID)
		if err := bot.SetMyCommandsForChat(rawID, tgMenuCommands); err != nil {
			d.sendMessage(chatID, msgID, fmt.Sprintf("Ошибка: %v", err))
			return
		}
		d.sendMessage(chatID, msgID, "✅ Меню установлено.")

	case "/new":
		name := "session"
		if len(parts) > 1 {
			name = strings.Join(parts[1:], " ")
		}
		cwd := d.cfg.DefaultCWD
		if cwd == "" {
			cwd, _ = os.UserHomeDir()
		}
		sess := d.store.New(sk, name, cwd)
		d.store.Save()
		d.sendMessage(chatID, msgID, fmt.Sprintf("✅ Новая сессия: <code>%s</code>\n📂 <code>%s</code>", sess.Name, sess.CWD))

	case "/name":
		if len(parts) < 2 {
			d.sendMessage(chatID, msgID, "Использование: /name <имя>")
			return
		}
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		sess.Name = strings.Join(parts[1:], " ")
		d.store.Save()
		d.sendMessage(chatID, msgID, d.sessionsText(sk))

	case "/cwd":
		if len(parts) < 2 {
			sess := d.store.Active(sk)
			if sess != nil {
				d.sendMessage(chatID, msgID, fmt.Sprintf("📂 <code>%s</code>", sess.CWD))
			}
			return
		}
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		sess.CWD = strings.Join(parts[1:], " ")
		d.store.Save()
		d.sendMessage(chatID, msgID, fmt.Sprintf("📂 <code>%s</code>", sess.CWD))

	case "/prompt":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		if len(parts) < 2 {
			if sess.AppendSystemPrompt == "" {
				d.sendMessage(chatID, msgID, "Системный промпт не задан.")
			} else {
				d.sendMessage(chatID, msgID, fmt.Sprintf("📝 <code>%s</code>", sess.AppendSystemPrompt))
			}
			return
		}
		sess.AppendSystemPrompt = text[len("/prompt "):]
		d.store.Save()
		d.sendMessage(chatID, msgID, fmt.Sprintf("📝 <code>%s</code>", sess.AppendSystemPrompt))

	case "/model":
		sess := d.store.Active(sk)
		if sess == nil {
			d.sendMessage(chatID, msgID, "Нет активной сессии")
			return
		}
		d.sendPlain(chatID, msgID, d.modelText(sess))

	case "/transports":
		d.handleTransports(chatID, msgID, parts)

	case "/bypass":
		if len(parts) < 2 {
			d.sendMessage(chatID, msgID, "Использование: /bypass <команда>")
			return
		}
		prompt := text[len("/bypass "):]
		d.enqueue(chatID, msgID, prompt)

	case "/abort":
		sr := d.getRunner(sk)
		sr.mu.Lock()
		cancelFn := sr.cancel
		busy := sr.runner.IsBusy()
		sr.mu.Unlock()
		dropped := d.clearSessionQueue(sk)
		if !busy && cancelFn == nil && dropped == 0 {
			d.sendMessage(chatID, msgID, "Нет активных задач.")
			return
		}
		// Cancel context first (stops retry loops), then kill claude process.
		if cancelFn != nil {
			cancelFn()
		}
		sr.runner.Abort()
		reply := "❌ Прерван."
		if dropped > 0 {
			reply += fmt.Sprintf(" Очередь очищена (%d сообщений удалено).", dropped)
		}
		d.sendMessage(chatID, msgID, reply)

	default:
		// /m_MODEL shortcut for model switch
		if strings.HasPrefix(cmd, "/m_") {
			sess := d.store.Active(sk)
			if sess == nil {
				d.sendMessage(chatID, msgID, "Нет активной сессии")
				return
			}
			alias := cmd[3:]
			if alias == "default" {
				sess.ModelOverride = ""
				d.store.Save()
				d.sendPlain(chatID, msgID, d.modelText(sess))
				return
			}
			// Resolve alias to full model name.
			resolved := alias
			for _, m := range knownModels {
				if m.alias == alias {
					resolved = m.model
					break
				}
			}
			sess.ModelOverride = resolved
			d.store.Save()
			d.sendPlain(chatID, msgID, d.modelText(sess))
			return
		}
		// /sN shortcut for /switch N
		if strings.HasPrefix(cmd, "/s") {
			if n, err := strconv.Atoi(cmd[2:]); err == nil {
				sess := d.store.Switch(sk, n-1)
				if sess == nil {
					d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%d", n))
					return
				}
				d.store.Save()
				d.sendMessage(chatID, msgID, d.sessionsText(sk))
				return
			}
		}
		// /dN shortcut for deleting session N
		if strings.HasPrefix(cmd, "/d") {
			if n, err := strconv.Atoi(cmd[2:]); err == nil {
				idx := n - 1
				sessions := d.store.SessionsFor(sk)
				if idx < 0 || idx >= len(sessions) {
					d.sendMessage(chatID, msgID, fmt.Sprintf("Нет сессии #%d", n))
					return
				}
				if sessions[idx].Active {
					d.sendMessage(chatID, msgID, "Нельзя удалить активную сессию.")
					return
				}
				d.store.Delete(sk, idx)
				d.store.Save()
				d.sendMessage(chatID, msgID, d.cleanupText(sk))
				return
			}
		}
		d.sendMessage(chatID, msgID, fmt.Sprintf("Неизвестная команда: %s\nНапиши /help", cmd))
	}
}

func (d *daemon) handleTransports(chatID, msgID string, parts []string) {
	// /transports — list
	if len(parts) == 1 {
		d.sendPlain(chatID, msgID, d.transportsText())
		return
	}

	// /transports on|off <name>
	if len(parts) == 3 {
		action := strings.ToLower(parts[1])
		name := strings.ToLower(parts[2])

		// Normalize aliases
		switch name {
		case "max":
			name = "mx"
		case "telegram":
			name = "tg"
		}

		if _, ok := d.transports[name]; !ok {
			d.sendMessage(chatID, msgID, fmt.Sprintf("Транспорт %q не настроен.", name))
			return
		}

		current := transportPrefix(chatID)

		switch action {
		case "on":
			d.mu.Lock()
			delete(d.disabled, name)
			d.mu.Unlock()
			d.saveDisabled()
			d.startPoll(name)
			d.sendPlain(chatID, msgID, d.transportsText())
		case "off":
			if name == current {
				d.sendMessage(chatID, msgID, "Нельзя отключить транспорт, через который идёт команда.")
				return
			}
			d.stopPoll(name)
			d.mu.Lock()
			d.disabled[name] = true
			d.mu.Unlock()
			d.saveDisabled()
			d.sendPlain(chatID, msgID, d.transportsText())
		default:
			d.sendMessage(chatID, msgID, "Использование: /transports [on|off <tg|max|vk>]")
		}
		return
	}

	d.sendMessage(chatID, msgID, "Использование: /transports [on|off <tg|max|vk>]")
}

var transportOrder = []string{"tg", "mx", "vk"}

var tgMenuCommands = []tg.BotCommand{
	{Command: "status", Description: "Статус"},
	{Command: "sessions", Description: "Сессии"},
	{Command: "new", Description: "Новая сессия"},
	{Command: "model", Description: "Модель"},
	{Command: "abort", Description: "Прервать"},
}

func (d *daemon) transportsText() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var sb strings.Builder
	sb.WriteString("Транспорты:\n")
	for _, name := range transportOrder {
		if _, ok := d.transports[name]; !ok {
			continue
		}
		if d.disabled[name] {
			sb.WriteString(fmt.Sprintf("  %s — выкл\n", name))
		} else {
			sb.WriteString(fmt.Sprintf("  %s — вкл\n", name))
		}
	}
	return sb.String()
}

// saveDisabled persists the disabled transports set to config.
func (d *daemon) saveDisabled() {
	d.mu.Lock()
	var list []string
	for name := range d.disabled {
		list = append(list, name)
	}
	d.mu.Unlock()
	d.cfg.DisabledTransports = list
	config.Save(d.cfg)
}

func (d *daemon) enqueue(chatID, msgID, text string) {
	if d.isDraining() {
		d.sendMessage(chatID, msgID, "🔄 klax перезапускается, новые задачи не принимаются.")
		return
	}

	sk := d.sessionKey(chatID)
	sr := d.getRunner(sk)

	sr.mu.Lock()
	qm := queuedMsg{chatID: chatID, msgID: msgID, text: text}
	busy := sr.runner.IsBusy()
	if busy {
		// Send queue notification and capture its ID for later reuse as progress message.
		t, rawChatID, _ := d.transportFor(chatID)
		if t != nil {
			qlen := len(sr.queue) + 1 // +1 for this message being added
			if mid, err := t.SendMessageReturnID(rawChatID, fmt.Sprintf("⏳ В очереди: %d", qlen), msgID, ""); err == nil {
				qm.progressID = mid
			}
		}
	}
	sr.queue = append(sr.queue, qm)
	sr.mu.Unlock()

	if busy {
		return
	}

	go d.processSessionQueue(sr)
}

func (d *daemon) processSessionQueue(sr *sessionRunner) {
	sr.mu.Lock()
	if sr.processing {
		sr.mu.Unlock()
		return
	}
	sr.processing = true
	sr.mu.Unlock()

	d.drainWg.Add(1)
	defer d.drainWg.Done()

	defer func() {
		sr.mu.Lock()
		sr.processing = false
		sr.mu.Unlock()
	}()

	for {
		sr.mu.Lock()
		if len(sr.queue) == 0 {
			sr.mu.Unlock()
			return
		}
		msg := sr.queue[0]
		sr.queue = sr.queue[1:]
		// Update queue position in remaining messages' progress notifications.
		for i, qm := range sr.queue {
			if qm.progressID != "" {
				t, rawChatID, _ := d.transportFor(qm.chatID)
				if t != nil {
					t.EditMessage(rawChatID, qm.progressID, fmt.Sprintf("⏳ В очереди: %d", i+1), "")
				}
			}
		}
		sr.mu.Unlock()

		d.runClaude(msg)
	}
}

func (d *daemon) clearSessionQueue(sk string) int {
	sr := d.getRunner(sk)
	sr.mu.Lock()
	n := len(sr.queue)
	sr.queue = nil
	sr.mu.Unlock()
	return n
}

func (d *daemon) runClaude(msg queuedMsg) {
	sk := d.sessionKey(msg.chatID)
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(msg.chatID, msg.msgID, "❌ Нет активной сессии. Напиши /new")
		return
	}

	sr := d.getRunner(sk)

	// Create a cancellable context for this run.
	// /abort cancels it, which stops both claude and retry loops.
	ctx, cancel := context.WithCancel(context.Background())
	sr.mu.Lock()
	sr.cancel = cancel
	sr.mu.Unlock()
	defer cancel()

	// Progress message — edit in place.
	// If this message was queued, reuse the "В очереди" notification.
	t, rawChatID, chatFmt := d.transportFor(msg.chatID)
	progressMsgID := msg.progressID
	if t != nil {
		if progressMsgID != "" {
			// Edit existing queue notification into progress indicator.
			t.EditMessage(rawChatID, progressMsgID, "...", "")
		} else {
			// Send new progress message as reply to user's message.
			retryDo(ctx, func() error {
				mid, err := t.SendMessageReturnID(rawChatID, "...", msg.msgID, "")
				if err == nil {
					progressMsgID = mid
				}
				return err
			})
		}
	}

	var toolLines []string
	lastProgress := ""
	onProgress := func(status string) {
		if status == lastProgress {
			return
		}
		lastProgress = status
		toolLines = append(toolLines, status)

		newText := formatToolLines(toolLines, chatFmt) + "\n\n..."
		if progressMsgID != "" {
			t.EditMessage(rawChatID, progressMsgID, newText, chatFmt)
		}
	}

	permMode := sess.PermissionMode
	if permMode == "" {
		permMode = d.cfg.PermissionMode
	}
	result := sr.runner.Run(runner.RunOptions{
		Prompt:             msg.text,
		SessionID:          sess.ID,
		CWD:                sess.CWD,
		PermissionMode:     permMode,
		Model:              sess.ModelOverride,
		AppendSystemPrompt: sess.AppendSystemPrompt,
	}, onProgress)

	// Update session metadata.
	sess.Messages++
	sess.LastUsed = time.Now().Unix()
	if result.SessionID != "" {
		sess.ID = result.SessionID
	}
	if result.Usage.Model != "" {
		sess.Model = result.Usage.Model
		sess.ContextWindow = result.Usage.ContextWindow
		sess.ContextUsed = result.Usage.ContextUsed
	}
	d.store.Save()

	if result.Error != nil {
		finalText := fmt.Sprintf("❌ Ошибка: %v", result.Error)
		if progressMsgID != "" && t != nil {
			tryEdit(ctx, t, rawChatID, progressMsgID, finalText, "")
		} else {
			d.sendMessage(msg.chatID, msg.msgID, finalText)
		}
		return
	}

	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "✅ Готово."
	}

	// Convert Claude's Markdown to the transport format.
	var formatted string
	if chatFmt == "html" {
		formatted = mdhtml.Convert(text)
	} else {
		formatted = text
	}

	// Build final message: tool log + separator + answer.
	var finalText string
	if len(toolLines) > 0 {
		finalText = formatToolLines(toolLines, chatFmt) + "\n\n" + formatted
	} else {
		finalText = formatted
	}

	if t != nil {
		d.deliverFinal(ctx, t, rawChatID, progressMsgID, finalText, chatFmt)
	}
}

const maxMessageLen = 4000 // safe limit under Telegram's 4096

// splitMessage splits text into chunks that fit within the message limit.
// Splits on newlines when possible, otherwise hard-cuts.
func splitMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		cut := limit
		// Try to split on a newline.
		if idx := strings.LastIndex(text[:limit], "\n"); idx > 0 {
			cut = idx
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
		// Skip the newline we split on.
		if len(text) > 0 && text[0] == '\n' {
			text = text[1:]
		}
	}
	return chunks
}

// --- Retry logic ---

const (
	baseBackoff = 2 * time.Second
	maxBackoff  = 60 * time.Second
)

// retryDo executes fn with retries on transient/rate-limit errors.
// Rate limits and network errors retry indefinitely with backoff.
// Permanent API errors (400, 401, 403) return immediately.
// Cancelling ctx aborts the retry loop (used by /abort).
func retryDo(ctx context.Context, fn func() error) error {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := fn()
		if err == nil {
			return nil
		}

		var apiErr *transport.APIError
		if errors.As(err, &apiErr) {
			if apiErr.RetryAfter > 0 {
				wait := time.Duration(apiErr.RetryAfter) * time.Second
				log.Printf("rate limited, retry after %v: %v", wait, err)
				if !sleepCtx(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			if apiErr.IsRetryable() {
				wait := backoff(attempt)
				log.Printf("server error, retry in %v: %v", wait, err)
				if !sleepCtx(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			// Permanent API error (400, 401, 403, etc.) — don't retry.
			return err
		}

		// Network error (timeout, DNS, connection refused) — retry indefinitely.
		wait := backoff(attempt)
		log.Printf("network error, retry in %v: %v", wait, err)
		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns true if slept fully.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func backoff(attempt int) time.Duration {
	d := baseBackoff
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// trySend sends text with format, retrying on transient errors.
// Falls back to plain text if formatted send fails with a permanent error.
func trySend(ctx context.Context, t transport.Transport, chatID, replyTo, text, format string) error {
	err := retryDo(ctx, func() error {
		return t.SendMessage(chatID, text, replyTo, format)
	})
	if err != nil && format != "" && ctx.Err() == nil {
		log.Printf("send error (%s): %v, retrying plain", format, err)
		return retryDo(ctx, func() error {
			return t.SendMessage(chatID, text, replyTo, "")
		})
	}
	return err
}

// tryEdit edits text with format, retrying on transient errors.
// Falls back to plain text if formatted edit fails with a permanent error.
func tryEdit(ctx context.Context, t transport.Transport, chatID, msgID, text, format string) error {
	err := retryDo(ctx, func() error {
		return t.EditMessage(chatID, msgID, text, format)
	})
	if err != nil && format != "" && ctx.Err() == nil {
		log.Printf("edit error (%s): %v, retrying plain", format, err)
		return retryDo(ctx, func() error {
			return t.EditMessage(chatID, msgID, text, "")
		})
	}
	return err
}

// deliverFinal sends the final response, splitting into chunks if needed.
// The first chunk edits the progress message; remaining chunks are new messages.
// On total failure, attempts a last-resort plain error notification.
func (d *daemon) deliverFinal(ctx context.Context, t transport.Transport, chatID, progressMsgID, text, format string) {
	if format == "" {
		text = stripHTML(text)
	}
	chunks := splitMessage(text, maxMessageLen)

	for i, chunk := range chunks {
		if ctx.Err() != nil {
			return
		}
		var err error
		if i == 0 && progressMsgID != "" {
			err = tryEdit(ctx, t, chatID, progressMsgID, chunk, format)
		} else {
			err = trySend(ctx, t, chatID, "", chunk, format)
		}
		if err != nil {
			log.Printf("deliver error (chunk %d/%d): %v", i+1, len(chunks), err)
			// Last resort: try to notify user about the failure.
			if i == 0 && ctx.Err() == nil {
				_ = retryDo(ctx, func() error {
					return t.SendMessage(chatID, "Ошибка доставки ответа. Попробуйте /status", "", "")
				})
			}
			return
		}
	}
}

func (d *daemon) sendMessage(chatID, replyTo, text string) {
	t, raw, fmtStr := d.transportFor(chatID)
	if t == nil {
		log.Printf("no transport for %s", chatID)
		return
	}
	if fmtStr == "" {
		text = stripHTML(text)
	}
	if err := trySend(context.Background(), t, raw, replyTo, text, fmtStr); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (d *daemon) sendPlain(chatID, replyTo, text string) {
	t, raw, _ := d.transportFor(chatID)
	if t == nil {
		log.Printf("no transport for %s", chatID)
		return
	}
	if err := retryDo(context.Background(), func() error {
		return t.SendMessage(raw, text, replyTo, "")
	}); err != nil {
		log.Printf("send error: %v", err)
	}
}

// formatToolLines wraps each tool line in monospace.
func formatToolLines(lines []string, format string) string {
	var out []string
	for _, l := range lines {
		if format == "html" {
			escaped := strings.ReplaceAll(l, "&", "&amp;")
			escaped = strings.ReplaceAll(escaped, "<", "&lt;")
			escaped = strings.ReplaceAll(escaped, ">", "&gt;")
			out = append(out, "<code>"+escaped+"</code>")
		} else {
			escaped := strings.ReplaceAll(l, "`", "'")
			out = append(out, "`"+escaped+"`")
		}
	}
	return strings.Join(out, "\n")
}

var reHTMLTag = regexp.MustCompile(`<[^>]+>`)

// stripHTML removes HTML tags and unescapes entities for plain-text transports.
func stripHTML(s string) string {
	s = reHTMLTag.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

var knownModels = []struct {
	alias string
	model string // actual --model value
	label string
}{
	{"opus", "claude-opus-4-6[1m]", "Claude Opus 1M"},
	{"sonnet", "claude-sonnet-4-6[1m]", "Claude Sonnet 1M"},
	{"haiku", "claude-haiku-4-5-20251001", "Claude Haiku 200k"},
}

func (d *daemon) modelText(sess *session.Session) string {
	var sb strings.Builder
	current := sess.ModelOverride
	for _, m := range knownModels {
		if m.model == current {
			fmt.Fprintf(&sb, "/m_%s %s ✅\n", m.alias, m.label)
		} else {
			fmt.Fprintf(&sb, "/m_%s %s\n", m.alias, m.label)
		}
	}
	if current == "" {
		fmt.Fprintf(&sb, "/m_default По умолчанию ✅\n")
	} else {
		fmt.Fprintf(&sb, "/m_default По умолчанию\n")
	}
	return sb.String()
}

// --- Text helpers ---

func helpText() string {
	return `<b>klax</b> — bridge для Claude Code

<b>Команды:</b>
/status — статус
/sessions — сессии
/new [имя] — новая сессия
/name — переименовать сессию
/cleanup — управление сессиями
/cwd [путь] — рабочая директория
/model — модель (opus/sonnet/haiku)
/prompt [текст] — системный промпт
/transports — управление транспортами
/bypass — команда в Claude
/abort — прервать исполнение`
}

func (d *daemon) statusText(chatID string) string {
	sess := d.store.Active(chatID)
	if sess == nil {
		return "❌ Нет активной сессии"
	}

	sr := d.getRunner(chatID)
	sr.mu.Lock()
	qlen := len(sr.queue)
	sr.mu.Unlock()

	tool, toolElapsed, totalElapsed := sr.runner.Status()
	var statusLine string
	if sr.runner.IsBusy() {
		totalSec := int(totalElapsed.Seconds())
		if tool.Name != "" {
			toolSec := int(toolElapsed.Seconds())
			statusLine = fmt.Sprintf("🔄 %s (%ds / %ds)", tool.String(), toolSec, totalSec)
		} else {
			statusLine = fmt.Sprintf("🔄 Работает (%ds)", totalSec)
		}
		if qlen > 0 {
			statusLine += fmt.Sprintf(" 📬 %d", qlen)
		}
	} else {
		statusLine = "💤 Свободен"
	}

	var contextLine string
	if sess.ContextWindow > 0 {
		pct := sess.ContextUsed * 100 / sess.ContextWindow
		contextLine = fmt.Sprintf("\n🤖 <code>%s</code>\n📊 Контекст: %d%% (%dk/%dk)",
			sess.Model, pct,
			sess.ContextUsed/1000, sess.ContextWindow/1000)
	}

	return fmt.Sprintf(
		"<b>klax</b> v%s\n\n📌 <code>%s</code>\n%s%s\n💬 Сообщений: %d",
		version, sess.Name, statusLine, contextLine, sess.Messages,
	)
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dм назад", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dч назад", h)
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dд назад", days)
	}
}

// formatSessionLine renders one session line.
// activePrefix/inactiveCmd control per-mode differences.
func formatSessionLine(sb *strings.Builder, i int, s *session.Session, activePrefix, inactiveCmd string) {
	ctx := ""
	if s.ContextWindow > 0 {
		pct := s.ContextUsed * 100 / s.ContextWindow
		ctx = fmt.Sprintf("%d%%", pct)
	}
	if s.Active {
		detail := "активна"
		if ctx != "" {
			detail += " " + ctx
		}
		fmt.Fprintf(sb, "%s<b>/s%d</b> <code>%s</code> <b>(%s)</b> <b>%d💬</b>\n",
			activePrefix, i+1, s.Name, detail, s.Messages)
	} else {
		ago := ""
		if s.LastUsed > 0 {
			ago = timeAgo(time.Unix(s.LastUsed, 0))
		}
		var parts []string
		if ago != "" {
			parts = append(parts, ago)
		}
		if ctx != "" {
			parts = append(parts, ctx)
		}
		detail := ""
		if len(parts) > 0 {
			detail = " (" + strings.Join(parts, " ") + ")"
		}
		fmt.Fprintf(sb, "%s%d <code>%s</code>%s %d💬\n",
			inactiveCmd, i+1, s.Name, detail, s.Messages)
	}
}

func (d *daemon) cleanupText(chatID string) string {
	sessions := d.store.SessionsFor(chatID)
	if len(sessions) == 0 {
		return "Нет сессий."
	}
	var sb strings.Builder
	inactive := 0
	for i, s := range sessions {
		if !s.Active {
			inactive++
		}
		formatSessionLine(&sb, i, s, "✅ ", "❌ /d")
	}
	if inactive == 0 {
		sb.WriteString("\nНечего удалять.")
	}
	return sb.String()
}

func (d *daemon) sessionsText(chatID string) string {
	sessions := d.store.SessionsFor(chatID)
	if len(sessions) == 0 {
		return "Нет сессий. Напиши /new"
	}
	var sb strings.Builder
	for i, s := range sessions {
		formatSessionLine(&sb, i, s, "", "/s")
	}
	sb.WriteString("\n/cleanup — управление сессиями")
	return sb.String()
}
