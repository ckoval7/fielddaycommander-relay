// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package instance

import (
	"errors"
	"testing"
)

// This uses its own name, so a relay running on the same computer doesn't matter.
func TestOnlyOneRelayHoldsTheClaim(t *testing.T) {
	const name = `Global\FieldDayCommanderRelayTest`

	release, err := acquire(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(name); !errors.Is(err, ErrHeld) {
		t.Errorf("second claim: got %v", err)
	}

	release()
	again, err := acquire(name)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}
