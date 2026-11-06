// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package feed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

type writeCall struct {
	kind   string
	key    string
	before contact.Contact
	after  contact.Contact
}

type fakeWriter struct {
	mu    sync.Mutex
	calls []writeCall
	err   error
}

func (f *fakeWriter) Ready() bool { return true }

func (f *fakeWriter) record(call writeCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, call)
	return nil
}

func (f *fakeWriter) Add(_ context.Context, c contact.Contact) error {
	return f.record(writeCall{kind: "add", after: c})
}

func (f *fakeWriter) Edit(_ context.Context, key string, before, after contact.Contact) error {
	return f.record(writeCall{kind: "edit", key: key, before: before, after: after})
}

func (f *fakeWriter) Delete(_ context.Context, key string) error {
	return f.record(writeCall{kind: "delete", key: key})
}

type fakeClient struct {
	pages   []api.ChangePage
	cursors []string
	err     error
}

func (f *fakeClient) Handshake(context.Context) (api.Handshake, error) {
	return api.Handshake{}, nil
}

func (f *fakeClient) ContactChanges(_ context.Context, cursor string, _ int) (api.ChangePage, error) {
	f.cursors = append(f.cursors, cursor)
	if f.err != nil {
		return api.ChangePage{}, f.err
	}
	if len(f.pages) == 0 {
		return api.ChangePage{NextCursor: cursor}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

type harness struct {
	queue  *queue.Queue
	client *fakeClient
	writer *fakeWriter
	syncer *Syncer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	client, writer := &fakeClient{}, &fakeWriter{}
	return &harness{q, client, writer, New(q, client, writer, slog.New(slog.NewTextHandler(io.Discard, nil)))}
}

func (h *harness) feed(t *testing.T, changes ...api.ContactChange) {
	t.Helper()
	h.client.pages = append(h.client.pages, api.ChangePage{Contacts: changes, NextCursor: "next"})
	if _, err := h.syncer.step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func live(uuid, call string) api.ContactChange {
	return api.ContactChange{UUID: uuid, Callsign: call, QSOTime: "2026-06-27T18:05:00Z", Band: "20m", Mode: "SSB", Section: "CT", Station: "Web"}
}

func normalized(t *testing.T, call string) contact.Contact {
	t.Helper()
	c, _, err := contact.Normalize(toContact(live("", call)))
	if err != nil {
		t.Fatal(err)
	}
	c.UUID = ""
	return c
}

func kinds(writer *fakeWriter) []string {
	var out []string
	for _, call := range writer.calls {
		out = append(out, call.kind)
	}
	return out
}

func TestAContactLoggedOnTheServerIsAddedOnce(t *testing.T) {
	h := newHarness(t)
	h.feed(t, live("u1", "W1AW"))
	h.feed(t, live("u1", "W1AW"))

	if got := kinds(h.writer); !slices.Equal(got, []string{"add"}) {
		t.Fatalf("calls = %v", got)
	}
	if h.writer.calls[0].after.Callsign != "W1AW" {
		t.Errorf("add = %+v", h.writer.calls[0].after)
	}
}

func TestRescoringThatChangesNothingWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.feed(t, live("u1", "W1AW"))
	repeat := live("u1", "W1AW")
	repeat.UpdatedAt = repeat.UpdatedAt.AddDate(0, 0, 1)
	h.feed(t, repeat)

	if got := kinds(h.writer); len(got) != 1 {
		t.Errorf("calls = %v", got)
	}
}

// The relay's own upload comes back through the feed. It must not look like a
// server edit, even with the values the server changed on the way: this is what
// stops a 5NN report or a section code being overwritten in the logger.
func TestTheRelaysOwnUploadOnlySetsTheBaseline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sent := normalized(t, "W1AW")
	if _, err := h.queue.Put(ctx, sent); err != nil {
		t.Fatal(err)
	}
	items, _ := h.queue.Next(ctx, 10)
	if err := h.queue.Apply(ctx, []queue.Outcome{{ID: items[0].ID, Status: queue.Accepted, ContactUUID: "u1"}}); err != nil {
		t.Fatal(err)
	}

	back := live("u1", "W1AW")
	back.IdempotencyKey = sent.IdempotencyKey
	back.Section = "TER"
	h.feed(t, back)

	if got := kinds(h.writer); len(got) != 0 {
		t.Fatalf("the upload was written back: %v", got)
	}

	row, err := h.queue.FindMirror(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.Baseline == nil || row.Baseline.Section != "TER" {
		t.Errorf("baseline = %+v", row)
	}
}

func TestAServerEditWritesWhatChanged(t *testing.T) {
	h := newHarness(t)
	h.feed(t, live("u1", "W1AW"))
	key := h.writer.calls[0].after.IdempotencyKey

	edited := live("u1", "W1AW")
	edited.Section = "NH"
	h.feed(t, edited)

	if got := kinds(h.writer); !slices.Equal(got, []string{"add", "edit"}) {
		t.Fatalf("calls = %v", got)
	}
	call := h.writer.calls[1]
	if call.key != key || call.before.Section != "CT" || call.after.Section != "NH" {
		t.Errorf("edit = %+v", call)
	}
}

func TestAServerEditThatChangesTheKeyFindsTheRecordLater(t *testing.T) {
	h := newHarness(t)
	h.feed(t, live("u1", "W1WA"))
	h.feed(t, live("u1", "W1AW"))
	renamed := h.writer.calls[1].after.IdempotencyKey

	again := live("u1", "W1AW")
	again.Section = "NH"
	h.feed(t, again)

	if got := h.writer.calls[2]; got.kind != "edit" || got.key != renamed {
		t.Errorf("edit = %+v, want key %s", got, renamed)
	}
}

// An unanswered upload is newer than anything the feed can show.
func TestAServerChangeWaitsForAnUnansweredUpload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.feed(t, live("u1", "W1AW"))
	before := normalized(t, "W1AW")

	if _, err := h.queue.Put(ctx, withSection(before, "ME")); err != nil {
		t.Fatal(err)
	}
	edited := live("u1", "W1AW")
	edited.Section = "NH"
	h.feed(t, edited)

	if got := kinds(h.writer); !slices.Equal(got, []string{"add"}) {
		t.Fatalf("wrote over an unanswered change: %v", got)
	}
	held, _ := h.queue.Deferred(ctx)
	if len(held) != 1 {
		t.Fatalf("held = %v", held)
	}

	h.feed(t)
	if got := kinds(h.writer); len(got) != 1 {
		t.Fatalf("still unanswered, but wrote: %v", got)
	}

	items, _ := h.queue.Next(ctx, 10)
	if err := h.queue.Apply(ctx, []queue.Outcome{{ID: items[0].ID, Status: queue.Accepted, ContactUUID: "u1"}}); err != nil {
		t.Fatal(err)
	}
	h.feed(t)
	held, _ = h.queue.Deferred(ctx)
	if len(held) != 0 {
		t.Errorf("held after the answer = %v", held)
	}
}

func withSection(c contact.Contact, section string) contact.Contact {
	c.Section = section
	return c
}

func TestAServerDeleteRemovesTheRecordOnce(t *testing.T) {
	h := newHarness(t)
	h.feed(t, live("u1", "W1AW"))
	key := h.writer.calls[0].after.IdempotencyKey
	gone := api.ContactChange{UUID: "u1", Deleted: true}

	h.feed(t, gone)
	h.feed(t, gone)

	if got := kinds(h.writer); !slices.Equal(got, []string{"add", "delete"}) {
		t.Fatalf("calls = %v", got)
	}
	if h.writer.calls[1].key != key {
		t.Errorf("delete = %+v", h.writer.calls[1])
	}
}

func TestADeleteOfAnUnknownContactIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.feed(t, api.ContactChange{UUID: "stranger", Deleted: true})
	if got := kinds(h.writer); len(got) != 0 {
		t.Errorf("calls = %v", got)
	}
}

