// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package tray

import (
	"context"
	"testing"
	"time"
)

// A test binary has no icon built in, so it shows what any program would get if
// the resource were ever missing.
func TestAMissingIconFallsBackToTheGenericOne(t *testing.T) {
	for _, resource := range []uintptr{0, 9999} {
		if loadTrayIcon(0, resource) == 0 {
			t.Errorf("resource %d: no icon at all", resource)
		}
	}
}

func TestTheIconAppearsAndGoesAwayWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	finished := make(chan error, 1)
	go func() {
		finished <- Run(ctx, Options{Tooltip: "Field Day Commander Relay test", OpenLabel: "Open", QuitLabel: "Quit"})
	}()

	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Err() == nil {
			t.Error("Run returned before it was canceled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop after it was canceled")
	}
}

func TestItCanRunAgainAfterStopping(t *testing.T) {
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := Run(ctx, Options{Tooltip: "Field Day Commander Relay test", OpenLabel: "Open", QuitLabel: "Quit"})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
}
