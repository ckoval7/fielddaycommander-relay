// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package logfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

var (
	eventStart = time.Date(2026, 6, 27, 18, 0, 0, 0, time.UTC)
	eventEnd   = time.Date(2026, 6, 28, 21, 0, 0, 0, time.UTC)
)

const header = "Written by a logger\n<ADIF_VER:5>3.1.4 <EOH>\n"

// qso writes a record for a call, minutes into the event.
func qso(call string, minutes int) string {
	when := eventStart.Add(time.Duration(minutes) * time.Minute)
	return fmt.Sprintf("<CALL:%d>%s <QSO_DATE:8>%s <TIME_ON:6>%s <BAND:3>20m <MODE:3>FT8 <EOR>\n",
		len(call), call, when.Format("20060102"), when.Format("150405"))
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// fixture is a watcher on a real queue, checked by hand, not on a timer.
type fixture struct {
	t       *testing.T
	path    string
	queue   *queue.Queue
	watcher *Watcher
	wakes   int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	q, err := queue.Open(filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	if err := q.SetEventPeriod(context.Background(), eventStart, eventEnd); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, path: filepath.Join(dir, "log.adi"), queue: q}
	f.restart()
	return f
}

func (f *fixture) restart() {
	f.watcher = New(f.path, f.queue, func() { f.wakes++ }, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *fixture) check() {
	f.t.Helper()
	if err := f.watcher.check(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// queued returns the calls waiting to send, oldest first.
func (f *fixture) queued() []string {
	f.t.Helper()
	items, err := f.queue.Next(context.Background(), 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	calls := make([]string, 0, len(items))
	for _, item := range items {
		calls = append(calls, item.Contact.Callsign)
	}
	return calls
}

func (f *fixture) wantQueued(want ...string) {
	f.t.Helper()
	got := f.queued()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		f.t.Errorf("queued %v, want %v", got, want)
	}
}

func (f *fixture) wantStatus(state State, contacts, outside int) {
	f.t.Helper()
	got := f.watcher.Status()
	if got.State != state || got.Contacts != contacts || got.Outside != outside {
		f.t.Errorf("got %+v, want %s, %d contacts, %d outside", got, state, contacts, outside)
	}
}

func TestContactsInTheFileAreQueued(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("K1ABC", 1)+qso("W2XYZ", 2))
	f.check()
	f.wantQueued("K1ABC", "W2XYZ")
	f.wantStatus(Watching, 2, 0)
	if f.wakes != 1 {
		t.Errorf("woke the sender %d times", f.wakes)
	}
	if f.watcher.Status().LastRead.IsZero() {
		t.Error("no last read time")
	}
}

func TestNewContactsAreReadAsTheyAreAdded(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("K1ABC", 1))
	f.check()
	appendFile(t, f.path, qso("W2XYZ", 2))
	f.check()
	f.wantQueued("K1ABC", "W2XYZ")
	f.wantStatus(Watching, 2, 0)
}

func TestEverySplitOfARecordReadsTheSame(t *testing.T) {
	record := qso("W2XYZ", 2)
	for split := 1; split < len(record)-1; split++ {
		f := newFixture(t)
		writeFile(t, f.path, header+record[:split])
		f.check()
		f.wantQueued()
		appendFile(t, f.path, record[split:])
		f.check()
		f.wantQueued("W2XYZ")
		if t.Failed() {
			t.Fatalf("split at %d", split)
		}
	}
}

func TestRestartingQueuesNothingNew(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("K1ABC", 1))
	f.check()
	f.restart()
	f.check()
	f.wantQueued("K1ABC")
	f.wantStatus(Watching, 1, 0)
	if f.wakes != 1 {
		t.Errorf("woke the sender %d times", f.wakes)
	}
}

func TestAContactLoggedWhileTheRelayWasClosedIsPickedUp(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("K1ABC", 1))
	f.check()
	appendFile(t, f.path, qso("W2XYZ", 2))
	f.restart()
	f.check()
	f.wantQueued("K1ABC", "W2XYZ")
}

func TestContactsOutsideTheEventAreNotSent(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("BEFORE", -1)+qso("FIRST", 0)+qso("LAST", int(eventEnd.Sub(eventStart).Minutes()))+qso("AFTER", 100000))
	f.check()
	f.wantQueued("FIRST", "LAST")
	f.wantStatus(Watching, 2, 2)
}

func TestNewEventDatesReadTheFileAgain(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("K1ABC", 1)+qso("LATE", 100000))
	f.check()
	f.wantQueued("K1ABC")

	if err := f.queue.SetEventPeriod(context.Background(), eventStart, eventEnd.Add(24*365*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.check()
	f.wantQueued("K1ABC", "LATE")
	f.wantStatus(Watching, 2, 0)
}

func TestNothingIsReadUntilTheEventIsKnown(t *testing.T) {
	f := newFixture(t)
	if err := f.queue.ForgetEventPeriod(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.path, header+qso("K1ABC", 1))
	f.check()
	f.wantQueued()
	f.wantStatus(WaitingForEvent, 0, 0)

	if err := f.queue.SetEventPeriod(context.Background(), eventStart, eventEnd); err != nil {
		t.Fatal(err)
	}
	f.check()
	f.wantQueued("K1ABC")
	f.wantStatus(Watching, 1, 0)
}

func TestARewrittenFileIsReadAgainWithoutRepeats(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("K1ABC", 1)+qso("W2XYZ", 2))
	f.check()

	// A longer file with different text at the old position, as when a logger sorts
	// newest first.
	writeFile(t, f.path, header+qso("N3NEW", 3)+qso("W2XYZ", 2)+qso("K1ABC", 1))
	f.check()
	f.wantQueued("K1ABC", "W2XYZ", "N3NEW")
	f.wantStatus(Watching, 3, 0)
}

