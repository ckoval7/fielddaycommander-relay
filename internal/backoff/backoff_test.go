// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package backoff

import (
	"testing"
	"time"
)

func TestWaitDoublesUpToTheLimitWithUpToAFifthExtra(t *testing.T) {
	const first, limit = 2 * time.Second, 5 * time.Minute

	tests := []struct {
		failures int
		base     time.Duration
	}{
		{0, first},
		{1, first},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{8, 256 * time.Second},
		{9, limit},
		{1_000_000, limit},
	}
	for _, test := range tests {
		// The extra is random, so one try could pass by luck.
		for range 200 {
			got := Wait(test.failures, first, limit)
			if got < test.base || got > test.base+test.base/5 {
				t.Fatalf("after %d failures, waited %v, want %v plus up to a fifth", test.failures, got, test.base)
			}
		}
	}
}

func TestWaitAddsSomethingExtra(t *testing.T) {
	first := Wait(1, time.Second, time.Minute)
	for range 200 {
		if Wait(1, time.Second, time.Minute) != first {
			return
		}
	}
	t.Error("every wait was the same, so a club's relays would retry together")
}
