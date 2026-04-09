package main

import (
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

// runFallback downloads the latest release from GitHub and installs it,
// ignoring local source. Useful as an escape hatch for local developers.
func runFallback() {
	tag, err := latestTag()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot get latest version: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("installing release %s...\n", tag)

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
