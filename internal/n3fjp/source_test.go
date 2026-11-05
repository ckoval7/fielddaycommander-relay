// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

// fakeSink records what the source stores.
type fakeSink struct {
	mu       sync.Mutex
	contacts []contact.Contact
	invalid  []string
	fail     error
}

func (f *fakeSink) Put(_ context.Context, c contact.Contact) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return false, f.fail
	}
	f.contacts = append(f.contacts, c)
	return true, nil
}

func (f *fakeSink) PutInvalid(_ context.Context, _, reason, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalid = append(f.invalid, reason)
	return nil
}

func (f *fakeSink) snapshot() []contact.Contact {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.contacts)
}

func (f *fakeSink) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

// fakeServer plays an N3FJP network server for one source.
type fakeServer struct {
	t        *testing.T
	listener net.Listener
	mu       sync.Mutex
	conn     net.Conn
	received []string
	accepts  atomic.Int32
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeServer{t: t, listener: listener}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.mu.Lock()
			server.conn = conn
			server.mu.Unlock()
			server.accepts.Add(1)
			go server.read(conn)
		}
	}()
	return server
}

func (f *fakeServer) read(conn net.Conn) {
	var frames decoder
	buffer := make([]byte, 4096)
	for {
		count, err := conn.Read(buffer)
		if err != nil {
			return
		}
		messages, _ := frames.feed(buffer[:count])
		f.mu.Lock()
		f.received = append(f.received, messages...)
		f.mu.Unlock()
	}
}

func (f *fakeServer) send(messages ...string) {
	f.t.Helper()
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	for _, message := range messages {
		if _, err := conn.Write(encode(message)); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fakeServer) dropConnection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.conn.Close()
}

func (f *fakeServer) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.received)
}

func (f *fakeServer) count(message string) int {
	total := 0
	for _, got := range f.sent() {
		if got == message {
			total++
		}
	}
	return total
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func runSource(t *testing.T, server *fakeServer, sink *fakeSink, configure func(*Source)) *Source {
	t.Helper()
	source := NewSource(server.listener.Addr().String(), sink, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	source.reconnectMin = time.Millisecond
	source.reconnectCap = 5 * time.Millisecond
	source.reloadGap = 0
	if configure != nil {
		configure(source)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		source.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	eventually(t, "the source to ask for the log", func() bool { return server.count(messageOpen) >= 1 })
	return source
}

func record2(call string, number int, extra string) string {
	return fmt.Sprintf("<FLDBAND>20</FLDBAND><FLDCALL>%s</FLDCALL><FLDDATESTR>2026/06/27</FLDDATESTR><FLDMODE>PH</FLDMODE><FLDPRIMARYKEY>%d</FLDPRIMARYKEY><FLDSTATION>Tent</FLDSTATION><FLDTIMEONSTR>18:42:07</FLDTIMEONSTR>%s", call, number, extra)
}

func tx(kind, data string) string {
	return "<BOR><NTWK><FROM>PC-1</FROM><TRANSACTION>" + kind + "</TRANSACTION><XMLDATA>" + data + "</XMLDATA><EOR>"
}

func keyOf(t *testing.T, call string) string {
	t.Helper()
	c, err := toContact(parseRecord(record2(call, 1, "")), "")
	if err != nil {
		t.Fatal(err)
	}
	return c.IdempotencyKey
}

// The server relays what a client sends to every PC, so this is the guarantee
// that the relay can't change the club's log, clock or anything else.
func TestTheSourceOnlyEverSendsOpenAndCheck(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, func(s *Source) { s.check = time.Millisecond })

	server.send(tx("ADD", record2("W1AW", 1, "")), tx("EDIT", record2("K1ABC", 1, "")), tx("DELETE", "1"),
		"<BOR><SCLK><YEAR>2026</YEAR><EOR>", "<BOR><MESG><TO></TO><FROM>A</FROM><MSGTXT>hi</MSGTXT><EOR>")
	eventually(t, "records and checks", func() bool {
		return len(sink.snapshot()) == 3 && server.count(messageCheck) >= 3
	})

	for _, message := range server.sent() {
		if message != messageOpen && message != messageCheck {
			t.Errorf("sent %q", message)
		}
	}
}

func TestAFullLogIsStoredAndNothingIsDeletedByAReload(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	source := runSource(t, server, sink, nil)

	server.send(tx("CLEAR", ""), tx("GET", record2("W1AW", 1, "")), tx("GET", record2("K1ABC", 2, "")), tx("LIST", ""))
	eventually(t, "two contacts", func() bool { return len(sink.snapshot()) == 2 && source.Status().RecordsKnown == 2 })

	// A reload that leaves a contact out, such as one cut short or from a PC on
	// another log, deletes nothing.
	server.send(tx("CLEAR", ""), tx("GET", record2("W1AW", 1, "")), tx("LIST", ""))
	eventually(t, "the reload to finish", func() bool { return source.Status().RecordsKnown == 1 })

	for _, c := range sink.snapshot() {
		if c.Deleted {
			t.Errorf("a reload deleted %+v", c)
		}
	}
	if !source.Status().Connected {
		t.Error("should be connected")
	}
}

func TestAddsAreNumberedByCounting(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("CLEAR", ""), tx("GET", record2("W1AW", 5, "")), tx("LIST", ""),
		tx("ADD", record2("K1ABC", 999, "")), tx("DELETE", "6"))

	eventually(t, "the delete", func() bool { return len(sink.snapshot()) == 3 })
	got := sink.snapshot()[2]
	if !got.Deleted || got.IdempotencyKey != keyOf(t, "K1ABC") || got.Callsign != "K1ABC" || got.Station != "Tent" {
		t.Errorf("delete = %+v", got)
	}
}

func TestAnEditThatChangesTheKeyNamesTheOldOne(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("ADD", record2("W1AW", 1, "")), tx("EDIT", record2("K1ABC", 1, "")))
	eventually(t, "the edit", func() bool { return len(sink.snapshot()) == 2 })

	got := sink.snapshot()[1]
	if got.Callsign != "K1ABC" || got.ReplacesKey != keyOf(t, "W1AW") || got.IdempotencyKey != keyOf(t, "K1ABC") {
		t.Errorf("edit = %+v", got)
	}
}

