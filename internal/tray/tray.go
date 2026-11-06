// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package tray shows the relay's icon in the system tray.
package tray

// Options configures the tray icon.
type Options struct {
	// IconResource is the ID of the icon built into the program. Without one,
	// Windows' generic icon shows.
	IconResource uintptr
	Tooltip      string
	OpenLabel    string
	QuitLabel    string
	// OnOpen runs when the person asks to see the status page.
	OnOpen func()
	// OnQuit runs when the person asks to quit.
	OnQuit func()
}
