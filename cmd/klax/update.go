package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"

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
		goInstall.Env = append(os.Environ(), "GOPROXY=direct")
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