func TestAShorterFileDeletesNothing(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+qso("K1ABC", 1)+qso("W2XYZ", 2))
	f.check()
	writeFile(t, f.path, header+qso("W2XYZ", 2))
	f.check()
	f.wantQueued("K1ABC", "W2XYZ")
	f.wantStatus(Watching, 1, 0)
	items, err := f.queue.Next(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Contact.Deleted {
			t.Errorf("%s was deleted", item.Contact.Callsign)
		}
	}
}

func TestACorrectionInTheFileReachesTheQueue(t *testing.T) {
	f := newFixture(t)
	before := strings.Replace(qso("K1ABC", 1), "<EOR>", "<NAME:3>Ann <EOR>", 1)
	after := strings.Replace(qso("K1ABC", 1), "<EOR>", "<NAME:3>Bea <EOR>", 1)
	writeFile(t, f.path, header+before)
	f.check()
	writeFile(t, f.path, header+after)
	// The same size, so only the file's time can give it away.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(f.path, later, later); err != nil {
		t.Fatal(err)
	}
	f.check()

	counts, err := f.queue.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 2 {
		t.Errorf("%d pending, want the original and its correction", counts.Pending)
	}
}

func TestABadRecordIsKeptOnceAndDoesNotStopTheRest(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, header+"<CALL:5>NOTIME<EOR>\n"+qso("BAD CALL", 1)+qso("K1ABC", 2))
	f.check()
	f.restart()
	f.check()
	f.wantQueued("K1ABC")
	f.wantStatus(Watching, 1, 0)

	counts, err := f.queue.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Invalid != 2 {
		t.Errorf("%d invalid, want 2", counts.Invalid)
	}
}

func TestAMissingFileIsWatchedForUntilItAppears(t *testing.T) {
	f := newFixture(t)
	f.check()
	f.wantStatus(Missing, 0, 0)
	writeFile(t, f.path, header+qso("K1ABC", 1))
	f.check()
	f.wantQueued("K1ABC")
	f.wantStatus(Watching, 1, 0)

	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.check()
	f.wantStatus(Missing, 0, 0)
	f.wantQueued("K1ABC")
}

func TestAFileThatIsNotADIFIsNamed(t *testing.T) {
	f := newFixture(t)
	writeFile(t, f.path, "SQLite format 3\x00 and so on")
	f.check()
	f.wantStatus(NotADIF, 0, 0)

	writeFile(t, f.path, "")
	f.check()
	f.wantStatus(Watching, 0, 0)
}

func TestAFolderIsUnreadable(t *testing.T) {
	f := newFixture(t)
	f.path = t.TempDir()
	f.restart()
	f.check()
	f.wantStatus(Unreadable, 0, 0)
}

func TestTextWithNoRecordEndIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.watcher.window = 512
	writeFile(t, f.path, header+qso("K1ABC", 1)+strings.Repeat("x", 4000)+"<EOR>"+qso("W2XYZ", 2))
	f.check()
	f.wantQueued("K1ABC", "W2XYZ")
}

func TestALargeLogIsReadAcrossWindows(t *testing.T) {
	f := newFixture(t)
	f.watcher.window = 256
	var text strings.Builder
	text.WriteString(header)
	var want []string
	for i := range 40 {
		call := fmt.Sprintf("K%dABC", i)
		want = append(want, call)
		text.WriteString(qso(call, i))
	}
	writeFile(t, f.path, text.String())
	f.check()
	f.wantQueued(want...)
	f.wantStatus(Watching, 40, 0)
}

// failingSink fails the first Put, as a full disk would.
type failingSink struct {
	*queue.Queue
	fail bool
}

func (s *failingSink) Put(ctx context.Context, c contact.Contact) (bool, error) {
	if s.fail {
		s.fail = false
		return false, errors.New("disk full")
	}
	return s.Queue.Put(ctx, c)
}

func TestAFailedSaveIsRetried(t *testing.T) {
	f := newFixture(t)
	sink := &failingSink{Queue: f.queue, fail: true}
	f.watcher = New(f.path, sink, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	writeFile(t, f.path, header+qso("K1ABC", 1)+qso("W2XYZ", 2))

	if err := f.watcher.check(context.Background()); err == nil {
		t.Fatal("expected the failure to be reported")
	}
	f.wantQueued()
	f.check()
	f.wantQueued("K1ABC", "W2XYZ")
	f.wantStatus(Watching, 2, 0)
}

func TestRunWatchesUntilCanceled(t *testing.T) {
	f := newFixture(t)
	f.watcher.poll = time.Millisecond
	writeFile(t, f.path, header+qso("K1ABC", 1))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.watcher.Run(ctx)
	}()

	deadline := time.After(10 * time.Second)
	for f.watcher.Status().Contacts != 1 {
		select {
		case <-deadline:
			t.Fatal("never read the file")
		case <-time.After(time.Millisecond):
		}
	}
	appendFile(t, f.path, qso("W2XYZ", 2))
	for f.watcher.Status().Contacts != 2 {
		select {
		case <-deadline:
			t.Fatal("never read the addition")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("didn't stop")
	}
}
