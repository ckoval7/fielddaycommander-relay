// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package autostart

import "testing"

// This uses the real registry, under its own name so a relay's own entry is safe.
func TestTheRegistryKeepsAnEntry(t *testing.T) {
	entry := starter{store: registry{name: "Field Day Commander Relay test"}, command: CommandLine(`C:\Relay Tést\relay.exe`, "")}
	t.Cleanup(func() { _ = entry.Set(false) })

	if err := entry.Set(true); err != nil {
		t.Fatal(err)
	}
	status, err := entry.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.On {
		t.Fatalf("after turning it on: %+v", status)
	}

	if err := entry.Set(false); err != nil {
		t.Fatal(err)
	}
	if status, err = entry.Status(); err != nil {
		t.Fatal(err)
	}
	if status.On || status.OtherCopy != "" {
		t.Fatalf("after turning it off: %+v", status)
	}
	if err := entry.Set(false); err != nil {
		t.Errorf("removing it twice: %v", err)
	}
}
