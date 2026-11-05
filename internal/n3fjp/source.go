// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

// SourceName identifies this source in the relay's records.
const SourceName = "n3fjp"

// DefaultAddress is where the source connects unless the user sets another
// address. N3FJP has no authentication, so nothing else is assumed to be safe.
const DefaultAddress = "127.0.0.1:1000"

// The only two messages the relay ever sends. An N3FJP server in TCP mode answers
// both on the asking connection alone. It relays anything else to every PC, so
// the source has no way to send an ADD, EDIT, DELETE, clock sync or chat. It also
// never announces a station with BAMS, which would put the relay on everyone's
// band status display.
const (
	messageOpen  = "<BOR><NTWK><OPEN>"
	messageCheck = "<BOR><NTWK><CHECK>"
)

const (
	dialTimeout    = 10 * time.Second
	writeTimeout   = 10 * time.Second
	checkInterval  = 30 * time.Second
	minReloadGap   = time.Minute
	reconnectStart = time.Second
	reconnectMax   = 30 * time.Second
	readBufferSize = 8192
	maxRecords     = 200_000
	maxRawInvalid  = 2048
)

// Sink stores what the source reads. Put must have made the contact durable
// before it returns.
type Sink interface {
	Put(ctx context.Context, c contact.Contact) (queued bool, err error)
	PutInvalid(ctx context.Context, source, reason, raw string) error
}

// Status is a snapshot for the status page.
type Status struct {
	Connected bool
	// LogAnswered is whether the server has answered with log traffic on this
	// connection. A server in file-share mode never does, and has no contacts to
	// offer over TCP.
	LogAnswered  bool
	LastMessage  time.Time
	RecordsKnown int
}

// Source connects to an N3FJP network server and passes contacts to a Sink.
type Source struct {
	address string
	sink    Sink
	notify  func()
	log     *slog.Logger

	// Waits, shortened by tests.
	check        time.Duration
	reloadGap    time.Duration
	reconnectMin time.Duration
	reconnectCap time.Duration

	statusMu sync.Mutex
	status   Status
}

// NewSource returns a Source for address. notify, when set, is called after a
// contact is queued.
func NewSource(address string, sink Sink, notify func(), log *slog.Logger) *Source {
	return &Source{
		address:      address,
		sink:         sink,
		notify:       notify,
		log:          log,
		check:        checkInterval,
		reloadGap:    minReloadGap,
		reconnectMin: reconnectStart,
		reconnectCap: reconnectMax,
	}
}

// Status returns the current status.
func (s *Source) Status() Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

// Run stays connected until ctx is canceled, reconnecting after any failure.
// Each connection starts by reading the whole log, which is how the relay
// catches up on contacts logged while it wasn't connected.
func (s *Source) Run(ctx context.Context) {
	wait := s.reconnectMin
	for ctx.Err() == nil {
		started := time.Now()
		err := s.session(ctx)
		s.setStatus(func(status *Status) {
			status.Connected = false
			status.LogAnswered = false
		})
		if ctx.Err() != nil {
			return
		}
		s.log.Warn("disconnected from N3FJP", "address", s.address, "error", err)

		// A connection that stayed up a while was healthy, so start over.
		if time.Since(started) > s.reconnectCap {
			wait = s.reconnectMin
		}
		delay := wait + time.Duration(rand.Int64N(int64(wait)/5+1))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		wait = min(wait*2, s.reconnectCap)
	}
}

