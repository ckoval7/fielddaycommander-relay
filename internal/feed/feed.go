// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package feed brings changes made on the server into the logger.
//
// A contact counts as changed only when it differs from what the server last
// said, never because it differs from the logger. The trip through the server
// isn't exact: the relay leaves out values the server refuses, like a 5NN report,
// and the server may store a section under the code it counts as. Comparing with
// the logger would overwrite what the operator typed. The same comparison means
// only the fields that really changed reach the logger.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

const (
	cursorName   = "feed_cursor"
	pageSize     = 100
	pollInterval = 5 * time.Second
	backoffStart = 2 * time.Second
	backoffMax   = 5 * time.Minute
	recheckWait  = 5 * time.Minute
)

// Client is the part of the API the feed uses.
type Client interface {
	Handshake(ctx context.Context) (api.Handshake, error)
	ContactChanges(ctx context.Context, cursor string, limit int) (api.ChangePage, error)
}

// Writer applies server changes to the logger. Each call must be safe to repeat:
// an error leaves the feed's position where it was, so the change comes again.
type Writer interface {
	// Ready reports whether the logger's log is loaded and can be written to.
	Ready() bool
	// Add puts a contact in the logger, unless it already has one with that key.
	Add(ctx context.Context, c contact.Contact) error
	// Edit applies the fields that differ between before and after to the record
	// with this key.
	Edit(ctx context.Context, key string, before, after contact.Contact) error
	// Delete removes the record with this key, if there is one.
	Delete(ctx context.Context, key string) error
}

// Status is a snapshot for the status page.
type Status struct {
	// Available is whether the server offers the feed.
	Available bool
	LastSync  time.Time
}

// Syncer runs the feed.
type Syncer struct {
	queue  *queue.Queue
	client Client
	writer Writer
	log    *slog.Logger

	poll    time.Duration
	minWait time.Duration
	maxWait time.Duration
	recheck time.Duration

	statusMu sync.Mutex
	status   Status
}

// New returns a Syncer.
func New(q *queue.Queue, client Client, writer Writer, log *slog.Logger) *Syncer {
	return &Syncer{
		queue: q, client: client, writer: writer, log: log,
		poll: pollInterval, minWait: backoffStart, maxWait: backoffMax, recheck: recheckWait,
	}
}

// Status returns the current status.
func (s *Syncer) Status() Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

// Run follows the feed until ctx is canceled.
func (s *Syncer) Run(ctx context.Context) {
	failures := 0
	available := false

	for ctx.Err() == nil {
		if !available {
			handshake, err := s.client.Handshake(ctx)
			if err != nil {
				failures++
				if !sleep(ctx, s.backoff(failures)) {
					return
				}
				continue
			}
			failures = 0
			available = handshake.Capabilities.ContactFeed
			s.setStatus(func(status *Status) { status.Available = available })
			if !available {
				// An older server: uploading still works without the feed.
				if !sleep(ctx, s.recheck) {
					return
				}
				continue
			}
		}

		// The source reads the logger's log first, so contacts in both places are
		// recognized and not written twice.
		if !s.writer.Ready() {
			if !sleep(ctx, s.poll) {
				return
			}
			continue
		}

		more, err := s.step(ctx)
		switch {
		case err == nil:
			failures = 0
			s.setStatus(func(status *Status) { status.LastSync = time.Now() })
			if more {
				continue
			}
			if !sleep(ctx, s.poll) {
				return
			}
		case ctx.Err() != nil:
			return
		default:
			s.log.Warn("following the server's contacts failed", "error", err)
			failures++
			available = false
			if !sleep(ctx, s.backoff(failures)) {
				return
			}
		}
	}
}

func (s *Syncer) step(ctx context.Context) (bool, error) {
	if err := s.retryDeferred(ctx); err != nil {
		return false, err
	}

	cursor, err := s.queue.Meta(ctx, cursorName)
	if err != nil {
		return false, err
	}
	page, err := s.client.ContactChanges(ctx, cursor, pageSize)
	if err != nil {
		var statusErr *api.StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusUnprocessableEntity && statusErr.Code == "invalid_cursor" {
			s.log.Warn("the server refused the feed position, starting over")
			return true, s.queue.SetMeta(ctx, cursorName, "")
		}
		return false, err
	}

	for _, change := range page.Contacts {
		if err := s.apply(ctx, change); err != nil {
			return false, err
		}
	}
	// Stored after the page is applied, so a crash repeats changes and never skips any.
	if err := s.queue.SetMeta(ctx, cursorName, page.NextCursor); err != nil {
		return false, err
	}
	return page.HasMore, nil
}

