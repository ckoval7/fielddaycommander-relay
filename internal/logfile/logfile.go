// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package logfile picks up contacts from an ADIF log on disk, which nearly every
// logger can write. It only reads: the logger never sees anything from the relay.
//
// A log can hold years of contacts, and the server refuses any outside the event
// for good, so only contacts in the event's dates are queued. Until the relay
// knows those dates the watcher waits, which is safe because the file keeps the
// contacts.
//
// Re-reading is harmless, because the queue recognizes a contact it already holds
// and queues a changed one as a correction. So anything other than a plain append
// starts over from the top. A contact that disappears from the file isn't
// deleted: a file can shrink for many reasons, and the file can't say which.
package logfile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/adif"
	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

// SourceName identifies this source in the relay's records.
const SourceName = "logfile"

const (
	pollInterval = 2 * time.Second
	// readWindow is how much is read at a time. The file is closed between windows,
	// so it's never held open while contacts are being saved.
	readWindow = 1 << 20
	// sampleSize is how much of the text before the read position is compared, to
	// tell a file that grew from one that was rewritten.
	sampleSize = 256
	// failureLimit is how many failed reads in a row count as a problem. Loggers can
	// hold a file for a moment while saving.
	failureLimit = 5
)

// State is what a watcher is doing.
type State string

// The states of a watcher.
const (
	Starting        State = "starting"
	WaitingForEvent State = "waiting_for_event"
	Watching        State = "watching"
	Missing         State = "missing"
	Unreadable      State = "unreadable"
	NotADIF         State = "not_adif"
)

// Status is a snapshot for the status page.
type Status struct {
	Path  string
	State State
	// Contacts is how many contacts in the event's dates the file holds.
	Contacts int
	// Outside is how many it holds from other dates, which aren't sent.
	Outside int
	// LastRead is when contacts last came from the file.
	LastRead time.Time
}

// Sink stores what the watcher reads. Put must have made the contact durable
// before it returns.
type Sink interface {
	Put(ctx context.Context, c contact.Contact) (queued bool, err error)
	PutInvalid(ctx context.Context, source, reason, raw string) error
	// EventPeriod returns the event's dates, or false until the relay has learned
	// them from the server.
	EventPeriod(ctx context.Context) (start, end time.Time, known bool, err error)
}

// Watcher follows one log file.
type Watcher struct {
	path   string
	sink   Sink
	notify func()
	log    *slog.Logger

	// Shortened by tests.
	poll   time.Duration
	window int

	// Only Run's goroutine uses these.
	start, end time.Time
	offset     int64
	sample     []byte
	size       int64
	modified   time.Time
	read       bool
	sawTag     bool
	failures   int

	statusMu sync.Mutex
	status   Status
}

// New returns a Watcher for path. notify, when set, is called after contacts are
// queued.
func New(path string, sink Sink, notify func(), log *slog.Logger) *Watcher {
	return &Watcher{
		path: path, sink: sink, notify: notify, log: log,
		poll: pollInterval, window: readWindow,
		status: Status{Path: path, State: Starting},
	}
}

// Status returns the current status.
func (w *Watcher) Status() Status {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	return w.status
}

// Run follows the file until ctx is canceled. Most checks only look at the file's
// size and time, so an idle log costs next to nothing.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		if err := w.check(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("reading a log file failed", "path", w.path, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Watcher) check(ctx context.Context) error {
	start, end, known, err := w.sink.EventPeriod(ctx)
	if err != nil {
		return err
	}
	if !known {
		w.setState(WaitingForEvent)
		w.forget()
		return nil
	}
	// New dates can change which contacts count, so read it all again.
	if !start.Equal(w.start) || !end.Equal(w.end) {
		w.start, w.end = start, end
		w.forget()
	}

	info, err := statFile(w.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		w.setState(Missing)
		w.forget()
		return nil
	case err != nil:
		return w.failed(err)
	case !info.Mode().IsRegular():
		w.setState(Unreadable)
		w.forget()
		return nil
	}

	if w.read && info.Size() == w.size && info.ModTime().Equal(w.modified) {
		return nil
	}
	if w.read && info.Size() > w.size {
		grew, err := w.grewFromTheEnd(info.Size())
		if err != nil {
			return w.failed(err)
		}
		if !grew {
			w.forget()
		}
	} else {
		w.forget()
	}

	if err := w.readNew(ctx); err != nil {
		return w.failed(err)
	}
	w.failures = 0
	w.read, w.size, w.modified = true, info.Size(), info.ModTime()
	if info.Size() > 0 && !w.sawTag {
		w.setState(NotADIF)
	} else {
		w.setState(Watching)
	}
	return nil
}

