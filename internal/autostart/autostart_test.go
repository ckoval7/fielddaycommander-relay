// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package autostart

import (
	"errors"
	"testing"
)

type fakeStore struct {
	command   string
	found     bool
	off       bool
	cleared   int
	readError error
}

func (f *fakeStore) entry() (string, bool, error) { return f.command, f.found, f.readError }
func (f *fakeStore) setEntry(command string) error {
	f.command, f.found = command, true
	return nil
}
func (f *fakeStore) removeEntry() error {
	f.command, f.found = "", false
	return nil
}
func (f *fakeStore) turnedOff() (bool, error) { return f.off, nil }
func (f *fakeStore) clearSwitch() error {
	f.off = false
	f.cleared++
	return nil
}

const mine = `"C:\Radio\relay.exe" --no-browser`

func TestStatusSaysWhatTheNextSignInWillDo(t *testing.T) {
	tests := []struct {
		name  string
		store fakeStore
		want  Status
	}{
		{"no entry", fakeStore{}, Status{Available: true}},
		{"this copy", fakeStore{command: mine, found: true}, Status{Available: true, On: true}},
		{"case doesn't matter in a Windows path", fakeStore{command: `"c:\radio\RELAY.EXE" --no-browser`, found: true}, Status{Available: true, On: true}},
		{"turned off in Windows", fakeStore{command: mine, found: true, off: true}, Status{Available: true, TurnedOffInWindows: true}},
		{"another copy", fakeStore{command: `"D:\Old\relay.exe" --no-browser`, found: true}, Status{Available: true, OtherCopy: `D:\Old\relay.exe`}},
		{"this copy with another data folder", fakeStore{command: mine + ` --data-dir "D:\Test"`, found: true}, Status{Available: true, OtherCopy: `C:\Radio\relay.exe`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := starter{store: &tt.store, command: mine}.Status()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAReadFailureIsReported(t *testing.T) {
	failure := errors.New("no access")
	if _, err := (starter{store: &fakeStore{readError: failure}, command: mine}).Status(); !errors.Is(err, failure) {
		t.Errorf("got %v", err)
	}
}

func TestTurningItOnReplacesAnotherCopyAndWindowsSwitch(t *testing.T) {
	store := &fakeStore{command: `"D:\Old\relay.exe" --no-browser`, found: true, off: true}
	entry := starter{store: store, command: mine}

	if err := entry.Set(true); err != nil {
		t.Fatal(err)
	}
	status, err := entry.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.On || store.command != mine {
		t.Errorf("status %+v, command %q", status, store.command)
	}
}

func TestTurningItOffLeavesNothingBehind(t *testing.T) {
	store := &fakeStore{command: mine, found: true, off: true}
	if err := (starter{store: store, command: mine}).Set(false); err != nil {
		t.Fatal(err)
	}
	if store.found || store.cleared != 1 {
		t.Errorf("%+v", store)
	}
}

func TestTheCommandLine(t *testing.T) {
	tests := []struct{ program, dataDir, want string }{
		{`C:\Program Files\Relay\relay.exe`, "", `"C:\Program Files\Relay\relay.exe" --no-browser`},
		{`C:\relay.exe`, `D:\My Data`, `"C:\relay.exe" --no-browser --data-dir "D:\My Data"`},
		// A lone backslash before the closing quote would escape it.
		{`C:\relay.exe`, `D:\`, `"C:\relay.exe" --no-browser --data-dir "D:\\"`},
	}
	for _, tt := range tests {
		if got := CommandLine(tt.program, tt.dataDir); got != tt.want {
			t.Errorf("got %s, want %s", got, tt.want)
		}
	}
}

func TestProgramIn(t *testing.T) {
	for command, want := range map[string]string{
		`"C:\A B\relay.exe" --no-browser`: `C:\A B\relay.exe`,
		`C:\relay.exe`:                    `C:\relay.exe`,
		`"C:\unclosed`:                    `"C:\unclosed`,
	} {
		if got := programIn(command); got != want {
			t.Errorf("%s: got %s, want %s", command, got, want)
		}
	}
}