func TestAnEditThatKeepsTheKeyHasNoOldKey(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("ADD", record2("W1AW", 1, "")), tx("EDIT", record2("W1AW", 1, "<FLDSECTION>CT</FLDSECTION>")))
	eventually(t, "the edit", func() bool { return len(sink.snapshot()) == 2 })

	if got := sink.snapshot()[1]; got.ReplacesKey != "" || got.Section != "CT" {
		t.Errorf("edit = %+v", got)
	}
}

func TestDeleteOfAnUnknownRecordReloadsAndDeletesNothing(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("DELETE", "42"), tx("DELETE", "not a number"))
	eventually(t, "a reload", func() bool { return server.count(messageOpen) == 2 })

	if got := sink.snapshot(); len(got) != 0 {
		t.Errorf("contacts = %+v", got)
	}
}

func TestAnEditOfAnUnknownRecordIsKeptAndReloads(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("EDIT", record2("W1AW", 42, "")))
	eventually(t, "the contact and a reload", func() bool {
		return len(sink.snapshot()) == 1 && server.count(messageOpen) == 2
	})
	if got := sink.snapshot()[0]; got.ReplacesKey != "" {
		t.Errorf("contact = %+v", got)
	}
}

// Even while a reload has to wait, the next ADD must get the number the server
// gives it, or a later DELETE would hit the wrong contact.
func TestACheckWithADifferentLastNumberKeepsNumberingRight(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, func(s *Source) { s.reloadGap = time.Hour })

	// Deleting the highest record frees its number, so the next ADD takes it.
	server.send(tx("ADD", record2("W1AW", 1, "")), tx("ADD", record2("K1ABC", 2, "")), tx("DELETE", "2"),
		tx("CHECK", "<COUNT>1</COUNT><LAST>1</LAST>"), tx("ADD", record2("N1MM", 2, "")), tx("DELETE", "2"))
	eventually(t, "the second delete", func() bool { return len(sink.snapshot()) == 5 })

	got := sink.snapshot()[4]
	if !got.Deleted || got.Callsign != "N1MM" {
		t.Errorf("the delete went to %+v", got)
	}
	if count := server.count(messageOpen); count != 1 {
		t.Errorf("reloads = %d", count)
	}
}

func TestACheckWithADifferentLastNumberAsksForTheLog(t *testing.T) {
	server := newFakeServer(t)
	runSource(t, server, &fakeSink{}, nil)

	server.send(tx("CHECK", "<COUNT>0</COUNT><LAST>7</LAST>"))
	eventually(t, "a reload", func() bool { return server.count(messageOpen) == 2 })
}

func TestNumberingStartsOverWhenTheLogEmpties(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("ADD", record2("W1AW", 1, "")), tx("DELETE", "1"), tx("ADD", record2("K1ABC", 1, "")), tx("DELETE", "1"))
	eventually(t, "the second delete", func() bool { return len(sink.snapshot()) == 4 })

	if got := sink.snapshot()[3]; !got.Deleted || got.Callsign != "K1ABC" {
		t.Errorf("the delete went to %+v", got)
	}
	if got := server.count(messageOpen); got != 1 {
		t.Errorf("reloads = %d", got)
	}
}

