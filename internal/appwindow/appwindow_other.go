// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package appwindow

// Show has no window to offer outside Windows.
func Show(string, string) error { return ErrUnavailable }