// A relay starting over needs these to remove what was deleted while it was away.
func TestADeleteNamingAKeyReachesTheLoggerAfterStartingOver(t *testing.T) {
	h := newHarness(t)
	h.feed(t, api.ContactChange{UUID: "u1", Deleted: true, IdempotencyKey: "key-from-before-000"})
	if len(h.writer.calls) != 1 || h.writer.calls[0].key != "key-from-before-000" {
		t.Errorf("calls = %+v", h.writer.calls)
	}
}

func TestARestoredContactComesBack(t *testing.T) {
	h := newHarness(t)
	h.feed(t, live("u1", "W1AW"))
	h.feed(t, api.ContactChange{UUID: "u1", Deleted: true})
	h.feed(t, live("u1", "W1AW"))

	if got := kinds(h.writer); !slices.Equal(got, []string{"add", "delete", "add"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestAServerContactThatDoesNotFitIsSkippedWithoutStoppingTheFeed(t *testing.T) {
	h := newHarness(t)
	h.feed(t, live("u1", "NOT A CALL"), live("u2", "W1AW"))
	if got := kinds(h.writer); !slices.Equal(got, []string{"add"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestThePositionMovesOnlyAfterThePageIsApplied(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.writer.err = errors.New("the logger went away")
	h.client.pages = []api.ChangePage{{Contacts: []api.ContactChange{live("u1", "W1AW")}, NextCursor: "after"}}

	if _, err := h.syncer.step(ctx); err == nil {
		t.Fatal("expected the failure to surface")
	}
	if cursor, _ := h.queue.Meta(ctx, cursorName); cursor != "" {
		t.Errorf("cursor moved past a change that wasn't applied: %q", cursor)
	}
	row, _ := h.queue.FindMirror(ctx, "u1")
	if row != nil {
		t.Errorf("recorded a change that wasn't applied: %+v", row)
	}

	h.writer.err = nil
	h.client.pages = []api.ChangePage{{Contacts: []api.ContactChange{live("u1", "W1AW")}, NextCursor: "after"}}
	more, err := h.syncer.step(ctx)
	if err != nil || more {
		t.Fatalf("more %v, err %v", more, err)
	}
	if cursor, _ := h.queue.Meta(ctx, cursorName); cursor != "after" {
		t.Errorf("cursor = %q", cursor)
	}
	if _, err := h.syncer.step(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.client.cursors[len(h.client.cursors)-1]; got != "after" {
		t.Errorf("sent cursor %q", got)
	}
}

func TestARefusedPositionStartsOver(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.queue.SetMeta(ctx, cursorName, "stale"); err != nil {
		t.Fatal(err)
	}
	h.client.err = &api.StatusError{StatusCode: 422, Code: "invalid_cursor"}

	more, err := h.syncer.step(ctx)
	if err != nil || !more {
		t.Fatalf("more %v, err %v", more, err)
	}
	if cursor, _ := h.queue.Meta(ctx, cursorName); cursor != "" {
		t.Errorf("cursor = %q", cursor)
	}
}

func TestAnOlderServerIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.syncer.Run(ctx)
	}()
	cancel()
	<-done
	if len(h.client.cursors) != 0 || h.syncer.Status().Available {
		t.Errorf("read the feed from a server without it: %v", h.client.cursors)
	}
}