func (s *Syncer) retryDeferred(ctx context.Context) error {
	held, err := s.queue.Deferred(ctx)
	if err != nil {
		return err
	}
	for uuid, payload := range held {
		var change api.ContactChange
		if err := json.Unmarshal(payload, &change); err != nil {
			s.log.Error("dropping a damaged held change", "uuid", uuid, "error", err)
			if err := s.queue.ClearDeferred(ctx, uuid); err != nil {
				return err
			}
			continue
		}
		deferred, err := s.applyNow(ctx, change)
		if err != nil {
			return err
		}
		if !deferred {
			if err := s.queue.ClearDeferred(ctx, uuid); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Syncer) apply(ctx context.Context, change api.ContactChange) error {
	deferred, err := s.applyNow(ctx, change)
	if err != nil || !deferred {
		return err
	}
	payload, err := json.Marshal(change)
	if err != nil {
		return err
	}
	return s.queue.Defer(ctx, change.UUID, payload)
}

// applyNow applies a change, or reports that it has to wait because the relay has
// an unanswered upload for that contact. That upload is newer than anything the
// feed can show, and the server will answer with the result.
func (s *Syncer) applyNow(ctx context.Context, change api.ContactChange) (bool, error) {
	if change.Deleted {
		return s.applyDelete(ctx, change)
	}

	incoming, _, err := contact.Normalize(toContact(change))
	if err != nil {
		// The server accepts contacts from paths the relay's rules don't cover.
		s.log.Warn("a server contact doesn't fit the logger", "uuid", change.UUID, "error", err)
		return false, nil
	}

	row, err := s.queue.FindMirror(ctx, change.UUID, change.IdempotencyKey, incoming.IdempotencyKey)
	if err != nil {
		return false, err
	}
	keys := []string{change.IdempotencyKey, incoming.IdempotencyKey}
	if row != nil {
		keys = append(keys, row.Key, row.DerivedKey)
	}
	if pending, err := s.queue.HasPending(ctx, keys...); err != nil || pending {
		return pending, err
	}

	next := queue.MirrorRow{UUID: change.UUID, Key: change.IdempotencyKey, DerivedKey: incoming.IdempotencyKey, Baseline: &incoming}

	switch {
	case row == nil || row.Deleted:
		// New to the relay, or restored on the server.
		if err := s.writer.Add(ctx, incoming); err != nil {
			return false, err
		}
	case row.Baseline == nil:
		// The relay's own upload, coming back. It sets the baseline.
		next.DerivedKey = firstNonEmpty(row.DerivedKey, incoming.IdempotencyKey)
	case sameValues(*row.Baseline, incoming):
		next.DerivedKey = row.DerivedKey
	default:
		if err := s.writer.Edit(ctx, row.DerivedKey, *row.Baseline, incoming); err != nil {
			return false, err
		}
	}
	return false, s.queue.SaveMirror(ctx, next)
}

func (s *Syncer) applyDelete(ctx context.Context, change api.ContactChange) (bool, error) {
	row, err := s.queue.FindMirror(ctx, change.UUID, change.IdempotencyKey)
	if err != nil {
		return false, err
	}

	key := change.IdempotencyKey
	if row != nil {
		if row.Deleted {
			return false, nil
		}
		key = firstNonEmpty(row.DerivedKey, row.Key, key)
	}
	// The spec says to ignore a delete for a contact the relay doesn't hold.
	if key == "" {
		return false, nil
	}

	keys := []string{key, change.IdempotencyKey}
	if pending, err := s.queue.HasPending(ctx, keys...); err != nil || pending {
		return pending, err
	}
	if err := s.writer.Delete(ctx, key); err != nil {
		return false, err
	}
	return false, s.queue.SaveMirror(ctx, queue.MirrorRow{UUID: change.UUID, Key: change.IdempotencyKey, DerivedKey: key, Deleted: true})
}

func toContact(change api.ContactChange) contact.Contact {
	return contact.Contact{
		UUID:          change.UUID,
		Callsign:      change.Callsign,
		QSOTime:       change.QSOTime,
		Band:          change.Band,
		Mode:          change.Mode,
		FrequencyHz:   change.FrequencyHz,
		Operator:      change.Operator,
		Station:       change.Station,
		ExchangeClass: change.ExchangeClass,
		Section:       change.Section,
		PowerWatts:    change.PowerWatts,
		RSTSent:       change.RSTSent,
		RSTReceived:   change.RSTReceived,
		GridSquare:    change.GridSquare,
		ContactName:   change.ContactName,
		Notes:         change.Notes,
	}
}

func sameValues(a, b contact.Contact) bool {
	for _, c := range []*contact.Contact{&a, &b} {
		c.IdempotencyKey, c.ReplacesKey, c.UUID = "", "", ""
		c.Deleted = false
	}
	return a == b
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *Syncer) backoff(failures int) time.Duration {
	wait := s.minWait
	for i := 1; i < failures && wait < s.maxWait; i++ {
		wait *= 2
	}
	wait = min(wait, s.maxWait)
	return wait + time.Duration(rand.Int64N(int64(wait)/5+1))
}

func (s *Syncer) setStatus(change func(*Status)) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	change(&s.status)
}

func sleep(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