// A server in file-share mode relays OPEN and CHECK to every PC, and each one
// beeps and shows a warning. One OPEN per connection is the most it should see.
func TestNothingRepeatsUntilTheServerAnswers(t *testing.T) {
	server := newFakeServer(t)
	source := runSource(t, server, &fakeSink{}, func(s *Source) { s.check = time.Millisecond })

	time.Sleep(50 * time.Millisecond)
	if got := server.count(messageCheck); got != 0 {
		t.Fatalf("sent %d checks to a server that never answered", got)
	}
	if source.Status().LogAnswered {
		t.Error("the server hasn't answered")
	}

	// Band status and chat also reach a file-share server's clients.
	server.send("<BOR><BAMS><STATION>Tent</STATION><EOR>", "<BOR><MESG><TO></TO><FROM>A</FROM><MSGTXT>hi</MSGTXT><EOR>")
	time.Sleep(50 * time.Millisecond)
	if got := server.count(messageCheck); got != 0 {
		t.Fatalf("band status counted as an answer: %d checks", got)
	}

	server.send(tx("CLEAR", ""), tx("LIST", ""))
	eventually(t, "checks to start", func() bool { return server.count(messageCheck) >= 2 })
	if !source.Status().LogAnswered {
		t.Error("the server has answered")
	}
}

func TestAMismatchedCheckAsksForTheLog(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("ADD", record2("W1AW", 1, "")), tx("CHECK", "<COUNT>1</COUNT><LAST>1</LAST>"))
	eventually(t, "the add", func() bool { return len(sink.snapshot()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := server.count(messageOpen); got != 1 {
		t.Fatalf("a matching check asked for the log: %d", got)
	}

	server.send(tx("CHECK", "<COUNT>2</COUNT><LAST>2</LAST>"))
	eventually(t, "a reload", func() bool { return server.count(messageOpen) == 2 })
}

func TestReloadsAreLimited(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, func(s *Source) { s.reloadGap = time.Hour })

	for range 5 {
		server.send(tx("CHECK", "<COUNT>9</COUNT><LAST>9</LAST>"))
	}
	server.send(tx("ADD", record2("W1AW", 1, "")))
	eventually(t, "the add", func() bool { return len(sink.snapshot()) == 1 })

	if got := server.count(messageOpen); got != 1 {
		t.Errorf("reloads = %d", got)
	}
}

func TestAnUnreadableRecordIsKeptAsInvalidAndStillCounts(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(tx("ADD", "<FLDCALL>BAD CALL</FLDCALL><FLDPRIMARYKEY>1</FLDPRIMARYKEY>"),
		tx("ADD", record2("W1AW", 2, "")), tx("CHECK", "<COUNT>2</COUNT><LAST>2</LAST>"), tx("DELETE", "1"), tx("DELETE", "2"))
	eventually(t, "the contacts", func() bool { return len(sink.snapshot()) == 2 })

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.invalid) != 1 {
		t.Errorf("invalid = %v", sink.invalid)
	}
	if !sink.contacts[1].Deleted || sink.contacts[1].Callsign != "W1AW" {
		t.Errorf("contacts = %+v", sink.contacts)
	}
	if got := server.count(messageOpen); got != 1 {
		t.Errorf("reloads = %d", got)
	}
}

func TestItReconnectsAndReadsTheLogAgain(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.dropConnection()
	eventually(t, "a second connection and request", func() bool {
		return server.accepts.Load() == 2 && server.count(messageOpen) == 2
	})
}

// A full disk mustn't lose the contact, so the source reconnects and reads the
// log again until the store works.
func TestAStoreFailureReconnectsAndRecovers(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	sink.setFail(errors.New("disk full"))
	runSource(t, server, sink, nil)

	server.send(tx("ADD", record2("W1AW", 1, "")))
	eventually(t, "a reconnect", func() bool { return server.accepts.Load() >= 2 })

	sink.setFail(nil)
	eventually(t, "another request for the log", func() bool { return server.count(messageOpen) >= 2 })
	server.send(tx("CLEAR", ""), tx("GET", record2("W1AW", 1, "")), tx("LIST", ""))
	eventually(t, "the contact", func() bool { return len(sink.snapshot()) == 1 })
}

func TestAnEndlessMessageDropsTheConnection(t *testing.T) {
	server := newFakeServer(t)
	runSource(t, server, &fakeSink{}, nil)

	server.mu.Lock()
	conn := server.conn
	server.mu.Unlock()
	go func() { _, _ = conn.Write(make([]byte, maxFrameBytes+readBufferSize)) }()

	eventually(t, "a reconnect", func() bool { return server.accepts.Load() >= 2 })
}

func TestNotifyRunsOnlyWhenAContactIsQueued(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	var notified atomic.Int32
	runSource(t, server, sink, func(s *Source) { s.notify = func() { notified.Add(1) } })

	server.send("<BOR><BAMS><STATION>Tent</STATION><EOR>", tx("ADD", record2("W1AW", 1, "")))
	eventually(t, "a notification", func() bool { return notified.Load() == 1 })
}

func TestEncodeUsesTheWireFormat(t *testing.T) {
	got := encode("A")
	if string(got) != strings.Join([]string{"A\x00", "\x03\x00", "\x04\x00", "\x07\x00"}, "") {
		t.Errorf("got %v", got)
	}
}
