// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package instance keeps a second relay from running on the same computer. Two
// would fight over the logger ports and could both send from one queue.
package instance

import "errors"

// ErrHeld means another relay is running.
var ErrHeld = errors.New("another relay is running")

// MutexName is the Windows mutex a running relay holds. It covers the whole computer
// and not one sign-in, because the logger ports do too. The installer looks for it.
const MutexName = `Global\FieldDayCommanderRelay`

// Acquire claims this computer for this relay, or returns ErrHeld. Call release when
// the relay stops. The system also gives the claim up if the relay crashes, so there's
// never a stale one to clean up.
func Acquire() (release func(), err error) { return acquire(MutexName) }
