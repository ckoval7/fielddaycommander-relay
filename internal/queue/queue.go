// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package queue is the relay's durable store. A contact is committed to disk
// here before anything tries to send it, and stays until the server has given
// a final answer, so a crash, power cut or long outage can't lose one.
package queue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"

	_ "modernc.org/sqlite" // Registers the pure-Go SQLite driver.
)

const (
	stateRejected = "rejected"
	statePending  = "pending"
	stateAccepted = "accepted"

	maxInvalidRows = 1000
	maxInvalidRaw  = 4096

	retryBase = 5 * time.Second
	retryMax  = 5 * time.Minute
)

const schema = `
CREATE TABLE IF NOT EXISTS outbox (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	idempotency_key TEXT    NOT NULL,
	replaces_key    TEXT    NOT NULL DEFAULT '',
	payload         TEXT    NOT NULL,
	state           TEXT    NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_ms INTEGER NOT NULL DEFAULT 0,
	code            TEXT    NOT NULL DEFAULT '',
	message         TEXT    NOT NULL DEFAULT '',
	contact_uuid    TEXT    NOT NULL DEFAULT '',
	created_ms      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS outbox_state ON outbox (state, id);

-- What the relay last queued for each contact, so re-reading a logger's whole
-- log doesn't send anything that hasn't changed.
CREATE TABLE IF NOT EXISTS known (
	idempotency_key TEXT PRIMARY KEY,
	payload_hash    TEXT NOT NULL
) WITHOUT ROWID;

-- The server's view of each contact, from the feed and from answers to uploads.
-- An empty payload means the next feed entry sets the baseline, since the relay's
-- own upload is what changed it.
CREATE TABLE IF NOT EXISTS mirror (
	uuid            TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL DEFAULT '',
	derived_key     TEXT NOT NULL DEFAULT '',
	payload         TEXT NOT NULL DEFAULT '',
	deleted         INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS mirror_key ON mirror (idempotency_key);
CREATE INDEX IF NOT EXISTS mirror_derived ON mirror (derived_key);

-- Server changes waiting for a contact's unanswered upload.
CREATE TABLE IF NOT EXISTS deferred (
	uuid    TEXT PRIMARY KEY,
	payload TEXT NOT NULL
) WITHOUT ROWID;

-- The log the relay keeps while it acts as the N3FJP network server. N3FJP clients
-- keep none of their own, so numbers and whole records have to survive a restart.
CREATE TABLE IF NOT EXISTS logger_records (
	number          INTEGER PRIMARY KEY,
	idempotency_key TEXT NOT NULL DEFAULT '',
	fields          TEXT NOT NULL
);

-- The key each contact from N1MM currently has, by N1MM's id for it.
CREATE TABLE IF NOT EXISTS n1mm_records (
	id              TEXT PRIMARY KEY,
	idempotency_key TEXT NOT NULL,
	callsign        TEXT NOT NULL,
	qso_time        TEXT NOT NULL,
	station         TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS meta (
	name  TEXT PRIMARY KEY,
	value TEXT NOT NULL
) WITHOUT ROWID;

-- Records from a logger that can't be made into a contact, kept for a person
-- to look at.
CREATE TABLE IF NOT EXISTS invalid (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	source      TEXT    NOT NULL,
	reason      TEXT    NOT NULL,
	raw         TEXT    NOT NULL,
	created_ms  INTEGER NOT NULL
);
`

// Status is the server's answer for one contact.
type Status string

// The answers the relay API gives, plus Retry for a contact with no answer.
const (
	Accepted Status = "accepted"
	Rejected Status = "rejected"
	Retry    Status = "retry"
)

// Item is a queued contact ready to send.
type Item struct {
	ID      int64
	Contact contact.Contact
}

// Outcome is what happened to one Item.
type Outcome struct {
	ID          int64
	Status      Status
	Code        string
	Message     string
	ContactUUID string
}

// Counts summarizes the queue for the status page.
type Counts struct {
	Pending  int
	Accepted int
	Rejected int
	Invalid  int
}

// Queue is safe for use by several goroutines.
type Queue struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens or creates the queue database at path.
func Open(path string) (*Queue, error) {
	dsn, err := dataSource(path)
	if err != nil {
		return nil, fmt.Errorf("open queue: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open queue: %w", err)
	}
	// SQLite allows one writer, and old hardware doesn't need a pool.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create queue: %w", err)
	}
	return &Queue{db: db, now: time.Now}, nil
}

// dataSource builds the SQLite URI, escaping the characters a Windows user or
// folder name can hold.
func dataSource(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed, RawQuery: url.Values{"_pragma": {
		"busy_timeout(5000)",
		"journal_mode(WAL)",
		// A contact isn't queued until it survives a power cut.
		"synchronous(FULL)",
	}}.Encode()}).String(), nil
}

