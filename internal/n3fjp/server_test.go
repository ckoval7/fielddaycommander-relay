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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

type serverHarness struct {
	server *Server
	queue  *queue.Queue
	addr   string
	stop   func()
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func startServer(t *testing.T, path string) *serverHarness {
	t.Helper()
	q, err := queue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	addr := freeAddress(t)
	server := NewServer(addr, q, q, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Run(ctx)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
		_ = q.Close()
	}
	t.Cleanup(stop)
	eventually(t, "the port to open", func() bool { return server.Status().Listening })
	return &serverHarness{server, q, addr, stop}
}

// simulatedPC is an N3FJP client, with the traffic the app's wire tests model.
type simulatedPC struct {
	t    *testing.T
	conn net.Conn
	mu   sync.Mutex
	got  []string
}

func connectPC(t *testing.T, addr string) *simulatedPC {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	pc := &simulatedPC{t: t, conn: conn}
	go func() {
		var frames decoder
		buffer := make([]byte, 8192)
		for {
			count, err := conn.Read(buffer)
			if count > 0 {
				messages, _ := frames.feed(buffer[:count])
				pc.mu.Lock()
				pc.got = append(pc.got, messages...)
				pc.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return pc
}

func (p *simulatedPC) send(messages ...string) {
	p.t.Helper()
	for _, message := range messages {
		if _, err := p.conn.Write(encode(message)); err != nil {
			p.t.Fatal(err)
		}
	}
}

func (p *simulatedPC) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

func (p *simulatedPC) kinds() []string {
	var out []string
	for _, message := range p.seen() {
		if kind := transaction(message); kind != "" {
			out = append(out, kind)
		}
	}
	return out
}

func (p *simulatedPC) waitFor(what string, check func([]string) bool) {
	p.t.Helper()
	eventually(p.t, what, func() bool { return check(p.seen()) })
}

func (p *simulatedPC) waitCount(kind string, count int) {
	p.t.Helper()
	eventually(p.t, fmt.Sprintf("%d %s", count, kind), func() bool {
		total := 0
		for _, seen := range p.kinds() {
			if seen == kind {
				total++
			}
		}
		return total >= count
	})
}

func hello(station string) []string {
	return []string{"<BOR><BAMS><STATION>" + station + "</STATION><BAND>20</BAND><MODE>PH</MODE><EOR>", "<BOR><NTWK><OPEN>", "<BOR><WHO><EOR>"}
}

func serverPending(t *testing.T, q *queue.Queue) []queue.Item {
	t.Helper()
	items, err := q.Next(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestAPCConnectingToAnEmptyLogGetsClearThenList(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	pc := connectPC(t, h.addr)
	pc.send(hello("GOTA-PC")...)
	pc.waitCount("LIST", 1)
	if got := pc.kinds(); !slices.Equal(got, []string{"CLEAR", "LIST"}) {
		t.Errorf("got %v", got)
	}
}

func TestAContactIsQueuedAndEchoedToEveryPCWithItsNumber(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	gota, cw := connectPC(t, h.addr), connectPC(t, h.addr)
	eventually(t, "two clients", func() bool { return h.server.Status().Clients == 2 })

	gota.send(clientTx("ADD", clientRecord(fieldDayFields("W1AW", 99, map[string]string{"RSTS": "5NN"}))))
	cw.waitCount("ADD", 1)
	gota.waitCount("ADD", 1)

	fields := parseRecord(field(cw.seen()[0], "XMLDATA"))
	if fields["PRIMARYKEY"] != "1" || fields["CALL"] != "W1AW" || fields["RSTS"] != "5NN" {
		t.Errorf("echo = %v", fields)
	}
	items := serverPending(t, h.queue)
	if len(items) != 1 || items[0].Contact.Callsign != "W1AW" {
		t.Errorf("queue = %+v", items)
	}
}

func TestARefusedContactGetsAChatAndNoEcho(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	pc := connectPC(t, h.addr)
	pc.send(clientTx("ADD", clientRecord(fieldDayFields("NOT A CALL", 1, nil))))
	pc.waitFor("a chat", func(seen []string) bool { return len(seen) > 0 })

	if !strings.Contains(pc.seen()[0], "<MESG>") || !strings.Contains(pc.seen()[0], "was not saved") {
		t.Errorf("got %q", pc.seen()[0])
	}
	if got := pc.kinds(); len(got) != 0 {
		t.Errorf("echoed %v", got)
	}
	if got := h.server.Status().Records; got != 0 {
		t.Errorf("records = %d", got)
	}
}

// Clients count echoes to number records, so a re-entered contact has to echo too.
func TestAReEnteredContactEchoesBothAndStaysUntilBothAreGone(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	pc := connectPC(t, h.addr)
	same := clientTx("ADD", clientRecord(fieldDayFields("W1AW", 1, nil)))
	pc.send(same, same)
	pc.waitCount("ADD", 2)
	if got := serverPending(t, h.queue); len(got) != 1 {
		t.Fatalf("queue = %+v", got)
	}

	pc.send(clientTx("DELETE", "1"))
	pc.waitCount("DELETE", 1)
	for _, item := range serverPending(t, h.queue) {
		if item.Contact.Deleted {
			t.Fatal("deleted a contact another record still has")
		}
	}

	pc.send(clientTx("DELETE", "2"))
	pc.waitCount("DELETE", 2)
	eventually(t, "the contact's delete", func() bool {
		counts, _ := h.queue.Counts(context.Background())
		return counts.Pending == 2
	})
}

func TestAnEditThatChangesTheKeyNamesTheOldOneAndIsEchoed(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	pc := connectPC(t, h.addr)
	pc.send(clientTx("ADD", clientRecord(fieldDayFields("W1WA", 1, nil))))
	pc.waitCount("ADD", 1)

	pc.send(clientTx("EDIT", clientRecord(fieldDayFields("W1AW", 1, map[string]string{"QSLR": "N"}))))
	pc.waitCount("EDIT", 1)

	eventually(t, "the edit to queue", func() bool {
		counts, _ := h.queue.Counts(context.Background())
		return counts.Pending == 2
	})
	ctx := context.Background()
	first := serverPending(t, h.queue)[0]
	if err := h.queue.Apply(ctx, []queue.Outcome{{ID: first.ID, Status: queue.Accepted}}); err != nil {
		t.Fatal(err)
	}
	edit := serverPending(t, h.queue)[0].Contact
	if edit.Callsign != "W1AW" || edit.ReplacesKey != first.Contact.IdempotencyKey {
		t.Errorf("edit = %+v", edit)
	}
}

func TestEditsAndDeletesOfUnknownRecordsAnswerWithACheck(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	pc := connectPC(t, h.addr)
	pc.send(clientTx("EDIT", clientRecord(fieldDayFields("W1AW", 42, nil))), clientTx("DELETE", "42"))
	pc.waitCount("CHECK", 2)
	if got := serverPending(t, h.queue); len(got) != 0 {
		t.Errorf("queue = %+v", got)
	}
}

func TestCheckReportsCountAndLastLikeN3FJP(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	pc := connectPC(t, h.addr)
	pc.send(clientTx("ADD", clientRecord(fieldDayFields("W1AW", 1, nil))), clientTx("ADD", clientRecord(fieldDayFields("K1ABC", 2, nil))))
	pc.waitCount("ADD", 2)
	pc.send(clientTx("DELETE", "2"), "<BOR><NTWK><CHECK>")
	pc.waitCount("CHECK", 1)

	var check string
	for _, message := range pc.seen() {
		if transaction(message) == "CHECK" {
			check = message
		}
	}
	// A deleted number isn't reused, so clients that counted ADDs still agree.
	if field(check, "COUNT") != "1" || field(check, "LAST") != "2" {
		t.Errorf("check = %q", check)
	}

	pc.send(clientTx("DELETE", "1"), "<BOR><NTWK><CHECK>")
	pc.waitCount("CHECK", 2)
	for _, message := range pc.seen() {
		if transaction(message) == "CHECK" {
			check = message
		}
	}
	if field(check, "COUNT") != "0" || field(check, "LAST") != "0" {
		t.Errorf("an empty log starts numbering over: %q", check)
	}
}

func TestALargeLogLoadsWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	q, err := queue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const total = 3000
	for number := 1; number <= total; number++ {
		fields := fieldDayFields(fmt.Sprintf("K%dABC", number), number, nil)
		if err := q.SaveLoggerRecord(ctx, queue.LoggerRecord{Number: number, Key: fmt.Sprint(number), Fields: fields}, number); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	h := startServer(t, path)
	pc := connectPC(t, h.addr)
	pc.send("<BOR><NTWK><OPEN>")
	pc.waitCount("LIST", 1)

	gets := 0
	for _, kind := range pc.kinds() {
		if kind == "GET" {
			gets++
		}
	}
	if gets != total {
		t.Errorf("loaded %d of %d", gets, total)
	}
}

func TestAClientFarBehindIsDroppedInsteadOfGrowingForever(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	stuck := newServerClient(server)
	message := strings.Repeat("x", 4096)
	for range maxPendingBytes / len(encode(message)) {
		stuck.send(message)
	}
	select {
	case <-stuck.done:
		t.Fatal("dropped below the limit")
	default:
	}
	stuck.send(message)
	select {
	case <-stuck.done:
	default:
		t.Error("kept growing past the limit")
	}
}

func TestTheLogSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	first := startServer(t, path)
	pc := connectPC(t, first.addr)
	pc.send(clientTx("ADD", clientRecord(fieldDayFields("W1AW", 1, map[string]string{"RSTS": "5NN", "COUNTRYWORKED": "USA"}))),
		clientTx("ADD", clientRecord(fieldDayFields("K1ABC", 2, nil))))
	pc.waitCount("ADD", 2)
	pc.send(clientTx("DELETE", "2"))
	pc.waitCount("DELETE", 1)
	first.stop()

	second := startServer(t, path)
	later := connectPC(t, second.addr)
	later.send("<BOR><NTWK><OPEN>", "<BOR><NTWK><CHECK>")
	later.waitCount("CHECK", 1)

	var gets []string
	for _, message := range later.seen() {
		if transaction(message) == "GET" {
			gets = append(gets, message)
		}
	}
	if len(gets) != 1 {
		t.Fatalf("gets = %q", gets)
	}
	fields := parseRecord(field(gets[0], "XMLDATA"))
	if fields["CALL"] != "W1AW" || fields["RSTS"] != "5NN" || fields["COUNTRYWORKED"] != "USA" || fields["PRIMARYKEY"] != "1" {
		t.Errorf("fields = %v", fields)
	}

	later.send(clientTx("ADD", clientRecord(fieldDayFields("N1MM", 9, nil))))
	later.waitCount("ADD", 1)
	for _, message := range later.seen() {
		if transaction(message) == "ADD" {
			if got := parseRecord(field(message, "XMLDATA"))["PRIMARYKEY"]; got != "3" {
				t.Errorf("number after restart = %s, want 3", got)
			}
		}
	}
}

func TestServerChangesReachEveryPCWithoutGoingBackToTheServer(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	ctx := context.Background()
	pc := connectPC(t, h.addr)
	eventually(t, "the client", func() bool { return h.server.Status().Clients == 1 })

	web := serverContact(t, "W1WEB", nil)
	if err := h.server.Add(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err := h.server.Add(ctx, web); err != nil {
		t.Fatal(err)
	}
	pc.waitCount("ADD", 1)
	time.Sleep(20 * time.Millisecond)
	if got := pc.kinds(); !slices.Equal(got, []string{"ADD"}) {
		t.Errorf("a repeat was written: %v", got)
	}

	after := web
	after.Section = "NH"
	if err := h.server.Edit(ctx, web.IdempotencyKey, web, after); err != nil {
		t.Fatal(err)
	}
	pc.waitCount("EDIT", 1)
	if err := h.server.Delete(ctx, web.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	pc.waitCount("DELETE", 1)

	if got := serverPending(t, h.queue); len(got) != 0 {
		t.Errorf("server changes were queued to go back: %+v", got)
	}
	if h.queue == nil || !h.server.Ready() {
		t.Error("should be ready")
	}
}

func TestAnOperatorsEditOfAWebContactNamesIt(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	ctx := context.Background()
	web := serverContact(t, "W1WEB", nil)
	if err := h.server.Add(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err := h.queue.SaveMirror(ctx, queue.MirrorRow{UUID: "uuid-web", DerivedKey: web.IdempotencyKey, Baseline: &web}); err != nil {
		t.Fatal(err)
	}

	pc := connectPC(t, h.addr)
	pc.send(clientTx("EDIT", clientRecord(fieldDayFields("K2WEB", 1, nil))))
	pc.waitCount("EDIT", 1)
	items := serverPending(t, h.queue)
	if len(items) != 1 || items[0].Contact.UUID != "uuid-web" {
		t.Errorf("queue = %+v", items)
	}
}

func TestClockSyncsStopHereAndChatsAreMadeSafe(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	a, b := connectPC(t, h.addr), connectPC(t, h.addr)
	eventually(t, "two clients", func() bool { return h.server.Status().Clients == 2 })

	a.send("<BOR><SCLK><YEAR>2026</YEAR><EOR>", "<BOR><MESG><TO></TO><FROM>GOTA</FROM><MSGTXT>need <NTWK> help <EOR> now</MSGTXT><EOR>")
	b.waitFor("the chat", func(seen []string) bool { return len(seen) >= 1 })
	for _, message := range b.seen() {
		if strings.Contains(message, "<SCLK>") {
			t.Error("a clock sync was passed on")
		}
	}
	chat := b.seen()[0]
	if strings.Count(chat, "<EOR>") != 1 || strings.Contains(chat, "<NTWK>") || !strings.Contains(chat, "help") {
		t.Errorf("chat = %q", chat)
	}
}

func TestBandStatusIsRelayedAndAnsweredOnWho(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	a := connectPC(t, h.addr)
	a.send("<BOR><BAMS><STATION>GOTA</STATION><BAND>20</BAND><MODE>PH</MODE><EOR>")
	a.waitFor("the relay", func(seen []string) bool { return len(seen) == 1 })

	b := connectPC(t, h.addr)
	b.send("<BOR><WHO><EOR>")
	b.waitFor("the answer", func(seen []string) bool { return len(seen) == 1 })
	if !strings.Contains(b.seen()[0], "<STATION>GOTA</STATION>") {
		t.Errorf("got %q", b.seen()[0])
	}
}

func TestEveryMessageHasContent(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(encode("<BOR><NTWK><OPEN>")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	count, _ := conn.Read(buffer)
	// An N3FJP client hangs up on an empty message.
	if strings.HasPrefix(string(buffer[:count]), string(terminator)) || strings.Contains(string(buffer[:count]), string(terminator)+string(terminator)) {
		t.Errorf("sent an empty message: %v", buffer[:count])
	}
}

func TestAStrayConnectionDoesNotDisturbAPC(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	stray, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stray.Close()
	go func() { _, _ = stray.Write(make([]byte, maxFrameBytes+readBufferSize)) }()

	pc := connectPC(t, h.addr)
	pc.send(clientTx("ADD", clientRecord(fieldDayFields("W1AW", 1, nil))))
	pc.waitCount("ADD", 1)
	eventually(t, "the stray to be dropped", func() bool { return h.server.Status().Clients == 1 })
}

func TestAStuckPCDoesNotHoldUpTheOthers(t *testing.T) {
	h := startServer(t, filepath.Join(t.TempDir(), "relay.db"))
	stuck, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()
	eventually(t, "the stuck client", func() bool { return h.server.Status().Clients == 1 })

	pc := connectPC(t, h.addr)
	notes := strings.Repeat("x", 450)
	for number := 1; number <= 30; number++ {
		pc.send(clientTx("ADD", clientRecord(fieldDayFields(fmt.Sprintf("K%dABC", number), number, map[string]string{"COMMENTS": notes}))))
	}
	pc.waitCount("ADD", 30)
}

type failingSink struct{ fakeSink }

func (f *failingSink) Put(context.Context, contact.Contact) (bool, error) {
	return false, errors.New("disk full")
}

// The contact stays in the operator's entry boxes, because there's no echo.
func TestADiskFailureNeverEchoesAContact(t *testing.T) {
	q, err := queue.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	addr := freeAddress(t)
	server := NewServer(addr, &failingSink{}, q, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	eventually(t, "the port", func() bool { return server.Status().Listening })

	pc := connectPC(t, addr)
	pc.send(clientTx("ADD", clientRecord(fieldDayFields("W1AW", 1, nil))))
	pc.waitFor("the chat", func(seen []string) bool { return len(seen) == 1 })
	if !strings.Contains(pc.seen()[0], "was not saved") || len(pc.kinds()) != 0 {
		t.Errorf("got %q", pc.seen())
	}
}