func (s *Source) session(ctx context.Context) error {
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.address)
	if err != nil {
		return err
	}
	defer conn.Close()

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-sessionCtx.Done()
		_ = conn.Close()
	}()

	messages := make(chan string)
	readErr := make(chan error, 1)
	go func() { readErr <- readMessages(sessionCtx, conn, messages) }()

	state := &session{source: s, conn: conn, records: make(map[int]record)}
	s.setStatus(func(status *Status) { status.Connected = true })
	if err := state.reload(); err != nil {
		return err
	}

	ticker := time.NewTicker(s.check)
	defer ticker.Stop()

	for {
		select {
		case <-sessionCtx.Done():
			return sessionCtx.Err()
		case err := <-readErr:
			return err
		case <-ticker.C:
			// A server in file-share mode relays CHECK to every PC, and each of
			// them beeps and shows a network warning. So nothing repeats until
			// the server has shown it's in TCP mode.
			if !state.answered {
				continue
			}
			if err := state.send(messageCheck); err != nil {
				return err
			}
		case message := <-messages:
			if err := state.handle(sessionCtx, message); err != nil {
				return err
			}
			s.setStatus(func(status *Status) {
				status.LastMessage = time.Now()
				status.RecordsKnown = len(state.records)
				status.LogAnswered = state.answered
			})
		}
	}
}