// Close closes the database.
func (q *Queue) Close() error {
	return q.db.Close()
}

// Put commits a normalized contact to the queue. It returns false, and queues
// nothing, when the relay already queued this contact with the same values.
func (q *Queue) Put(ctx context.Context, c contact.Contact) (bool, error) {
	if c.IdempotencyKey == "" {
		return false, errors.New("queue: contact has no idempotency key")
	}
	if c.ReplacesKey == c.IdempotencyKey {
		c.ReplacesKey = ""
	}

	hash, err := valueHash(c)
	if err != nil {
		return false, err
	}

	queued := false
	err = q.inTx(ctx, func(tx *sql.Tx) error {
		// Name the contact by uuid when the server has given one, which also
		// reaches contacts logged elsewhere that have no idempotency key.
		if c.UUID == "" {
			if err := tx.QueryRowContext(ctx,
				`SELECT uuid FROM mirror WHERE idempotency_key IN (?1, ?2) OR derived_key IN (?1, ?2) LIMIT 1`,
				c.IdempotencyKey, c.ReplacesKey).Scan(&c.UUID); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		payload, err := json.Marshal(c)
		if err != nil {
			return fmt.Errorf("encode contact: %w", err)
		}

		var known string
		err = tx.QueryRowContext(ctx, `SELECT payload_hash FROM known WHERE idempotency_key = ?`, c.IdempotencyKey).Scan(&known)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && known == hash {
			return nil
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO outbox (idempotency_key, replaces_key, payload, state, created_ms) VALUES (?, ?, ?, ?, ?)`,
			c.IdempotencyKey, c.ReplacesKey, string(payload), statePending, q.now().UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO known (idempotency_key, payload_hash) VALUES (?, ?)
			 ON CONFLICT (idempotency_key) DO UPDATE SET payload_hash = excluded.payload_hash`,
			c.IdempotencyKey, hash); err != nil {
			return err
		}
		if c.ReplacesKey != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM known WHERE idempotency_key = ?`, c.ReplacesKey); err != nil {
				return err
			}
		}
		queued = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("queue contact: %w", err)
	}
	return queued, nil
}

// PutInvalid keeps a record that can't become a contact. Local data is
// untrusted, so the text is capped and only the newest records are kept. Sources
// re-read their logs, so a record already kept isn't kept twice.
func (q *Queue) PutInvalid(ctx context.Context, source, reason, raw string) error {
	if len(raw) > maxInvalidRaw {
		raw = raw[:maxInvalidRaw]
	}
	err := q.inTx(ctx, func(tx *sql.Tx) error {
		var seen int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invalid WHERE source = ? AND raw = ?`, source, raw).Scan(&seen); err != nil {
			return err
		}
		if seen > 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO invalid (source, reason, raw, created_ms) VALUES (?, ?, ?, ?)`,
			source, reason, raw, q.now().UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM invalid WHERE id <= (SELECT MAX(id) FROM invalid) - ?`, maxInvalidRows)
		return err
	})
	if err != nil {
		return fmt.Errorf("keep invalid record: %w", err)
	}
	return nil
}

