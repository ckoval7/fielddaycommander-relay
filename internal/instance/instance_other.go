// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package instance

// acquire always succeeds. Other systems only have the check for a relay already
// answering on the status page's port.
func acquire(string) (func(), error) { return func() {}, nil }
