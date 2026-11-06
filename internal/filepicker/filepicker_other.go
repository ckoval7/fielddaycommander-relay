// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package filepicker

// Open has no window to show outside Windows.
func Open(Options) (string, error) { return "", ErrUnavailable }
