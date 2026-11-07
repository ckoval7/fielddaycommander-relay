// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ckoval7/fielddaycommander-relay/internal/secret"
)

const crashDirEnv = "RELAY_TEST_CRASH_DIR"

// The Windows build has no console, so without this a crash leaves no trace.
func TestACrashIsWrittenToTheCrashLogFromAnyGoroutine(t *testing.T) {
	if dir := os.Getenv(crashDirEnv); dir != "" {
		if err := captureCrashes(dir); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			panic("a test crash on another goroutine")
		}()
		<-done
		return
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, crashFile), []byte("an earlier crash\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestACrashIsWrittenToTheCrashLogFromAnyGoroutine$")
	command.Env = append(os.Environ(), crashDirEnv+"="+dir)
	if err := command.Run(); err == nil {
		t.Fatal("the helper should have crashed")
	}

	got, err := os.ReadFile(filepath.Join(dir, crashFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"an earlier crash", "a test crash on another goroutine", "goroutine"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("crash log lacks %q:\n%s", want, got)
		}
	}
}

func TestTheSavedPortIsOnlyTrustedIfItsAPort(t *testing.T) {
	dir := t.TempDir()
	if got := savedPort(dir); got != 0 {
		t.Errorf("nothing saved: got %d", got)
	}
	savePort(dir, 49152, slog.New(slog.DiscardHandler))
	if got := savedPort(dir); got != 49152 {
		t.Errorf("got %d", got)
	}
	for _, bad := range []string{"", "not a port", "0", "70000", "-1"} {
		if err := os.WriteFile(filepath.Join(dir, portFile), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := savedPort(dir); got != 0 {
			t.Errorf("%q: got %d", bad, got)
		}
	}
}

func useTemporaryConfigFolder(t *testing.T) string {
	t.Helper()
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("AppData", config)
	return config
}

func TestTheDataFolder(t *testing.T) {
	config := useTemporaryConfigFolder(t)
	usual := filepath.Join(config, dataFolder)
	folder := t.TempDir()

	// A file where the program's folder should be can't hold a data folder.
	blocked := filepath.Join(folder, "not-a-folder")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, override, program, want string
		fellBack                      bool
	}{
		{name: "installed", want: usual},
		{name: "chosen", override: filepath.Join(folder, "chosen"), want: filepath.Join(folder, "chosen")},
		{name: "portable", program: filepath.Join(folder, "relay.exe"), want: filepath.Join(folder, portableFolder)},
		{name: "chosen wins in portable", override: filepath.Join(folder, "chosen"), program: filepath.Join(folder, "relay.exe"), want: filepath.Join(folder, "chosen")},
		{name: "portable can't write", program: filepath.Join(blocked, "relay.exe"), want: usual, fellBack: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, besideProgramErr, err := resolveDataDir(tt.override, tt.program)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
			if (besideProgramErr != nil) != tt.fellBack {
				t.Errorf("besideProgramErr = %v", besideProgramErr)
			}
			if info, err := os.Stat(got); err != nil || !info.IsDir() {
				t.Errorf("the folder wasn't created: %v", err)
			}
			entries, err := os.ReadDir(got)
			if err != nil || len(entries) != 0 {
				t.Errorf("something was left behind: %v, %v", entries, err)
			}
		})
	}
}

func TestEachPortableFolderKeepsItsOwnToken(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	target, err := portableTarget(first)
	if err != nil {
		t.Fatal(err)
	}
	if target == secret.DefaultTarget {
		t.Error("shares the installed version's token")
	}
	if again, err := portableTarget(first); err != nil || again != target {
		t.Errorf("the same folder changed: %q, %v", again, err)
	}
	if other, err := portableTarget(second); err != nil || other == target {
		t.Errorf("two folders share a token: %q, %v", other, err)
	}

	// Moving the folder, as to a new drive letter, keeps the name.
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(first, moved); err != nil {
		t.Fatal(err)
	}
	if after, err := portableTarget(moved); err != nil || after != target {
		t.Errorf("moving changed it: %q, %v", after, err)
	}
}
