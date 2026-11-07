// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package autostart

// New returns nil: only Windows can start the relay at sign-in so far. The relay
// couldn't reach a server without Windows' keychain anyway.
func New(string, string) Starter { return nil }
