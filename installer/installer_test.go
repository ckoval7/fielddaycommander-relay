// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package installer holds the Windows installer's script, and checks it against the
// relay. Nothing would catch a mismatch until someone's update failed to close the
// relay or showed the wrong startup state.
package installer

import (
	"os"
	"strings"
	"testing"

	"github.com/ckoval7/fielddaycommander-relay/internal/autostart"
	"github.com/ckoval7/fielddaycommander-relay/internal/instance"
	"github.com/ckoval7/fielddaycommander-relay/internal/tray"
)

func TestTheScriptUsesTheRelaysNames(t *testing.T) {
	script, err := os.ReadFile("relay.nsi")
	if err != nil {
		t.Fatal(err)
	}
	for what, want := range map[string]string{
		"instance mutex":  `"` + instance.MutexName + `"`,
		"tray window":     `"` + tray.WindowClass + `"`,
		"startup entry":   `!define PRODUCT "` + autostart.EntryName + `"`,
		"startup command": `'` + autostart.CommandLine(`$INSTDIR\relay.exe`, "") + `'`,
		"startup key":     `"` + autostart.RunKey + `"`,
		"windows' switch": `"` + autostart.SwitchKey + `"`,
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("%s: the script doesn't contain %s", what, want)
		}
	}
}
