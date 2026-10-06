package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCopyFilePreservesBinaryOnWriteFailure(t *testing.T) {
	if dir := os.Getenv("KLAX_INSTALL_FAIL_TEST"); dir != "" {
		signal.Ignore(syscall.SIGXFSZ)
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 16, Max: 16}); err != nil {
			t.Fatal(err)
		}
		if err := copyFile(filepath.Join(dir, "source"), filepath.Join(dir, "binary"), 0755); err == nil {
			t.Fatal("copy succeeded beyond the file-size limit")
		}
		data, err := os.ReadFile(filepath.Join(dir, "binary"))
		if err != nil || string(data) != "OLD" {
			t.Fatalf("installed binary damaged: %q, %v", data, err)
		}
		return
	}
	dir := t.TempDir()
	for name, data := range map[string]string{"source": strings.Repeat("NEW", 32), "binary": "OLD"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCopyFilePreservesBinaryOnWriteFailure$")
	cmd.Env = append(os.Environ(), "KLAX_INSTALL_FAIL_TEST="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write-failure subprocess: %v\n%s", err, out)
	}
	if err := copyFile(filepath.Join(dir, "source"), filepath.Join(dir, "binary"), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "binary"))
	if err != nil || string(data) != strings.Repeat("NEW", 32) {
		t.Fatalf("replacement = %q, %v", data, err)
	}
	info, err := os.Stat(filepath.Join(dir, "binary"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("installed mode: %v, %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("staging files leaked: %v, %v", entries, err)
	}
}

func TestShellInstallerReplacesBinaryOnlyAfterDownload(t *testing.T) {
	const newBinary = "#!/bin/sh\necho 'klax 9.9.9'\n"
	for _, mode := range []string{"interrupted", "complete"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "stubs")
			installDir := filepath.Join(home, ".local", "bin")
			for _, dir := range []string{bin, installDir} {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			binary := filepath.Join(installDir, "klax")
			if err := os.WriteFile(binary, []byte("OLD"), 0755); err != nil {
				t.Fatal(err)
			}
			curl := `#!/bin/sh
if [ "$1" = "-sfI" ]; then
  printf 'location: https://github.com/example/releases/tag/v9.9.9\r\n'
  exit 0
fi
while [ "$1" != "-o" ]; do shift; done
download_path="$2"
if [ "$KLAX_INSTALL_TEST_MODE" = "interrupted" ]; then
  printf PARTIAL > "$download_path"
  exit 18
fi
cat > "$download_path" <<'BINARY'
#!/bin/sh
echo 'klax 9.9.9'
BINARY
`
			for name, body := range map[string]string{"curl": curl, "loginctl": "#!/bin/sh\necho Linger=yes\n"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("PATH", bin+":"+installDir+":/usr/bin:/bin")
			t.Setenv("SHELL", "/bin/bash")
			t.Setenv("KLAX_INSTALL_TEST_MODE", mode)
			out, err := exec.Command("bash", "../../install.sh").CombinedOutput()
			if (err != nil) != (mode == "interrupted") {
				t.Fatalf("installer: %v\n%s", err, out)
			}
			want := newBinary
			if mode == "interrupted" {
				want = "OLD"
			}
			data, err := os.ReadFile(binary)
			if err != nil || string(data) != want {
				t.Fatalf("installed binary = %q, want %q, err = %v", data, want, err)
			}
			info, err := os.Stat(binary)
			if err != nil || info.Mode().Perm() != 0755 {
				t.Fatalf("installed binary mode: %v, %v", info, err)
			}
			entries, err := os.ReadDir(installDir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "klax" {
				t.Fatalf("staging files left behind: %v, %v", entries, err)
			}
		})
	}
}

func TestRenderServiceUnitPlacesStartLimitInUnitSection(t *testing.T) {
	unit := renderServiceUnit("/home/test/.local/bin/klax")

	want := "[Unit]\nDescription=klax — AI messaging bridge\nAfter=network.target\nStartLimitBurst=3\nStartLimitIntervalSec=60\n\n[Service]\nType=simple\nExecStart=/home/test/.local/bin/klax start --foreground\nRestart=always\nRestartSec=5\n"
	if !strings.Contains(unit, "OOMPolicy=continue") {
		t.Fatalf("service unit must keep the daemon alive across member OOM kills:\n%s", unit)
	}
	if !strings.Contains(unit, want) {
		t.Fatalf("service unit missing expected structure:\n%s", unit)
	}
	if strings.Contains(unit, "[Service]\nType=simple\nExecStart=/home/test/.local/bin/klax start --foreground\nRestart=always\nRestartSec=5\nStartLimitBurst=3") {
		t.Fatalf("start limit settings must not be in [Service]:\n%s", unit)
	}
}

func TestUnitDriftedDetectsDifferentContent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/klax.service"
	if err := os.WriteFile(path, []byte("broken"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	drifted, err := unitDrifted(path, "expected")
	if err != nil {
		t.Fatalf("unitDrifted error: %v", err)
	}
	if !drifted {
		t.Fatalf("expected drift to be detected")
	}
}

func TestUnitDriftedIgnoresMissingFile(t *testing.T) {
	drifted, err := unitDrifted("/no/such/file", "expected")
	if err != nil {
		t.Fatalf("unitDrifted error: %v", err)
	}
	if drifted {
		t.Fatalf("missing file should not count as drift")
	}
}

func TestIgnorableVerifyError(t *testing.T) {
	if !ignorableVerifyError(fmt.Errorf("SO_PASSCRED failed: Operation not permitted")) {
		t.Fatalf("expected sandbox verification error to be ignorable")
	}
	if !ignorableVerifyError(fmt.Errorf("systemd-analyze not found")) {
		t.Fatalf("expected a systemd-less environment (e.g. a container) to be ignorable, not fatal")
	}
	if ignorableVerifyError(fmt.Errorf("syntax error in unit")) {
		t.Fatalf("real verification errors must stay fatal")
	}
}
