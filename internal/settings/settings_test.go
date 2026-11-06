// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestANewInstallHasEmptySettings(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != (Settings{}) {
		t.Errorf("got %+v", got)
	}
}

func TestSettingsSurviveSaving(t *testing.T) {
	dir := t.TempDir()
	want := Settings{ServerURL: "https://logs.example.org", PinSHA256: "abc", RelayName: "Tent", LoggerAddress: "192.168.1.5:1000"}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("left temporary files behind: %v", entries)
	}
}

func TestDamagedSettingsAreAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected an error")
	}
}
