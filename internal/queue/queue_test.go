// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
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

	if len(next(t, q)) != 1 || len(next(t, q)) != 1 {
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
	for range maxInvalidRows + 5 {
		if err := q.PutInvalid(ctx, "n3fjp", "callsign", strings.Repeat("x", maxInvalidRaw+1)); err != nil {
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
