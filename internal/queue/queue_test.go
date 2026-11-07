// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

func open(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(filepath.Join(t.TempDir(), "relay #1 queue?.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func normalized(t *testing.T, call string) contact.Contact {
	t.Helper()
	c, _, err := contact.Normalize(contact.Contact{Callsign: call, QSOTime: "2026-06-27T18:05:00Z", Band: "20m", Mode: "SSB"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func put(t *testing.T, q *Queue, c contact.Contact) bool {
	t.Helper()
	queued, err := q.Put(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return queued
}

func next(t *testing.T, q *Queue) []Item {
	t.Helper()
	items, err := q.Next(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func apply(t *testing.T, q *Queue, outcomes ...Outcome) {
	t.Helper()
	if err := q.Apply(context.Background(), outcomes); err != nil {
		t.Fatal(err)
	}
}

func TestContactsSurviveReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := normalized(t, "W1AW")
	put(t, q, c)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	q, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	items := next(t, q)
	if len(items) != 1 || items[0].Contact != c {
		t.Fatalf("items = %+v", items)
	}
	if put(t, q, c) {
		t.Error("the same contact was queued again after reopening")
	}
}

func TestPutSkipsAnUnchangedContactButQueuesAChangedOne(t *testing.T) {
	q := open(t)
	c := normalized(t, "W1AW")

	if !put(t, q, c) {
		t.Fatal("first put wasn't queued")
	}
	if put(t, q, c) {
		t.Error("repeat was queued")
	}

	apply(t, q, Outcome{ID: next(t, q)[0].ID, Status: Accepted})
	if put(t, q, c) {
		t.Error("repeat of an accepted contact was queued")
	}

	c.Section = "CT"
	if !put(t, q, c) {
		t.Error("changed contact wasn't queued")
	}
}

func TestAContactIsNotGoneUntilAnswered(t *testing.T) {
	q := open(t)
	put(t, q, normalized(t, "W1AW"))

	first, second := next(t, q), next(t, q)
	if len(first) != 1 || len(second) != 1 {
		t.Fatal("reading the queue must not remove a contact")
	}
}

func TestRetryWaitsLongerEachTime(t *testing.T) {
	q := open(t)
	now := time.Unix(1_000_000, 0)
	q.now = func() time.Time { return now }
	put(t, q, normalized(t, "W1AW"))

	item := next(t, q)[0]
	apply(t, q, Outcome{ID: item.ID, Status: Retry, Code: "server_error"})
	if len(next(t, q)) != 0 {
		t.Fatal("should wait after a retry")
	}

	now = now.Add(retryBase)
	if len(next(t, q)) != 1 {
		t.Fatal("should be ready after the delay")
	}

	apply(t, q, Outcome{ID: item.ID, Status: Retry})
	now = now.Add(retryBase)
	if len(next(t, q)) != 0 {
		t.Fatal("the second delay should be longer")
	}
	now = now.Add(retryBase)
	if len(next(t, q)) != 1 {
		t.Fatal("should be ready after the second delay")
	}

	if retryDelay(100) != retryMax {
		t.Errorf("retryDelay(100) = %v", retryDelay(100))
	}
}

func TestRejectedContactStaysOnRecordAndIsNotResent(t *testing.T) {
	q := open(t)
	c := normalized(t, "W1AW")
	put(t, q, c)
	apply(t, q, Outcome{ID: next(t, q)[0].ID, Status: Rejected, Code: "out_of_period", Message: "outside"})

	if len(next(t, q)) != 0 {
		t.Error("rejected contact was offered again")
	}
	if put(t, q, c) {
		t.Error("rejected contact was queued again unchanged")
	}
	counts, err := q.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Rejected != 1 || counts.Pending != 0 {
		t.Errorf("counts = %+v", counts)
	}
}

func TestRetryingContactDoesNotBlockOthers(t *testing.T) {
	q := open(t)
	first := normalized(t, "W1AW")
	second := normalized(t, "K1ABC")
	put(t, q, first)
	put(t, q, second)

	items := next(t, q)
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	apply(t, q, Outcome{ID: items[0].ID, Status: Retry, Code: "unknown_station"}, Outcome{ID: items[1].ID, Status: Retry})

	third := normalized(t, "N1MM")
	put(t, q, third)
	items = next(t, q)
	if len(items) != 1 || items[0].Contact.Callsign != "N1MM" {
		t.Fatalf("items = %+v", items)
	}
}

// If an edit went out beside a failed original, the server would store the edit
// as new and the retried original as a second contact.
func TestAnEditWaitsForTheChangeBeforeIt(t *testing.T) {
	q := open(t)
	original := normalized(t, "W1AW")
	put(t, q, original)

	edit := normalized(t, "K1ABC")
	edit.ReplacesKey = original.IdempotencyKey
	put(t, q, edit)

	deleted := edit
	deleted.ReplacesKey = ""
	deleted.Deleted = true
	put(t, q, deleted)

	items := next(t, q)
	if len(items) != 1 || items[0].Contact != original {
		t.Fatalf("first batch = %+v", items)
	}

	apply(t, q, Outcome{ID: items[0].ID, Status: Retry, Code: "busy"})
	if items := next(t, q); len(items) != 0 {
		t.Fatalf("edit went ahead of its retrying original: %+v", items)
	}

	now := time.Now().Add(retryMax)
	q.now = func() time.Time { return now }

	first := next(t, q)
	if len(first) != 1 || first[0].Contact != original {
		t.Fatalf("after waiting = %+v", first)
	}
	apply(t, q, Outcome{ID: first[0].ID, Status: Accepted})

	second := next(t, q)
	if len(second) != 1 || second[0].Contact != edit {
		t.Fatalf("second batch = %+v", second)
	}
	apply(t, q, Outcome{ID: second[0].ID, Status: Rejected, Code: "invalid_contact"})

	third := next(t, q)
	if len(third) != 1 || !third[0].Contact.Deleted {
		t.Fatalf("third batch = %+v", third)
	}
}

func TestAnEditForgetsTheOldKey(t *testing.T) {
	q := open(t)
	original := normalized(t, "W1AW")
	put(t, q, original)

	edit := normalized(t, "K1ABC")
	edit.ReplacesKey = original.IdempotencyKey
	put(t, q, edit)

	if !put(t, q, original) {
		t.Error("editing a contact back to its old values should queue a change")
	}

	retried := edit
	retried.ReplacesKey = ""
	if put(t, q, retried) {
		t.Error("the same values under the same key shouldn't queue again")
	}
}

func TestNeedingAttentionListsWhatAPersonCanFix(t *testing.T) {
	q := open(t)
	for _, call := range []string{"W1AW", "K1ABC", "N1MM", "W3OK"} {
		put(t, q, normalized(t, call))
	}
	items := next(t, q)
	apply(t, q,
		Outcome{ID: items[0].ID, Status: Rejected, Code: "out_of_period", Message: "outside"},
		Outcome{ID: items[1].ID, Status: Retry, Code: "unknown_station", Message: "create it"},
		Outcome{ID: items[2].ID, Status: Retry, Code: "busy"},
		Outcome{ID: items[3].ID, Status: Accepted},
	)

	got, err := q.NeedingAttention(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Callsign != "K1ABC" || got[0].Rejected || got[1].Callsign != "W1AW" || !got[1].Rejected {
		t.Errorf("got %+v", got)
	}

	limited, err := q.NeedingAttention(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Errorf("limited = %+v", limited)
	}
}

func TestNextRespectsTheLimit(t *testing.T) {
	q := open(t)
	for _, call := range []string{"W1AW", "K1ABC", "N1MM"} {
		put(t, q, normalized(t, call))
	}
	items, err := q.Next(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Errorf("items = %d", len(items))
	}
}

func TestInvalidRecordsAreCapped(t *testing.T) {
	q := open(t)
	ctx := context.Background()
	for i := range maxInvalidRows + 5 {
		if err := q.PutInvalid(ctx, "n3fjp", "callsign", fmt.Sprintf("%d %s", i, strings.Repeat("x", maxInvalidRaw+1))); err != nil {
			t.Fatal(err)
		}
	}

	var count, longest int
	if err := q.db.QueryRow(`SELECT COUNT(*), MAX(LENGTH(raw)) FROM invalid`).Scan(&count, &longest); err != nil {
		t.Fatal(err)
	}
	if count != maxInvalidRows || longest != maxInvalidRaw {
		t.Errorf("count %d, longest %d", count, longest)
	}
}

func TestPutRefusesAContactWithoutAKey(t *testing.T) {
	if _, err := open(t).Put(context.Background(), contact.Contact{Callsign: "W1AW"}); err == nil {
		t.Error("expected an error")
	}
}

func TestAnswersFillInTheMirrorAndLaterUploadsNameTheUUID(t *testing.T) {
	q := open(t)
	ctx := context.Background()
	original := normalized(t, "W1AW")
	put(t, q, original)
	apply(t, q, Outcome{ID: next(t, q)[0].ID, Status: Accepted, ContactUUID: "uuid-1"})

	row, err := q.FindMirror(ctx, "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Key != original.IdempotencyKey || row.Baseline != nil || row.Deleted {
		t.Fatalf("row = %+v", row)
	}

	edit := normalized(t, "K1ABC")
	edit.ReplacesKey = original.IdempotencyKey
	put(t, q, edit)
	item := next(t, q)[0]
	if item.Contact.UUID != "uuid-1" {
		t.Errorf("the edit should name the contact: %+v", item.Contact)
	}

	apply(t, q, Outcome{ID: item.ID, Status: Accepted, ContactUUID: "uuid-1"})
	if row, _ = q.FindMirror(ctx, "", edit.IdempotencyKey); row == nil || row.UUID != "uuid-1" {
		t.Errorf("row after the edit = %+v", row)
	}

	gone := edit
	gone.ReplacesKey = ""
	gone.Deleted = true
	put(t, q, gone)
	item = next(t, q)[0]
	if item.Contact.UUID != "uuid-1" {
		t.Errorf("the delete should name the contact: %+v", item.Contact)
	}
	apply(t, q, Outcome{ID: item.ID, Status: Accepted})
	if row, _ = q.FindMirror(ctx, "uuid-1"); row == nil || !row.Deleted {
		t.Errorf("row after the delete = %+v", row)
	}
}

// A contact logged on the server has no idempotency key, and a new contact has no
// key it replaces. Those two blanks must not count as a match, or the new contact
// is sent as an edit of the server's contact and overwrites it.
func TestANewContactDoesNotTakeTheUUIDOfAContactWithNoKey(t *testing.T) {
	q := open(t)
	ctx := context.Background()
	if err := q.SaveMirror(ctx, MirrorRow{UUID: "logged-on-the-server", DerivedKey: "some-other-key"}); err != nil {
		t.Fatal(err)
	}
	if err := q.SaveMirror(ctx, MirrorRow{UUID: "no-keys-at-all"}); err != nil {
		t.Fatal(err)
	}

	put(t, q, normalized(t, "W1AW"))
	if item := next(t, q)[0]; item.Contact.UUID != "" {
		t.Errorf("a new contact was sent as an edit of %q", item.Contact.UUID)
	}
}

func TestAUUIDDoesNotMakeAContactLookChanged(t *testing.T) {
	q := open(t)
	c := normalized(t, "W1AW")
	put(t, q, c)
	c.UUID = "uuid-1"
	if put(t, q, c) {
		t.Error("the same values with a uuid were queued again")
	}
}

func TestRememberKeepsAnEchoFromBeingSent(t *testing.T) {
	q := open(t)
	c := normalized(t, "W1AW")
	if err := q.Remember(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if put(t, q, c) {
		t.Error("the relay's own write came back and was queued")
	}
	c.Section = "CT"
	if !put(t, q, c) {
		t.Error("a real change should still be queued")
	}
}

func TestHasPendingCoversBothKeys(t *testing.T) {
	q := open(t)
	ctx := context.Background()
	edit := normalized(t, "K1ABC")
	edit.ReplacesKey = "old-key-0000000000"
	put(t, q, edit)

	for key, want := range map[string]bool{edit.IdempotencyKey: true, "old-key-0000000000": true, "other": false, "": false} {
		if got, err := q.HasPending(ctx, key); err != nil || got != want {
			t.Errorf("%q: %v, %v", key, got, err)
		}
	}
	apply(t, q, Outcome{ID: next(t, q)[0].ID, Status: Rejected})
	if got, _ := q.HasPending(ctx, edit.IdempotencyKey); got {
		t.Error("an answered upload isn't pending")
	}
}

func TestResetSyncKeepsContactsAndForgetsTheServer(t *testing.T) {
	q := open(t)
	ctx := context.Background()
	c := normalized(t, "W1AW")
	put(t, q, c)
	apply(t, q, Outcome{ID: next(t, q)[0].ID, Status: Accepted, ContactUUID: "uuid-1"})
	if err := q.SetMeta(ctx, "feed_cursor", "x"); err != nil {
		t.Fatal(err)
	}
	if err := q.Defer(ctx, "uuid-2", []byte("{}")); err != nil {
		t.Fatal(err)
	}

	if err := q.ResetSync(ctx); err != nil {
		t.Fatal(err)
	}
	row, _ := q.FindMirror(ctx, "uuid-1")
	cursor, _ := q.Meta(ctx, "feed_cursor")
	held, _ := q.Deferred(ctx)
	counts, _ := q.Counts(ctx)
	if row != nil || cursor != "" || len(held) != 0 || counts.Accepted != 1 {
		t.Errorf("row %+v, cursor %q, held %v, counts %+v", row, cursor, held, counts)
	}
	if put(t, q, c) {
		t.Error("the upload history should survive")
	}
}

func TestTheSameInvalidRecordIsKeptOnce(t *testing.T) {
	q := open(t)
	ctx := context.Background()
	for _, raw := range []string{"bad", "bad", "worse"} {
		if err := q.PutInvalid(ctx, "logfile", "reason", raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.PutInvalid(ctx, "n3fjp", "reason", "bad"); err != nil {
		t.Fatal(err)
	}
	counts, err := q.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Invalid != 3 {
		t.Errorf("%d invalid, want 3", counts.Invalid)
	}
}

func TestTheEventPeriodOutlastsASyncResetButNotBeingForgotten(t *testing.T) {
	q := open(t)
	ctx := context.Background()
	if _, _, known, err := q.EventPeriod(ctx); err != nil || known {
		t.Fatalf("new queue: known %v, %v", known, err)
	}

	start := time.Date(2026, 6, 27, 14, 0, 0, 0, time.FixedZone("EDT", -4*3600))
	end := start.Add(27 * time.Hour)
	for range 2 {
		if err := q.SetEventPeriod(ctx, start, end); err != nil {
			t.Fatal(err)
		}
	}
	gotStart, gotEnd, known, err := q.EventPeriod(ctx)
	if err != nil || !known || !gotStart.Equal(start) || !gotEnd.Equal(end) {
		t.Errorf("got %v to %v, known %v, %v", gotStart, gotEnd, known, err)
	}

	if err := q.ResetSync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, known, _ := q.EventPeriod(ctx); !known {
		t.Error("changing the logger forgot the event's dates")
	}
	if err := q.ForgetEventPeriod(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, known, _ := q.EventPeriod(ctx); known {
		t.Error("still knows the old event's dates")
	}
}

func TestPickingAStationRetriesOnlyContactsWaitingForOne(t *testing.T) {
	q := open(t)
	put(t, q, normalized(t, "W1AW"))
	put(t, q, normalized(t, "K1ABC"))
	items := next(t, q)
	apply(t, q, Outcome{ID: items[0].ID, Status: Retry, Code: "unknown_station"}, Outcome{ID: items[1].ID, Status: Retry, Code: "busy"})

	if err := q.RetryWaitingForStation(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready := next(t, q)
	if len(ready) != 1 || ready[0].ID != items[0].ID {
		t.Errorf("ready = %+v", ready)
	}
}
