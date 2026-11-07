// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package backoff works out how long to wait before trying the server again.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Wait returns the wait after a number of failures in a row, doubling from first
// up to limit.
func Wait(failures int, first, limit time.Duration) time.Duration {
	wait := first
	for i := 1; i < failures && wait < limit; i++ {
		wait *= 2
	}
	wait = min(wait, limit)
	// Up to 20% extra, so a club's relays don't all retry at once.
	return wait + time.Duration(rand.Int64N(int64(wait)/5+1))
}
