// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/ckoval7/fielddaycommander-relay/internal/autostart"
)

type fakeStarter struct {
	status autostart.Status
	err    error
	set    []bool
}

func (f *fakeStarter) Status() (autostart.Status, error) { return f.status, f.err }
func (f *fakeStarter) Set(on bool) error {
	f.set = append(f.set, on)
	return f.err
}

func newAutostartApp(t *testing.T, starter autostart.Starter) *App {
	t.Helper()
	a, err := New(Config{DataDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Autostart: starter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.queue.Close() })
	return a
}

func TestABuildThatCantStartAtSignInDoesntOfferIt(t *testing.T) {
	a := newAutostartApp(t, nil)
	if got := a.StartAtLogin(); got.Available {
		t.Errorf("got %+v", got)
	}
	if err := a.SetStartAtLogin(true); err == nil {
		t.Error("turning it on should fail")
	}
}

func TestStartingAtSignInReachesTheSystem(t *testing.T) {
	starter := &fakeStarter{status: autostart.Status{Available: true, On: true}}
	a := newAutostartApp(t, starter)

	if got := a.StartAtLogin(); !got.On {
		t.Errorf("got %+v", got)
	}
	if err := a.SetStartAtLogin(false); err != nil {
		t.Fatal(err)
	}
	if len(starter.set) != 1 || starter.set[0] {
		t.Errorf("set %v", starter.set)
	}
}

func TestAStartupEntryThatCantBeReadIsntOffered(t *testing.T) {
	a := newAutostartApp(t, &fakeStarter{status: autostart.Status{Available: true}, err: errors.New("no access")})
	if got := a.StartAtLogin(); got.Available {
		t.Errorf("got %+v", got)
	}
}
