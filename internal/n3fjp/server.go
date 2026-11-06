// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

const (
	// LocalListenAddress serves N3FJP running on this computer only.
	LocalListenAddress = "127.0.0.1:1000"
	// NetworkListenAddress lets other PCs connect. N3FJP's protocol has no
	// password, so the relay doesn't use it unless someone turns it on.
	NetworkListenAddress = "0.0.0.0:1000"

	serverName       = "FDRELAY"
	maxClients       = 64
	clientWriteLimit = 30 * time.Second
	// maxPendingBytes is what one client can have waiting. A full reload of a large
	// Field Day log is a few megabytes.
	maxPendingBytes  = 16 << 20
	listenRetry      = 5 * time.Second
	bandStatusLimit  = 256
	bandStatusMaxAge = 15 * time.Minute
)

// RecordStore keeps the log. N3FJP clients hold no log of their own, so numbers
// and whole records have to survive a restart.
type RecordStore interface {
	LoggerRecords(ctx context.Context) ([]queue.LoggerRecord, int, error)
	SaveLoggerRecord(ctx context.Context, record queue.LoggerRecord, counter int) error
	DeleteLoggerRecord(ctx context.Context, number, counter int) error
}

// ServerStatus is a snapshot for the status page.
type ServerStatus struct {
	Listening bool
	Clients   int
	Records   int
}

// Server is the relay acting as an N3FJP network server, so N3FJP PCs log straight
// into it, the way they do with a local Field Day Commander. It owns the log and
// its numbering, and keeps working with no internet connection.
//
// A contact is on disk and queued before any client sees it echoed. A client only
// lists its own contact once the echo arrives, so a failed store shows up at the
// operator's keyboard with the contact still in the entry boxes.
type Server struct {
	address string
	sink    Sink
	store   RecordStore
	notify  func()
	log     *slog.Logger

	mu         sync.Mutex
	records    map[int]record
	counter    int
	clients    map[*serverClient]struct{}
	listening  bool
	loaded     bool
	bandStatus map[string]bandEntry
}

type bandEntry struct {
	message string
	at      time.Time
}

// NewServer returns a Server for address.
func NewServer(address string, sink Sink, store RecordStore, notify func(), log *slog.Logger) *Server {
	return &Server{
		address: address, sink: sink, store: store, notify: notify, log: log,
		records: make(map[int]record), clients: make(map[*serverClient]struct{}), bandStatus: make(map[string]bandEntry),
	}
}

// Status returns the current status.
func (s *Server) Status() ServerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ServerStatus{Listening: s.listening, Clients: len(s.clients), Records: len(s.records)}
}

// Ready reports whether the log has loaded from disk.
func (s *Server) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

// Run serves until ctx is canceled, retrying if the port is taken: another program
// may be holding it for a moment, and contacts can't arrive until it's free.
func (s *Server) Run(ctx context.Context) {
	if !s.load(ctx) {
		return
	}
	for ctx.Err() == nil {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.address)
		if err != nil {
			s.log.Warn("couldn't open the N3FJP port", "address", s.address, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(listenRetry):
				continue
			}
		}
		s.serve(ctx, listener)
	}
}

func (s *Server) load(ctx context.Context) bool {
	for ctx.Err() == nil {
		records, counter, err := s.store.LoggerRecords(ctx)
		if err == nil {
			s.mu.Lock()
			for _, stored := range records {
				s.records[stored.Number] = recordFromFields(stored.Key, stored.Fields)
			}
			s.counter = counter
			s.loaded = true
			s.mu.Unlock()
			return true
		}
		s.log.Error("reading the N3FJP log failed", "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(listenRetry):
		}
	}
	return false
}

func (s *Server) serve(ctx context.Context, listener net.Listener) {
	s.setListening(true)
	defer s.setListening(false)

	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()

	var running sync.WaitGroup
	defer running.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("the N3FJP port stopped accepting", "error", err)
			}
			_ = listener.Close()
			return
		}
		client := newServerClient(conn)
		if !s.addClient(client) {
			_ = conn.Close()
			continue
		}
		running.Go(func() {
			s.runClient(ctx, client)
			s.removeClient(client)
		})
	}
}

func (s *Server) setListening(listening bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listening = listening
}

func (s *Server) addClient(client *serverClient) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) >= maxClients {
		return false
	}
	s.clients[client] = struct{}{}
	return true
}

func (s *Server) removeClient(client *serverClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, client)
}

// serverClient queues writes per client, so a PC stuck behind a dialog can't hold
// up the others. A client too far behind is dropped, and reloads when it returns.
type serverClient struct {
	conn    net.Conn
	done    chan struct{}
	once    sync.Once
	wake    chan struct{}
	mu      sync.Mutex
	pending [][]byte
	bytes   int
}