func readMessages(ctx context.Context, conn net.Conn, out chan<- string) error {
	var frames decoder
	buffer := make([]byte, readBufferSize)
	for {
		count, err := conn.Read(buffer)
		if count > 0 {
			messages, frameErr := frames.feed(buffer[:count])
			for _, message := range messages {
				select {
				case out <- message:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if frameErr != nil {
				return frameErr
			}
		}
		if err != nil {
			return err
		}
	}
}

// record is what the source remembers about a numbered record, so it can name
// the contact a later EDIT or DELETE refers to. The key is empty for a record
// that couldn't become a contact.
type record struct {
	key      string
	callsign string
	qsoTime  string
	station  string
}

// session is one connection's view of the log.
//
// Clients number records themselves: each counts the ADDs it sees, and takes
// the numbers in a full reload as they come. Those numbers only ever connect an
// EDIT or DELETE to a record during this connection. They aren't stable, so they
// are never part of a contact's identity.
type session struct {
	source   *Source
	conn     net.Conn
	records  map[int]record
	loading  map[int]record
	counter  int
	lastOpen time.Time
	answered bool
}

func (s *session) send(message string) error {
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	_, err := s.conn.Write(encode(message))
	return err
}

// reload asks for the whole log, at most once per reload gap.
func (s *session) reload() error {
	if !s.lastOpen.IsZero() && time.Since(s.lastOpen) < s.source.reloadGap {
		return nil
	}
	s.lastOpen = time.Now()
	return s.send(messageOpen)
}

// current is the record table that incoming records belong to.
func (s *session) current() map[int]record {
	if s.loading != nil {
		return s.loading
	}
	return s.records
}

func (s *session) handle(ctx context.Context, message string) error {
	data := field(message, "XMLDATA")
	from := field(message, "FROM")

	kind := transaction(message)
	if kind != "" {
		s.answered = true
	}

	switch kind {
	case "CLEAR":
		// Only starts a new table. A reload is never treated as deletions: a
		// truncated reload, or a PC pointed at the wrong log, would otherwise
		// wipe contacts from the server.
		s.loading = make(map[int]record)
		s.counter = 0
	case "GET":
		fields := parseRecord(data)
		number, err := strconv.Atoi(fields["PRIMARYKEY"])
		if err != nil || number <= 0 {
			s.counter++
			number = s.counter
		}
		s.counter = number
		return s.store(ctx, number, fields, from, data)
	case "LIST":
		if s.loading != nil {
			s.records = s.loading
			s.loading = nil
		}
	case "ADD":
		s.counter++
		return s.store(ctx, s.counter, parseRecord(data), from, data)
	case "EDIT":
		return s.edit(ctx, data, from)
	case "DELETE":
		return s.delete(ctx, data)
	case "CHECK":
		return s.compare(message)
	}
	// Chat, band status, clock sync, spots and anything else aren't contacts.
	return nil
}

func (s *session) store(ctx context.Context, number int, fields map[string]string, from, raw string) error {
	table := s.current()
	if len(table) >= maxRecords {
		return fmt.Errorf("n3fjp: more than %d records", maxRecords)
	}

	c, err := toContact(fields, from)
	if err != nil {
		table[number] = record{}
		return s.invalid(ctx, err, raw)
	}
	table[number] = recordFor(c)
	return s.put(ctx, c)
}

// edit updates a contact. When the edit changed what identifies the contact, its
// old key goes along, so the server changes that contact and doesn't add another.
func (s *session) edit(ctx context.Context, raw, from string) error {
	fields := parseRecord(raw)
	number, err := strconv.Atoi(fields["PRIMARYKEY"])
	if err != nil || number <= 0 {
		return s.reload()
	}

	table := s.current()
	previous, known := table[number]

	c, err := toContact(fields, from)
	if err != nil {
		return s.invalid(ctx, err, raw)
	}
	if previous.key != c.IdempotencyKey && !keyInUse(table, number, previous.key) {
		c.ReplacesKey = previous.key
	}
	table[number] = recordFor(c)

	if err := s.put(ctx, c); err != nil {
		return err
	}
	if !known {
		return s.reload()
	}
	return nil
}

// delete passes on a delete of a record this session can name. A number it
// doesn't know means its table is out of step, so it reloads and deletes nothing.
func (s *session) delete(ctx context.Context, raw string) error {
	number, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}

	table := s.current()
	previous, known := table[number]
	if !known {
		return s.reload()
	}
	delete(table, number)
	// Numbering starts over when the log empties. Otherwise a deleted number
	// isn't reused unless the server says so in a CHECK.
	if len(table) == 0 {
		s.counter = 0
	}

	if previous.key == "" || keyInUse(table, number, previous.key) {
		return nil
	}
	return s.put(ctx, contact.Contact{
		IdempotencyKey: previous.key,
		Deleted:        true,
		Callsign:       previous.callsign,
		QSOTime:        previous.qsoTime,
		Station:        previous.station,
	})
}

// compare checks the count and last number the server reports against this
// session's own. A client that differs asks for the whole log again.
func (s *session) compare(message string) error {
	if s.loading != nil {
		return nil
	}
	count, countErr := strconv.Atoi(strings.TrimSpace(field(message, "COUNT")))
	last, lastErr := strconv.Atoi(strings.TrimSpace(field(message, "LAST")))
	if countErr != nil || lastErr != nil {
		return nil
	}
	if count == len(s.records) && last == s.counter {
		return nil
	}
	// Field Day Commander's server reuses a deleted highest number. Taking the
	// server's number keeps the next ADD right if the reload has to wait.
	if count == len(s.records) {
		s.counter = last
	}
	return s.reload()
}

// keyInUse reports whether a record other than number has this key. An operator
// who re-enters a contact leaves two records for one contact on the server, and
// that contact stays until the logger no longer has either.
func keyInUse(table map[int]record, number int, key string) bool {
	for other, candidate := range table {
		if other != number && candidate.key == key {
			return true
		}
	}
	return false
}

func (s *session) put(ctx context.Context, c contact.Contact) error {
	queued, err := s.source.sink.Put(ctx, c)
	if err != nil {
		// Ends the connection. The reload on reconnect brings the contact back,
		// so a full disk costs a delay and not a contact.
		return fmt.Errorf("storing a contact: %w", err)
	}
	if queued && s.source.notify != nil {
		s.source.notify()
	}
	return nil
}

func (s *session) invalid(ctx context.Context, cause error, raw string) error {
	if !errors.Is(cause, contact.ErrInvalid) {
		return cause
	}
	if len(raw) > maxRawInvalid {
		raw = raw[:maxRawInvalid]
	}
	if err := s.source.sink.PutInvalid(ctx, SourceName, cause.Error(), raw); err != nil {
		return fmt.Errorf("keeping an invalid record: %w", err)
	}
	return nil
}

func recordFor(c contact.Contact) record {
	return record{key: c.IdempotencyKey, callsign: c.Callsign, qsoTime: c.QSOTime, station: c.Station}
}

func (s *Source) setStatus(change func(*Status)) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	change(&s.status)
}
