// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package appwindow shows the status page in a window of its own. A browser can only
// be told to open an address, which adds a tab every time. A window with no tabs can
// be found again and brought forward.
package appwindow

import (
	"errors"
	"path/filepath"
)

// ErrUnavailable means this computer can't show the page in its own window, so it's
// a job for the default browser.
var ErrUnavailable = errors.New("no way to show the page in its own window")

// edgeCandidates is where Edge may be installed, as absolute paths so that nothing in
// the current folder or on the search path can stand in for it.
func edgeCandidates(getenv func(string) string) []string {
	var paths []string
	for _, variable := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		if folder := getenv(variable); filepath.IsAbs(folder) {
			paths = append(paths, filepath.Join(folder, "Microsoft", "Edge", "Application", "msedge.exe"))
		}
	}
	return paths
}
