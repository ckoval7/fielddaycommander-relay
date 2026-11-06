// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
