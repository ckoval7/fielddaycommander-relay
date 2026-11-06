// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package queue

import (
	"context"
	"encoding/json"
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
