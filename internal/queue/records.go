// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const loggerCounterName = "logger_counter"

// LoggerRecord is a numbered record in the log the relay keeps as an N3FJP server.
type LoggerRecord struct {
	Number int
	Key    string
	Fields map[string]string
}

// LoggerRecords returns the log and the last number handed out.
func (q *Queue) LoggerRecords(ctx context.Context) ([]LoggerRecord, int, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT number, idempotency_key, fields FROM logger_records ORDER BY number`)
	if err != nil {
		return nil, 0, fmt.Errorf("read log: %w", err)
	}
	defer rows.Close()

	var records []LoggerRecord
	for rows.Next() {
		var (
			record LoggerRecord
			fields string
		)
		if err := rows.Scan(&record.Number, &record.Key, &fields); err != nil {
			return nil, 0, fmt.Errorf("read log: %w", err)
		}
		if err := json.Unmarshal([]byte(fields), &record.Fields); err != nil {
			return nil, 0, fmt.Errorf("read log: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read log: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("read log: %w", err)
	}

	counter := 0
	value, err := q.Meta(ctx, loggerCounterName)
	if err != nil {
		return nil, 0, err
	}
	if value != "" {
		if counter, err = strconv.Atoi(value); err != nil {
			return nil, 0, fmt.Errorf("read log: %w", err)
		}
	}
	return records, counter, nil
}

// SaveLoggerRecord stores a record and the last number handed out.
func (q *Queue) SaveLoggerRecord(ctx context.Context, record LoggerRecord, counter int) error {
	fields, err := json.Marshal(record.Fields)
	if err != nil {
		return fmt.Errorf("save log: %w", err)
	}
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO logger_records (number, idempotency_key, fields) VALUES (?, ?, ?)
		 ON CONFLICT (number) DO UPDATE SET idempotency_key = excluded.idempotency_key, fields = excluded.fields`,
		record.Number, record.Key, string(fields))
	if err != nil {
		return fmt.Errorf("save log: %w", err)
	}
	return q.SetMeta(ctx, loggerCounterName, strconv.Itoa(counter))
}

// DeleteLoggerRecord removes a record and stores the last number handed out.
func (q *Queue) DeleteLoggerRecord(ctx context.Context, number, counter int) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM logger_records WHERE number = ?`, number); err != nil {
		return fmt.Errorf("save log: %w", err)
	}
	return q.SetMeta(ctx, loggerCounterName, strconv.Itoa(counter))
}

// N1MMRecord is what the relay remembers about a contact N1MM has sent. N1MM's id
// stays the same through edits, and its edits and deletes don't say what the
// contact's key was.
type N1MMRecord struct {
	ID       string
	Key      string
	Callsign string
	QSOTime  string
	Station  string
}

// N1MMRecord finds a contact by N1MM's id.
func (q *Queue) N1MMRecord(ctx context.Context, id string) (N1MMRecord, bool, error) {
	record := N1MMRecord{ID: id}
	err := q.db.QueryRowContext(ctx, `SELECT idempotency_key, callsign, qso_time, station FROM n1mm_records WHERE id = ?`, id).
		Scan(&record.Key, &record.Callsign, &record.QSOTime, &record.Station)
	if errors.Is(err, sql.ErrNoRows) {
		return N1MMRecord{}, false, nil
	}
	if err != nil {
		return N1MMRecord{}, false, fmt.Errorf("read N1MM record: %w", err)
	}
	return record, true, nil
}

// SaveN1MMRecord remembers a contact's current key.
func (q *Queue) SaveN1MMRecord(ctx context.Context, record N1MMRecord) error {
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO n1mm_records (id, idempotency_key, callsign, qso_time, station) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET idempotency_key = excluded.idempotency_key, callsign = excluded.callsign,
		 qso_time = excluded.qso_time, station = excluded.station`,
		record.ID, record.Key, record.Callsign, record.QSOTime, record.Station)
	if err != nil {
		return fmt.Errorf("save N1MM record: %w", err)
	}
	return nil
}

// DeleteN1MMRecord forgets a contact N1MM deleted.
func (q *Queue) DeleteN1MMRecord(ctx context.Context, id string) error {
	if _, err := q.db.ExecContext(ctx, `DELETE FROM n1mm_records WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete N1MM record: %w", err)
	}
	return nil
}