func newServerClient(conn net.Conn) *serverClient {
	return &serverClient{conn: conn, done: make(chan struct{}), wake: make(chan struct{}, 1)}
}

func (c *serverClient) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *serverClient) send(message string) {
	data := encode(message)

	c.mu.Lock()
	if c.bytes+len(data) > maxPendingBytes {
		c.mu.Unlock()
		c.close()
		return
	}
	c.pending = append(c.pending, data)
	c.bytes += len(data)
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// writeLoop sends each message as one whole write, as N3FJP clients expect.
func (c *serverClient) writeLoop() {
	for {
		c.mu.Lock()
		batch := c.pending
		c.pending = nil
		c.mu.Unlock()

		for _, data := range batch {
			if err := c.conn.SetWriteDeadline(time.Now().Add(clientWriteLimit)); err != nil {
				c.close()
				return
			}
			if _, err := c.conn.Write(data); err != nil {
				c.close()
				return
			}
			c.mu.Lock()
			c.bytes -= len(data)
			c.mu.Unlock()
		}
		if len(batch) > 0 {
			continue
		}

		select {
		case <-c.wake:
		case <-c.done:
			return
		}
	}
}

func (s *Server) runClient(ctx context.Context, client *serverClient) {
	defer client.close()
	stop := context.AfterFunc(ctx, client.close)
	defer stop()

	go client.writeLoop()

	var frames decoder
	buffer := make([]byte, readBufferSize)
	for {
		count, err := client.conn.Read(buffer)
		if count > 0 {
			messages, frameErr := frames.feed(buffer[:count])
			for _, message := range messages {
				if err := s.receive(ctx, client, message); err != nil {
					s.log.Error("handling an N3FJP message failed", "error", err)
				}
			}
			if frameErr != nil {
				s.log.Warn("dropping a connection that isn't N3FJP", "error", frameErr)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) receive(ctx context.Context, client *serverClient, message string) error {
	upper := upperASCII(message)
	trimmed := strings.TrimSpace(upper)

	switch {
	case strings.HasPrefix(trimmed, "<BOR><MESG>"):
		s.broadcast(cleanChat(message))
	case strings.Contains(upper, "<BOR><NTWK><OPEN>"):
		s.sendFullLog(client)
	case strings.Contains(upper, "<BOR><NTWK><CHECK>"):
		s.sendCheck(client)
	case strings.Contains(upper, "<NTWK>"):
		return s.handleTransaction(ctx, client, message)
	case strings.Contains(upper, "<BAMS>"):
		s.relayBandStatus(message)
	case strings.Contains(upper, "<WHO>"):
		s.sendBandStatus(client)
	case strings.Contains(upper, "<DXSP>"):
		s.broadcast(message)
	}
	// Clock syncs stop here: passing one on would set every PC's clock on the say
	// of any one of them.
	return nil
}

func (s *Server) handleTransaction(ctx context.Context, client *serverClient, message string) error {
	from := cleanValue(field(message, "FROM"))
	data := field(message, "XMLDATA")

	switch strings.ToUpper(strings.TrimSpace(field(message, "TRANSACTION"))) {
	case "ADD":
		return s.clientAdd(ctx, client, from, data)
	case "EDIT":
		return s.clientEdit(ctx, client, from, data)
	case "DELETE":
		return s.clientDelete(ctx, client, data)
	}
	return nil
}

func (s *Server) clientAdd(ctx context.Context, client *serverClient, from, raw string) error {
	fields := parseRecord(raw)
	c, err := toContact(fields, from)
	if err != nil {
		s.reject(ctx, client, from, fields["CALL"], err, raw)
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.queueContact(ctx, c); err != nil {
		s.rejectFailure(client, from, fields["CALL"])
		return err
	}
	number := s.counter + 1
	fields["PRIMARYKEY"] = strconv.Itoa(number)
	if err := s.storeLocked(ctx, number, c.IdempotencyKey, fields, number); err != nil {
		s.rejectFailure(client, from, fields["CALL"])
		return err
	}
	s.broadcastLocked(serverTransaction("ADD", recordText(fields)))
	return nil
}

func (s *Server) clientEdit(ctx context.Context, client *serverClient, from, raw string) error {
	fields := parseRecord(raw)
	number, err := strconv.Atoi(fields["PRIMARYKEY"])
	if err != nil || number <= 0 {
		s.sendCheck(client)
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, known := s.records[number]
	if !known {
		client.send(checkMessage(len(s.records), s.counter))
		return nil
	}

	c, err := toContact(fields, from)
	if err != nil {
		s.sendChat(client, from, fmt.Sprintf(rejectEditText, cleanValue(fields["CALL"])))
		// Sent either way: it puts the sender's copy back.
		client.send(serverTransaction("EDIT", recordText(previous.fields)))
		return nil
	}
	if previous.key != c.IdempotencyKey && !keyInUse(s.records, number, previous.key) {
		c.ReplacesKey = previous.key
	}
	if err := s.queueContact(ctx, c); err != nil {
		s.sendChat(client, from, fmt.Sprintf(rejectEditText, cleanValue(fields["CALL"])))
		client.send(serverTransaction("EDIT", recordText(previous.fields)))
		return err
	}
	if err := s.storeLocked(ctx, number, c.IdempotencyKey, fields, s.counter); err != nil {
		return err
	}
	s.broadcastLocked(serverTransaction("EDIT", recordText(fields)))
	return nil
}

func (s *Server) clientDelete(ctx context.Context, client *serverClient, raw string) error {
	number, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, known := s.records[number]
	if !known {
		client.send(checkMessage(len(s.records), s.counter))
		return nil
	}
	return s.deleteLocked(ctx, number, previous, true)
}

// deleteLocked removes a record, and queues the contact's delete when asked and
// when no other record still holds that contact.
func (s *Server) deleteLocked(ctx context.Context, number int, previous record, upload bool) error {
	remaining := keyInUse(s.records, number, previous.key)
	if upload && previous.key != "" && !remaining {
		if err := s.queueContact(ctx, deleteOf(previous)); err != nil {
			return err
		}
	}
	counter := s.counter
	if len(s.records) == 1 {
		counter = 0
	}
	if err := s.store.DeleteLoggerRecord(ctx, number, counter); err != nil {
		return err
	}
	delete(s.records, number)
	s.counter = counter
	s.broadcastLocked(serverTransaction("DELETE", strconv.Itoa(number)))
	return nil
}

func (s *Server) queueContact(ctx context.Context, c contact.Contact) error {
	queued, err := s.sink.Put(ctx, c)
	if err != nil {
		return fmt.Errorf("storing a contact: %w", err)
	}
	if queued && s.notify != nil {
		s.notify()
	}
	return nil
}

func (s *Server) storeLocked(ctx context.Context, number int, key string, fields map[string]string, counter int) error {
	if err := s.store.SaveLoggerRecord(ctx, queue.LoggerRecord{Number: number, Key: key, Fields: fields}, counter); err != nil {
		return err
	}
	s.records[number] = recordFromFields(key, fields)
	s.counter = counter
	return nil
}

// Add puts a contact from the server in the log, unless a record already has it.
func (s *Server) Add(ctx context.Context, c contact.Contact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.numberOfLocked(c.IdempotencyKey) != 0 {
		return nil
	}
	return s.addLocked(ctx, c)
}

func (s *Server) addLocked(ctx context.Context, c contact.Contact) error {
	number := s.counter + 1
	fields := fieldsFor(c)
	fields["PRIMARYKEY"] = strconv.Itoa(number)
	fields = parseRecord(recordText(fields))

	known, err := toContact(fields, serverName)
	if err != nil {
		return fmt.Errorf("the contact can't be written: %w", err)
	}
	if err := s.sink.Remember(ctx, known); err != nil {
		return err
	}
	if err := s.storeLocked(ctx, number, known.IdempotencyKey, fields, number); err != nil {
		return err
	}
	s.broadcastLocked(serverTransaction("ADD", recordText(fields)))
	return nil
}

// Edit applies what changed on the server to the record with this key.
func (s *Server) Edit(ctx context.Context, key string, before, after contact.Contact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	number := s.numberOfLocked(key)
	if number == 0 {
		return s.addLocked(ctx, after)
	}
	fields, changed := mergedFields(s.records[number].fields, before, after)
	if !changed {
		return nil
	}
	fields["PRIMARYKEY"] = strconv.Itoa(number)
	fields = parseRecord(recordText(fields))

	known, err := toContact(fields, serverName)
	if err != nil {
		return fmt.Errorf("the contact can't be written: %w", err)
	}
	if err := s.sink.Remember(ctx, known); err != nil {
		return err
	}
	if err := s.storeLocked(ctx, number, known.IdempotencyKey, fields, s.counter); err != nil {
		return err
	}
	s.broadcastLocked(serverTransaction("EDIT", recordText(fields)))
	return nil
}

// Delete removes every record with this key.
func (s *Server) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, number := range s.numbersOfLocked(key) {
		previous := s.records[number]
		if err := s.sink.Remember(ctx, deleteOf(previous)); err != nil {
			return err
		}
		if err := s.deleteLocked(ctx, number, previous, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) numberOfLocked(key string) int {
	if numbers := s.numbersOfLocked(key); len(numbers) > 0 {
		return numbers[0]
	}
	return 0
}

func (s *Server) numbersOfLocked(key string) []int {
	var numbers []int
	for number, candidate := range s.records {
		if key != "" && candidate.key == key {
			numbers = append(numbers, number)
		}
	}
	slices.Sort(numbers)
	return numbers
}

func (s *Server) sendFullLog(client *serverClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client.send("<BOR><NTWK><TRANSACTION>CLEAR</TRANSACTION><EOR>")
	for _, number := range slices.Sorted(maps.Keys(s.records)) {
		client.send(serverTransaction("GET", recordText(s.records[number].fields)))
	}
	client.send("<BOR><NTWK><TRANSACTION>LIST</TRANSACTION><EOR>")
}

func (s *Server) sendCheck(client *serverClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	client.send(checkMessage(len(s.records), s.counter))
}

func checkMessage(count, last int) string {
	return serverTransaction("CHECK", "<COUNT>"+strconv.Itoa(count)+"</COUNT><LAST>"+strconv.Itoa(last)+"</LAST>")
}

func serverTransaction(kind, data string) string {
	return "<BOR><NTWK><FROM>" + serverName + "</FROM><TRANSACTION>" + kind + "</TRANSACTION><XMLDATA>" + data + "</XMLDATA><EOR>"
}

func (s *Server) broadcast(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcastLocked(message)
}

func (s *Server) broadcastLocked(message string) {
	for client := range s.clients {
		client.send(message)
	}
}

func (s *Server) relayBandStatus(message string) {
	station := cleanValue(field(message, "STATION"))
	clean := "<BOR><BAMS><STATION>" + station + "</STATION><BAND>" + cleanValue(field(message, "BAND")) + "</BAND><MODE>" + cleanValue(field(message, "MODE")) + "</MODE><EOR>"

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for name, entry := range s.bandStatus {
		if now.Sub(entry.at) > bandStatusMaxAge {
			delete(s.bandStatus, name)
		}
	}
	if station != "" && (len(s.bandStatus) < bandStatusLimit || s.bandStatus[station].message != "") {
		s.bandStatus[station] = bandEntry{message: clean, at: now}
	}
	s.broadcastLocked(clean)
}

func (s *Server) sendBandStatus(client *serverClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.bandStatus {
		client.send(entry.message)
	}
}

// cleanChat rebuilds a chat with its text made safe. N3FJP clients read by the
// first closing tag and by <EOR>, so tags in the text would break it for everyone.
func cleanChat(message string) string {
	upper := upperASCII(message)
	text := ""
	if start, end := strings.Index(upper, "<MSGTXT>"), strings.LastIndex(upper, "</MSGTXT>"); start >= 0 && end > start {
		text = message[start+len("<MSGTXT>") : end]
	}
	return "<BOR><MESG><TO>" + cleanValue(field(message, "TO")) + "</TO><FROM>" + cleanValue(field(message, "FROM")) + "</FROM><MSGTXT>" + cleanValue(text) + "</MSGTXT><EOR>"
}

func (s *Server) sendChat(client *serverClient, to, text string) {
	client.send("<BOR><MESG><TO>" + cleanValue(to) + "</TO><FROM>" + serverName + "</FROM><MSGTXT>" + cleanValue(text) + "</MSGTXT><EOR>")
}

// reject tells a PC its contact wasn't saved. Without an echo, the client also
// puts the contact back in its entry boxes.
func (s *Server) reject(ctx context.Context, client *serverClient, from, call string, cause error, raw string) {
	s.sendChat(client, from, fmt.Sprintf(rejectAddText, cleanValue(call)))
	if !errors.Is(cause, contact.ErrInvalid) {
		return
	}
	if len(raw) > maxRawInvalid {
		raw = raw[:maxRawInvalid]
	}
	if err := s.sink.PutInvalid(ctx, SourceName, cause.Error(), raw); err != nil {
		s.log.Error("keeping an invalid record failed", "error", err)
	}
}

func (s *Server) rejectFailure(client *serverClient, from, call string) {
	s.sendChat(client, from, fmt.Sprintf(rejectStoreText, cleanValue(call)))
}

func recordFromFields(key string, fields map[string]string) record {
	c, err := toContact(fields, "")
	if err != nil {
		return record{key: key, fields: fields}
	}
	return record{key: key, callsign: c.Callsign, qsoTime: c.QSOTime, station: c.Station, fields: fields}
}
