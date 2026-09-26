package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultStorageDirectory(t *testing.T) {
	t.Setenv("TOKSCOPE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if got, want := homeDir(), filepath.Join(home, ".tokscoop"); got != want {
		t.Fatalf("homeDir() = %q, want %q", got, want)
	}
	custom := t.TempDir()
	t.Setenv("TOKSCOPE_HOME", custom)
	if got := homeDir(); got != custom {
		t.Fatalf("custom homeDir() = %q, want %q", got, custom)
	}
}

func TestLocalBuildSetupFromAnotherDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := filepath.Join(t.TempDir(), "user's build $files")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "tokscoop")
	script := "#!/bin/sh\n[ \"$1\" = env ] && [ \"$2\" = --listen ] && [ \"$3\" = 127.0.0.1:18899 ] && [ \"$4\" = --shell ] && [ \"$5\" = sh ] || exit 1\nprintf \"export SETUP_RESULT='ok'\\n\"\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, executable)
	if err != nil {
		t.Fatal(err)
	}
	command := envSetupCommand(relative, "sh", "127.0.0.1:18899")
	cmd := exec.Command("sh", "-c", command+"\nprintf '%s' \"$SETUP_RESULT\"")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "ok" {
		t.Fatalf("setup command failed: %v, output %q, command %s", err, output, command)
	}
}

func TestInstalledSetupUsesCommandName(t *testing.T) {
	command := envSetupCommand("tokscoop", "sh", "")
	if !strings.HasPrefix(command, `eval "$(tokscoop env`) {
		t.Fatalf("unexpected setup command: %s", command)
	}
}

func TestPrintedExportsWorkInShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	// Paths may contain shell metacharacters; copying exports must preserve them.
	want := "/tmp/user's certs/$(printf injected)/ca.pem"
	var script bytes.Buffer
	writeEnv(&script, "sh", []envVar{{"NODE_EXTRA_CA_CERTS", want}})
	script.WriteString("printf '%s' \"$NODE_EXTRA_CA_CERTS\"\n")
	cmd := exec.Command("sh", "-c", script.String())
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("exported path = %q, want %q", got, want)
	}
}
