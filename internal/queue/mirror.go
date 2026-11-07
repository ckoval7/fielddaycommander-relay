// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

const eventPeriodName = "event_period"

// MirrorRow is what the relay knows of the server's copy of a contact.
type MirrorRow struct {
	UUID string
	// Key is the idempotency key the server holds, if any.
	Key string
	// DerivedKey is the key this contact has in the logger.
	DerivedKey string
	// Baseline is the server's last known values. Nil means the next feed entry
	// sets it, because the relay's own upload is what changed them.
	Baseline *contact.Contact
	Deleted  bool
}

// FindMirror looks a contact up by uuid, then by any of the keys.
func (q *Queue) FindMirror(ctx context.Context, uuid string, keys ...string) (*MirrorRow, error) {
	row, err := q.scanMirror(q.db.QueryRowContext(ctx, mirrorByUUID, uuid))
	if row != nil || err != nil {
		return row, err
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		row, err = q.scanMirror(q.db.QueryRowContext(ctx, mirrorByKey, key))
		if row != nil || err != nil {
			return row, err
		}
	}
	return nil, nil
}

const (
	mirrorByUUID = `SELECT uuid, idempotency_key, derived_key, payload, deleted FROM mirror WHERE uuid = ?`
	mirrorByKey  = `SELECT uuid, idempotency_key, derived_key, payload, deleted FROM mirror WHERE idempotency_key = ?1 OR derived_key = ?1 LIMIT 1`
)

func (q *Queue) scanMirror(found *sql.Row) (*MirrorRow, error) {
	var (
		row     MirrorRow
		payload string
	)
	if err := found.Scan(&row.UUID, &row.Key, &row.DerivedKey, &payload, &row.Deleted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mirror: %w", err)
	}
	if payload != "" {
		row.Baseline = &contact.Contact{}
		if err := json.Unmarshal([]byte(payload), row.Baseline); err != nil {
			return nil, fmt.Errorf("decode mirror: %w", err)
		}
	}
	return &row, nil
}