// Next returns up to limit contacts to send, oldest first.
//
// Each batch holds at most one change per contact, and a contact waits behind
// an earlier change that hasn't been answered. The server handles a whole
// batch whatever each result is, so an edit sent beside its failed original
// would be stored as a second contact when the original was retried. A contact
// waiting on a retry doesn't hold up unrelated ones.
func (q *Queue) Next(ctx context.Context, limit int) ([]Item, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT id, idempotency_key, replaces_key, payload, next_attempt_ms FROM outbox WHERE state = ? ORDER BY id`, statePending)
	if err != nil {
		return nil, fmt.Errorf("read queue: %w", err)
	}
	defer rows.Close()

	nowMs := q.now().UnixMilli()
	busy := make(map[string]struct{})
	var items []Item

	for rows.Next() && len(items) < limit {
		var (
			item          Item
			key, replaces string
			payload       string
			nextAttemptMs int64
		)
		if err := rows.Scan(&item.ID, &key, &replaces, &payload, &nextAttemptMs); err != nil {
			return nil, fmt.Errorf("read queue: %w", err)
		}

		_, keyBusy := busy[key]
		_, replacesBusy := busy[replaces]
		ready := !keyBusy && !replacesBusy && nextAttemptMs <= nowMs

		busy[key] = struct{}{}
		if replaces != "" {
			busy[replaces] = struct{}{}
		}
		if !ready {
			continue
		}

		if err := json.Unmarshal([]byte(payload), &item.Contact); err != nil {
			return nil, fmt.Errorf("decode queued contact %d: %w", item.ID, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read queue: %w", err)
	}
	return items, nil
}

// Apply records the server's answers. Accepted and rejected contacts keep their
// rows as a local record, and a rejected one isn't sent again unless the logger
// changes it. A contact to retry waits longer after each attempt.
func (q *Queue) Apply(ctx context.Context, outcomes []Outcome) error {
	nowMs := q.now().UnixMilli()
	err := q.inTx(ctx, func(tx *sql.Tx) error {
		for _, o := range outcomes {
			var err error
			switch o.Status {
			case Accepted:
				_, err = tx.ExecContext(ctx,
					`UPDATE outbox SET state = ?, contact_uuid = ?, code = '', message = '', attempts = attempts + 1 WHERE id = ?`,
					stateAccepted, o.ContactUUID, o.ID)
				if err == nil {
					err = recordAccepted(ctx, tx, o)
				}
			case Rejected:
				_, err = tx.ExecContext(ctx,
					`UPDATE outbox SET state = ?, code = ?, message = ?, attempts = attempts + 1 WHERE id = ?`,
					stateRejected, o.Code, o.Message, o.ID)
			case Retry:
				var attempts int
				if err = tx.QueryRowContext(ctx, `SELECT attempts FROM outbox WHERE id = ?`, o.ID).Scan(&attempts); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx,
					`UPDATE outbox SET code = ?, message = ?, attempts = ?, next_attempt_ms = ? WHERE id = ?`,
					o.Code, o.Message, attempts+1, nowMs+retryDelay(attempts).Milliseconds(), o.ID)
			default:
				err = fmt.Errorf("unknown status %q", o.Status)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record results: %w", err)
	}
	return nil
}

// RetryWaitingForStation sends contacts waiting for a station on the next round
// instead of after their retry delay, because a person has just changed the station.
func (q *Queue) RetryWaitingForStation(ctx context.Context) error {
	if _, err := q.db.ExecContext(ctx,
		`UPDATE outbox SET next_attempt_ms = 0 WHERE state = ? AND code = 'unknown_station'`, statePending); err != nil {
		return fmt.Errorf("retry contacts waiting for a station: %w", err)
	}
	return nil
}

// Attention is a contact a person may need to do something about.
type Attention struct {
	Callsign string
	QSOTime  string
	Station  string
	Rejected bool
	Code     string
	Message  string
}

// NeedingAttention returns the newest contacts the server rejected or that wait
// for a station to be created. Other retries sort themselves out.
func (q *Queue) NeedingAttention(ctx context.Context, limit int) ([]Attention, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT payload, state, code, message FROM outbox
		 WHERE state = ? OR (state = ? AND code = 'unknown_station')
		 ORDER BY id DESC LIMIT ?`, stateRejected, statePending, limit)
	if err != nil {
		return nil, fmt.Errorf("read queue: %w", err)
	}
	defer rows.Close()

	var attention []Attention
	for rows.Next() {
		var payload, state, code, message string
		if err := rows.Scan(&payload, &state, &code, &message); err != nil {
			return nil, fmt.Errorf("read queue: %w", err)
		}
		var c contact.Contact
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			return nil, fmt.Errorf("decode queued contact: %w", err)
		}
		attention = append(attention, Attention{
			Callsign: c.Callsign,
			QSOTime:  c.QSOTime,
			Station:  c.Station,
			Rejected: state == stateRejected,
			Code:     code,
			Message:  message,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read queue: %w", err)
	}
	return attention, nil
}

// Counts returns how many contacts are in each state.
func (q *Queue) Counts(ctx context.Context) (Counts, error) {
	var counts Counts
	rows, err := q.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM outbox GROUP BY state`)
	if err != nil {
		return counts, fmt.Errorf("count queue: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			state string
			count int
		)
		if err := rows.Scan(&state, &count); err != nil {
			return counts, fmt.Errorf("count queue: %w", err)
		}
		switch state {
		case statePending:
			counts.Pending = count
		case stateAccepted:
			counts.Accepted = count
		case stateRejected:
			counts.Rejected = count
		}
	}
	if err := rows.Err(); err != nil {
		return counts, fmt.Errorf("count queue: %w", err)
	}
	if err := rows.Close(); err != nil {
		return counts, fmt.Errorf("count queue: %w", err)
	}

	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invalid`).Scan(&counts.Invalid); err != nil {
		return counts, fmt.Errorf("count queue: %w", err)
	}
	return counts, nil
}

func (q *Queue) inTx(ctx context.Context, work func(*sql.Tx) error) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := work(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// valueHash identifies a contact's values. ReplacesKey describes how it got
// here rather than what it is, so it stays out.
func valueHash(c contact.Contact) (string, error) {
	c.ReplacesKey = ""
	c.UUID = ""
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode contact: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func retryDelay(attempts int) time.Duration {
	delay := retryBase
	for range attempts {
		delay *= 2
		if delay >= retryMax {
			return retryMax
		}
	}
	return delay
}
