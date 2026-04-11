package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
)

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

const repo = "PiDmitrius/klax"

// latestTag returns the latest release tag (e.g. "v1.2.3") via GitHub redirect.
func latestTag() (string, error) {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Head("https://github.com/" + repo + "/releases/latest")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no releases found")
	}
	parts := strings.Split(loc, "/")
	tag := parts[len(parts)-1]
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("unexpected tag format: %s", tag)
	}
	return tag, nil
}

// downloadRelease downloads the release binary for the current platform.
func downloadRelease(tag string) (string, error) {
	arch := runtime.GOARCH
	name := fmt.Sprintf("klax-%s-linux-%s", tag, arch)
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, name)

	fmt.Printf("downloading %s...\n", name)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	tmp, err := os.CreateTemp("", "klax-update-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	os.Chmod(tmp.Name(), 0755)
	return tmp.Name(), nil
}

func runUpdate() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot load config: %v\nRun 'klax setup' first.\n", err)
		os.Exit(1)
	}

	srcDir := cfg.SourceDir
	if srcDir == "" {
		// No local source — download latest release.
		tag, err := latestTag()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot get latest version: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("latest: %s (current: %s)\n", tag, version)

		binPath, err := downloadRelease(tag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "download failed: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(binPath)

		// Run install from the downloaded binary.
		install := exec.Command(binPath, "install")
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

	// Install (copies binary to ~/.local/bin/, updates service, writes restart marker)
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

// parseVersion extracts major, minor, patch from "v0.4.39" or "0.5.11".
func parseVersion(s string) (major, minor, patch int, ok bool) {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return
	}
	major, e1 := strconv.Atoi(parts[0])
	minor, e2 := strconv.Atoi(parts[1])
	patch, e3 := strconv.Atoi(parts[2])
	ok = e1 == nil && e2 == nil && e3 == nil
	return
}

// versionLess returns true if a < b.
func versionLess(a, b string) bool {
	a1, a2, a3, ok1 := parseVersion(a)
	b1, b2, b3, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return a < b
	}
	if a1 != b1 {
		return a1 < b1
	}
	if a2 != b2 {
		return a2 < b2
	}
	return a3 < b3
}

type releaseInfo struct {
	Tag         string `json:"tag_name"`
	PublishedAt string `json:"published_at"` // "2026-04-09T11:17:22Z"
	URL         string `json:"html_url"`
}

// fetchReleases returns releases sorted descending (newest first).
func fetchReleases() ([]releaseInfo, error) {
	var all []releaseInfo
	for page := 1; page <= 10; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100&page=%d", repo, page)
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var releases []releaseInfo
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, err
		}
		if len(releases) == 0 {
			break
		}
		all = append(all, releases...)
	}
	return all, nil
}

// releaseAge formats "2026-04-09T11:17:22Z" as relative time using timeAgo.
func releaseAge(published string) string {
	t, err := time.Parse(time.RFC3339, published)
	if err != nil {
		return ""
	}
	return timeAgo(t)
}

// updateText returns a menu: build from source + available release versions.
func updateText() (string, error) {
	releases, err := fetchReleases()
	if err != nil {
		return "", err
	}

	current := "v" + version
	var sb strings.Builder
	fmt.Fprintf(&sb, "Текущая: %s\n\n", current)
	limit := 10
	if len(releases) < limit {
		limit = len(releases)
	}
	for _, r := range releases[:limit] {
		alias := strings.ReplaceAll(strings.TrimPrefix(r.Tag, "v"), ".", "_")
		date := releaseAge(r.PublishedAt)
		mark := ""
		if r.Tag == current {
			mark = " ✅"
		}
		fmt.Fprintf(&sb, "/v_%s <a href=\"%s\">%s</a> %s%s\n", alias, r.URL, r.Tag, date, mark)
	}
	return sb.String(), nil
}

// tagFromAlias converts "0_4_39" back to "v0.4.39".
func tagFromAlias(alias string) string {
	return "v" + strings.ReplaceAll(alias, "_", ".")
}

// runFallback installs a specific release version.
func runFallback() {
	// Called from CLI: klax fallback [tag]
	// Without args, just print available versions.
	tag := ""
	if len(os.Args) > 2 {
		tag = os.Args[2]
	}
	if tag == "" {
		text, err := updateText()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot list releases: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(text)
		return
	}

	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	fmt.Printf("installing %s...\n", tag)

	binPath, err := downloadRelease(tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "download failed: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(binPath)

	install := exec.Command(binPath, "install")
	install.Stdout = os.Stdout
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("daemon will restart via marker")
}