// SaveMirror stores a row, along with any stale row an earlier answer left under
// a different uuid for the same contact.
func (q *Queue) SaveMirror(ctx context.Context, row MirrorRow) error {
	payload := ""
	if row.Baseline != nil {
		encoded, err := json.Marshal(row.Baseline)
		if err != nil {
			return fmt.Errorf("encode mirror: %w", err)
		}
		payload = string(encoded)
	}
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO mirror (uuid, idempotency_key, derived_key, payload, deleted) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (uuid) DO UPDATE SET idempotency_key = excluded.idempotency_key,
		 derived_key = excluded.derived_key, payload = excluded.payload, deleted = excluded.deleted`,
		row.UUID, row.Key, row.DerivedKey, payload, row.Deleted)
	if err != nil {
		return fmt.Errorf("save mirror: %w", err)
	}
	return nil
}

// recordAccepted notes what an accepted upload means for the mirror. The next
// feed entry for that contact reflects the relay's own change, so it becomes the
// baseline and isn't mistaken for an edit made on the server.
func recordAccepted(ctx context.Context, tx *sql.Tx, o Outcome) error {
	var payload string
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM outbox WHERE id = ?`, o.ID).Scan(&payload); err != nil {
		return err
	}
	var sent contact.Contact
	if err := json.Unmarshal([]byte(payload), &sent); err != nil {
		return err
	}

	if sent.Deleted {
		_, err := tx.ExecContext(ctx,
			`UPDATE mirror SET deleted = 1, payload = '' WHERE uuid = ?1 OR idempotency_key = ?2 OR derived_key = ?2`,
			sent.UUID, sent.IdempotencyKey)
		return err
	}
	if o.ContactUUID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO mirror (uuid, idempotency_key, derived_key) VALUES (?1, ?2, ?2)
		 ON CONFLICT (uuid) DO UPDATE SET idempotency_key = ?2, derived_key = ?2, payload = '', deleted = 0`,
		o.ContactUUID, sent.IdempotencyKey)
	return err
}

// HasPending reports whether an upload for any of the keys is still unanswered.
func (q *Queue) HasPending(ctx context.Context, keys ...string) (bool, error) {
	for _, key := range keys {
		if key == "" {
			continue
		}
		var count int
		if err := q.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM outbox WHERE state = ?1 AND (idempotency_key = ?2 OR replaces_key = ?2)`,
			statePending, key).Scan(&count); err != nil {
			return false, fmt.Errorf("read queue: %w", err)
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Remember records a contact as already known without queueing it. The relay
// calls it before writing to the logger, so seeing its own write come back
// doesn't send the contact to the server again.
func (q *Queue) Remember(ctx context.Context, c contact.Contact) error {
	hash, err := valueHash(c)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO known (idempotency_key, payload_hash) VALUES (?, ?)
		 ON CONFLICT (idempotency_key) DO UPDATE SET payload_hash = excluded.payload_hash`,
		c.IdempotencyKey, hash)
	if err != nil {
		return fmt.Errorf("remember contact: %w", err)
	}
	return nil
}

// Meta reads a stored value, or "" if there isn't one.
func (q *Queue) Meta(ctx context.Context, name string) (string, error) {
	var value string
	err := q.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE name = ?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read meta: %w", err)
	}
	return value, nil
}

// SetMeta stores a value.
func (q *Queue) SetMeta(ctx context.Context, name, value string) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO meta (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, value)
	if err != nil {
		return fmt.Errorf("save meta: %w", err)
	}
	return nil
}

// SetEventPeriod remembers the event's dates, so a restart without internet still
// knows them. They belong to the enrollment, so changing the logger keeps them.
func (q *Queue) SetEventPeriod(ctx context.Context, start, end time.Time) error {
	value := start.UTC().Format(time.RFC3339) + "/" + end.UTC().Format(time.RFC3339)
	current, err := q.Meta(ctx, eventPeriodName)
	if err != nil || current == value {
		return err
	}
	return q.SetMeta(ctx, eventPeriodName, value)
}

// EventPeriod returns the event's dates, or false until the relay has learned
// them from the server.
func (q *Queue) EventPeriod(ctx context.Context) (start, end time.Time, known bool, err error) {
	value, err := q.Meta(ctx, eventPeriodName)
	if err != nil || value == "" {
		return time.Time{}, time.Time{}, false, err
	}
	first, last, found := strings.Cut(value, "/")
	if !found {
		return time.Time{}, time.Time{}, false, nil
	}
	if start, err = time.Parse(time.RFC3339, first); err != nil {
		return time.Time{}, time.Time{}, false, nil
	}
	if end, err = time.Parse(time.RFC3339, last); err != nil {
		return time.Time{}, time.Time{}, false, nil
	}
	return start, end, true, nil
}

// ForgetEventPeriod makes a new enrollment wait for its own event's dates.
func (q *Queue) ForgetEventPeriod(ctx context.Context) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM meta WHERE name = ?`, eventPeriodName); err != nil {
		return fmt.Errorf("forget event dates: %w", err)
	}
	return nil
}

// Defer holds a server change until its contact's upload is answered. The newest
// change for a contact replaces any older one.
func (q *Queue) Defer(ctx context.Context, uuid string, payload []byte) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO deferred (uuid, payload) VALUES (?, ?) ON CONFLICT (uuid) DO UPDATE SET payload = excluded.payload`,
		uuid, string(payload))
	if err != nil {
		return fmt.Errorf("defer change: %w", err)
	}
	return nil
}

// Deferred returns the held changes.
func (q *Queue) Deferred(ctx context.Context) (map[string][]byte, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT uuid, payload FROM deferred`)
	if err != nil {
		return nil, fmt.Errorf("read deferred: %w", err)
	}
	defer rows.Close()

	held := make(map[string][]byte)
	for rows.Next() {
		var uuid, payload string
		if err := rows.Scan(&uuid, &payload); err != nil {
			return nil, fmt.Errorf("read deferred: %w", err)
		}
		held[uuid] = []byte(payload)
	}
	return held, rows.Err()
}

// ClearDeferred drops a held change.
func (q *Queue) ClearDeferred(ctx context.Context, uuid string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM deferred WHERE uuid = ?`, uuid)
	if err != nil {
		return fmt.Errorf("clear deferred: %w", err)
	}
	return nil
}

// ResetSync forgets the server's view, for a relay moving to another server or
// event. The contacts themselves and their upload history stay.
func (q *Queue) ResetSync(ctx context.Context) error {
	return q.inTx(ctx, func(tx *sql.Tx) error {
		for _, statement := range []string{`DELETE FROM mirror`, `DELETE FROM deferred`} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("reset sync: %w", err)
			}
		}
		// The log's own numbering and the event's dates aren't about the feed, so they stay.
		_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE name NOT IN (?, ?)`, loggerCounterName, eventPeriodName)
		if err != nil {
			return fmt.Errorf("reset sync: %w", err)
		}
		return nil
	})
}
