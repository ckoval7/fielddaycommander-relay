// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestCredentialManagerRoundTrip(t *testing.T) {
	store := New(fmt.Sprintf("Field Day Commander Relay/test-%d", os.Getpid()))
	t.Cleanup(func() { _ = store.Delete() })

	if _, err := store.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store: %v", err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("deleting nothing: %v", err)
	}

	for _, value := range []string{"fdca_first", "fdca_second_value_that_is_longer"} {
		if err := store.Set(value); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get()
		if err != nil {
			t.Fatal(err)
		}
		if got != value {
			t.Errorf("got %d bytes, want %d", len(got), len(value))
		}
	}

	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := store.Set(""); err == nil {
		t.Error("stored an empty secret")
	}
}
