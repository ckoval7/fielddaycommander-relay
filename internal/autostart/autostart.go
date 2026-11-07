// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package autostart starts the relay when the person signs in to the computer.
//
// It's sign-in and not boot on purpose: the token is in that person's keychain, and
// the tray icon and file window need their desktop.
//
// The system's own list is the only record of the choice, so the page can't
// disagree with what will really happen at the next sign-in.
package autostart

import "strings"

const (
	// EntryName is the relay's entry in the list of what starts at sign-in.
	EntryName = "Field Day Commander Relay"
	// RunKey needs no administrator, and only affects the person who asked.
	RunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	// SwitchKey is where Task Manager and Settings keep their on and off switches.
	// Windows doesn't document it. The first byte of a value is odd when it's off.
	SwitchKey = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
)

// Status is what will happen at the next sign-in.
type Status struct {
	// Available is whether this system and build can start the relay at sign-in.
	Available bool
	// On is whether this copy of the relay will start.
	On bool
	// OtherCopy is the program that will start instead of this one. It's left alone
	// until someone asks for this copy, since either could be the one they meant.
	OtherCopy string
	// TurnedOffInWindows is whether someone turned the relay off in Windows' list of
	// startup apps, which keeps it from starting though its entry is still there.
	TurnedOffInWindows bool
}

// Starter is a program's entry in the list of what starts at sign-in.
type Starter interface {
	Status() (Status, error)
	Set(on bool) error
}

// store is the system's list of what starts at sign-in.
type store interface {
	// entry returns the saved command line, if there is one.
	entry() (command string, found bool, err error)
	setEntry(command string) error
	removeEntry() error
	// turnedOff is whether the system's own switch for the entry is off.
	turnedOff() (bool, error)
	// clearSwitch forgets the system's own switch for the entry.
	clearSwitch() error
}

type starter struct {
	store   store
	command string
}

func (s starter) Status() (Status, error) {
	command, found, err := s.store.entry()
	if err != nil {
		return Status{}, err
	}
	status := Status{Available: true}
	if !found {
		return status, nil
	}
	// Windows paths ignore case, and what the program calls itself can vary with
	// how it was started.
	if !strings.EqualFold(command, s.command) {
		status.OtherCopy = programIn(command)
		return status, nil
	}
	off, err := s.store.turnedOff()
	if err != nil {
		return Status{}, err
	}
	status.TurnedOffInWindows = off
	status.On = !off
	return status, nil
}

// Set adds or removes the entry. Asking for it here outranks an earlier choice in
// Windows' list, or the checkbox could never turn it back on.
func (s starter) Set(on bool) error {
	if on {
		if err := s.store.setEntry(s.command); err != nil {
			return err
		}
	} else if err := s.store.removeEntry(); err != nil {
		return err
	}
	return s.store.clearSwitch()
}

// CommandLine is what the system runs at sign-in. The relay starts quietly in the
// tray, because a browser opening at every sign-in would be in the way. dataDir is
// empty unless the relay was told to use another folder, and has to be absolute,
// since a program started at sign-in doesn't begin in any useful folder.
func CommandLine(program, dataDir string) string {
	command := quote(program) + " --no-browser"
	if dataDir != "" {
		command += " --data-dir " + quote(dataDir)
	}
	return command
}

// quote wraps a Windows path for a command line. Paths can't contain a quote, but
// one can end in a backslash, which would escape the closing quote unless doubled.
func quote(path string) string {
	trimmed := strings.TrimRight(path, `\`)
	return `"` + path + strings.Repeat(`\`, len(path)-len(trimmed)) + `"`
}

// programIn is the program a command line runs.
func programIn(command string) string {
	if rest, ok := strings.CutPrefix(command, `"`); ok {
		if program, _, closed := strings.Cut(rest, `"`); closed {
			return program
		}
	}
	return command
}
