// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package appwindow

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestEdgeIsOnlyLookedForInAbsoluteFolders(t *testing.T) {
	root := t.TempDir()
	settings := map[string]string{
		"ProgramFiles(x86)": root,
		"ProgramFiles":      "relative",
		"LocalAppData":      "",
	}
	got := edgeCandidates(func(name string) string { return settings[name] })
	want := []string{filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe")}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