// forget starts over from the top of the file.
func (w *Watcher) forget() {
	w.read, w.sawTag, w.offset, w.sample, w.size, w.modified = false, false, 0, nil, 0, time.Time{}
	w.setStatus(func(status *Status) { status.Contacts, status.Outside = 0, 0 })
}

func (w *Watcher) failed(err error) error {
	w.failures++
	if w.failures >= failureLimit {
		w.setState(Unreadable)
	}
	return err
}

// grewFromTheEnd reports whether the text before the read position is still what
// was read there, which means the logger only added to the file.
func (w *Watcher) grewFromTheEnd(size int64) (bool, error) {
	if w.offset > size {
		return false, nil
	}
	if len(w.sample) == 0 {
		return true, nil
	}
	found, err := w.readAt(w.offset-int64(len(w.sample)), len(w.sample))
	if err != nil {
		return false, err
	}
	return bytes.Equal(found, w.sample), nil
}

func (w *Watcher) readNew(ctx context.Context) error {
	queued := false
	defer func() {
		if queued && w.notify != nil {
			w.notify()
		}
	}()

	for {
		data, err := w.readAt(w.offset, w.window)
		if err != nil {
			return err
		}
		records, consumed, sawTag := adif.Scan(data)
		w.sawTag = w.sawTag || sawTag

		found, outside, any, err := w.store(ctx, records)
		queued = queued || any
		if err != nil {
			return err
		}
		w.setStatus(func(status *Status) {
			status.Contacts += found
			status.Outside += outside
			if any {
				status.LastRead = time.Now()
			}
		})

		short := len(data) < w.window
		if consumed == 0 && !short {
			// A whole window with no end of record is no log the relay can read. Skip
			// it so the rest of the file still gets read.
			w.log.Warn("skipping text in a log file with no record end", "path", w.path, "offset", w.offset)
			consumed = len(data)
		}
		if consumed > 0 {
			w.offset += int64(consumed)
			w.sample = bytes.Clone(data[max(0, consumed-sampleSize):consumed])
		}
		if short {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// store saves records. The caller only moves past them once this succeeds, and
// saving one again later changes nothing.
func (w *Watcher) store(ctx context.Context, records []adif.Record) (found, outside int, queued bool, err error) {
	for _, record := range records {
		candidate, when, err := adif.ToContact(record.Fields)
		if err == nil && (when.Before(w.start) || when.After(w.end)) {
			outside++
			continue
		}
		var normalized contact.Contact
		if err == nil {
			normalized, _, err = contact.Normalize(candidate)
		}
		if err != nil {
			if !errors.Is(err, contact.ErrInvalid) {
				return 0, 0, queued, err
			}
			if err := w.sink.PutInvalid(ctx, SourceName, err.Error(), record.Raw); err != nil {
				return 0, 0, queued, err
			}
			continue
		}

		isNew, err := w.sink.Put(ctx, normalized)
		if err != nil {
			return 0, 0, queued, err
		}
		found++
		queued = queued || isNew
	}
	return found, outside, queued, nil
}

func (w *Watcher) readAt(offset int64, length int) ([]byte, error) {
	file, err := openShared(w.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data := make([]byte, length)
	read, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:read], nil
}

func (w *Watcher) setState(state State) {
	w.setStatus(func(status *Status) { status.State = state })
}

func (w *Watcher) setStatus(change func(*Status)) {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	change(&w.status)
}
