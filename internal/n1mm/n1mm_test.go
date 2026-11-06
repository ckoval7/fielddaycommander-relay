// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n1mm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

const (
	testID   = "0123456789abcdef0123456789abcdef"
	testTime = "2026-06-27 18:42:07"
)

// wire builds a datagram the way N1MM does: .NET XmlWriter layout, and only as many
// of the UTF-8 bytes as the text has characters.
func wire(root string, fields ...[2]string) []byte {
	var text strings.Builder
	text.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\r\n<" + root + ">")
	for _, field := range fields {
		value := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(field[1])
		text.WriteString("\r\n\t<" + field[0] + ">" + value + "</" + field[0] + ">")
	}
	text.WriteString("\r\n</" + root + ">")
	data := []byte(text.String())
	return data[:utf8.RuneCount(data)]
}

// contactFields are the fields of a Field Day contact, with changes laid over them.
func contactFields(changes map[string]string) [][2]string {
	values := map[string]string{
		"app": "N1MM", "contestname": "FD", "contestnr": "12", "timestamp": testTime, "mycall": "W2XYZ", "band": "14",
		"rxfreq": "1402530", "txfreq": "1402530", "operator": "K3CPK", "mode": "CW", "call": "K1ABC", "snt": "599", "rcv": "599",
		"gridsquare": "", "exchange1": "2A", "section": "CT", "comment": "", "name": "", "power": "", "IsOriginal": "True",
		"NetBiosName": "SHACK-PC", "StationName": "SHACK-PC", "ID": testID, "oldtimestamp": testTime, "oldcall": "K1ABC",
	}
	for name, value := range changes {
		values[name] = value
	}
	order := []string{"app", "contestname", "contestnr", "timestamp", "mycall", "band", "rxfreq", "txfreq", "operator", "mode", "call", "snt", "rcv", "gridsquare", "exchange1", "section", "comment", "name", "power", "IsOriginal", "NetBiosName", "StationName", "ID", "oldtimestamp", "oldcall"}
	fields := make([][2]string, 0, len(order))
	for _, name := range order {
		fields = append(fields, [2]string{name, values[name]})
	}
	return fields
}

func contactPacket(root string, changes map[string]string) []byte {
	return wire(root, contactFields(changes)...)
}

func deletePacket(id string) []byte {
	return wire(rootDelete, [2]string{"app", "N1MM"}, [2]string{"timestamp", testTime}, [2]string{"call", "K1ABC"},
		[2]string{"band", "14"}, [2]string{"StationName", "SHACK-PC"}, [2]string{"ID", id})
}

func newQueue(t *testing.T) *queue.Queue {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func handle(t *testing.T, l *Listener, data []byte) {
	t.Helper()
	if err := l.handle(context.Background(), parsePacket(data), string(data)); err != nil {
		t.Fatal(err)
	}
}

// answerAll marks everything sendable as accepted, so the next change to a contact
// can come out of the queue, and returns what went out.
func answerAll(t *testing.T, q *queue.Queue) []contact.Contact {
	t.Helper()
	ctx := context.Background()
	items, err := q.Next(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var sent []contact.Contact
	var outcomes []queue.Outcome
	for _, item := range items {
		sent = append(sent, item.Contact)
		outcomes = append(outcomes, queue.Outcome{ID: item.ID, Status: queue.Accepted})
	}
	if err := q.Apply(ctx, outcomes); err != nil {
		t.Fatal(err)
	}
	return sent
}

func TestAContactIsReadAsTheServerNeedsIt(t *testing.T) {
	q := newQueue(t)
	l := New(LocalAddress, q, nil, quiet())
	handle(t, l, contactPacket(rootContact, map[string]string{"gridsquare": "FN31", "name": "Ann", "comment": "R&D <net>"}))

	sent := answerAll(t, q)
	if len(sent) != 1 {
		t.Fatalf("sent %+v", sent)
	}
	want := contact.Contact{
		IdempotencyKey: sent[0].IdempotencyKey, Callsign: "K1ABC", QSOTime: "2026-06-27T18:42:07Z", Band: "20m", Mode: "CW",
		FrequencyHz: 14_025_300, Operator: "K3CPK", Station: "SHACK-PC", ExchangeClass: "2A", Section: "CT",
		RSTSent: "599", RSTReceived: "599", GridSquare: "FN31", ContactName: "Ann", Notes: "R&D <net>",
	}
	if sent[0] != want {
		t.Errorf("got  %+v\nwant %+v", sent[0], want)
	}
}

func TestBands(t *testing.T) {
	for value, want := range map[string]string{
		"1.8": "160m", "3.5": "80m", "3,5": "80m", "7": "40m", "14": "20m", "50": "6m", "144": "2m", "222": "1.25m", "420": "70cm",
		"1296": "23cm", "10000": "3cm", "20m": "20m", "70CM": "70cm", "": "", "nonsense": "", "0": "", "99": "",
	} {
		if got := bandName(value); got != want {
			t.Errorf("%q: got %q, want %q", value, got, want)
		}
	}
}

func TestAFrequencyOutsideTheBandIsLeftOut(t *testing.T) {
	for value, want := range map[string]int64{"1402530": 14_025_300, "0": 0, "": 0, "703000": 0, "-5": 0, "nope": 0} {
		if got := frequencyHz(value, "20m"); got != want {
			t.Errorf("%q: got %d, want %d", value, got, want)
		}
	}
}

func TestAPassedOnContactCountsForThePCThatLoggedIt(t *testing.T) {
	relayed := parsePacket(contactPacket(rootContact, map[string]string{"IsOriginal": "False", "NetBiosName": "CW-TENT", "StationName": "OTHER-PC"}))
	if got := relayed.station(); got != "CW-TENT" {
		t.Errorf("got %q", got)
	}
	encoded := parsePacket(contactPacket(rootContact, map[string]string{"StationName": "Bob&#39;s &amp; Co"}))
	if got := encoded.station(); got != "Bob's & Co" {
		t.Errorf("got %q", got)
	}
}

func TestAContactCutShortKeepsWhatArrived(t *testing.T) {
	// Accented letters cost the end of the packet, which here reaches into the ID.
	q := newQueue(t)
	l := New(LocalAddress, q, nil, quiet())
	data := contactPacket(rootContact, map[string]string{"comment": strings.Repeat("é", 200)})
	parsed := parsePacket(data)
	if parsed.id() != "" {
		t.Errorf("read an id from a damaged packet: %q", parsed.id())
	}
	handle(t, l, data)
	if sent := answerAll(t, q); len(sent) != 1 || sent[0].Callsign != "K1ABC" {
		t.Errorf("sent %+v", sent)
	}
}

func TestEveryCutOfAPacketIsSafe(t *testing.T) {
	data := contactPacket(rootContact, nil)
	for end := range data {
		parsed := parsePacket(data[:end])
		// A value is either whole or absent, never cut.
		if call, ok := parsed.fields["call"]; ok && call != "K1ABC" {
			t.Fatalf("cut at %d read call %q", end, call)
		}
		if id := parsed.id(); id != "" && id != testID {
			t.Fatalf("cut at %d read id %q", end, id)
		}
	}
}

func TestOtherPacketsAreNotContacts(t *testing.T) {
	for _, tt := range []struct{ data, root string }{
		{`<?xml version="1.0"?><dynamicresults><club>Hamden & District</club></dynamicresults>`, "dynamicresults"},
		{"<?xml version=\"1.0\" encoding=\"utf-8\"?>\r\n<RadioInfo>\r\n\t<Freq>1402530</Freq>\r\n</RadioInfo>", "radioinfo"},
		{`<ContactInfo><call>K1ABC</call></ContactInfo>`, rootContact},
		{"\xEF\xBB\xBF<contactdelete/>", rootDelete},
		{"", ""},
		{"no xml here", ""},
		{"\x00\x01\x02", ""},
	} {
		if got := rootOf([]byte(tt.data)); got != tt.root {
			t.Errorf("%q: got %q, want %q", tt.data, got, tt.root)
		}
	}
}

func TestAnUnusableContactIsKeptForAPersonOnce(t *testing.T) {
	q := newQueue(t)
	l := New(LocalAddress, q, nil, quiet())
	bad := contactPacket(rootContact, map[string]string{"timestamp": "1900-01-01 00:00:00"})
	handle(t, l, bad)
	handle(t, l, bad)
	handle(t, l, contactPacket(rootContact, map[string]string{"call": "NOT A CALL"}))
	counts, err := q.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Invalid != 2 || counts.Pending != 0 {
		t.Errorf("counts = %+v", counts)
	}
}

func TestARepeatChangesNothing(t *testing.T) {
	q := newQueue(t)
	wakes := 0
	l := New(LocalAddress, q, func() { wakes++ }, quiet())
	// Rebroadcast all Contest QSO's to other Apps sends contactreplace with the contact's own call and time.
	handle(t, l, contactPacket(rootContact, nil))
	handle(t, l, contactPacket(rootReplace, nil))
	if sent := answerAll(t, q); len(sent) != 1 {
		t.Errorf("sent %+v", sent)
	}
	if wakes != 1 || l.Status().Contacts != 1 {
		t.Errorf("woke %d times, status %+v", wakes, l.Status())
	}
}

func TestAnEditThatKeepsTheKeyIsACorrection(t *testing.T) {
	q := newQueue(t)
	l := New(LocalAddress, q, nil, quiet())
	handle(t, l, contactPacket(rootContact, nil))
	first := answerAll(t, q)[0]
	handle(t, l, contactPacket(rootReplace, map[string]string{"section": "EMA"}))
	sent := answerAll(t, q)
	if len(sent) != 1 || sent[0].IdempotencyKey != first.IdempotencyKey || sent[0].ReplacesKey != "" || sent[0].Section != "EMA" {
		t.Errorf("sent %+v", sent)
	}
}

func TestEditsToWhatMakesTheKeyNameTheOldKey(t *testing.T) {
	for name, changes := range map[string]map[string]string{
		"call": {"call": "K1XYZ", "oldcall": "K1ABC"},
		"time": {"timestamp": "2026-06-27 18:43:00"},
		// The packet doesn't say what the band and mode were, so only the saved key can tell.
		"band": {"band": "7", "rxfreq": "703000", "txfreq": "703000"},
		"mode": {"mode": "USB"},
	} {
		t.Run(name, func(t *testing.T) {
			q := newQueue(t)
			l := New(LocalAddress, q, nil, quiet())
			handle(t, l, contactPacket(rootContact, nil))
			first := answerAll(t, q)[0]
			handle(t, l, contactPacket(rootReplace, changes))
			sent := answerAll(t, q)
			if len(sent) != 1 || sent[0].ReplacesKey != first.IdempotencyKey || sent[0].IdempotencyKey == first.IdempotencyKey {
				t.Errorf("sent %+v", sent)
			}
		})
	}
}

func TestAnEditAfterARestartStillNamesTheOldKey(t *testing.T) {
	q := newQueue(t)
	handle(t, New(LocalAddress, q, nil, quiet()), contactPacket(rootContact, nil))
	first := answerAll(t, q)[0]
	handle(t, New(LocalAddress, q, nil, quiet()), contactPacket(rootReplace, map[string]string{"mode": "USB"}))
	if sent := answerAll(t, q); len(sent) != 1 || sent[0].ReplacesKey != first.IdempotencyKey {
		t.Errorf("sent %+v", sent)
	}
}

func TestADeleteFollowsEdits(t *testing.T) {
	q := newQueue(t)
	l := New(LocalAddress, q, nil, quiet())
	handle(t, l, contactPacket(rootContact, nil))
	answerAll(t, q)
	handle(t, l, contactPacket(rootReplace, map[string]string{"mode": "USB"}))
	edited := answerAll(t, q)[0]

	handle(t, l, deletePacket(testID))
	sent := answerAll(t, q)
	if len(sent) != 1 || !sent[0].Deleted || sent[0].IdempotencyKey != edited.IdempotencyKey || sent[0].Station != "SHACK-PC" {
		t.Errorf("sent %+v", sent)
	}

	// N1MM has forgotten it, and a repeat of the delete is harmless.
	handle(t, l, deletePacket(testID))
	if sent := answerAll(t, q); len(sent) != 0 {
		t.Errorf("sent %+v", sent)
	}
}

func TestADeleteOfAnUnknownContactDoesNothing(t *testing.T) {
	q := newQueue(t)
	l := New(LocalAddress, q, nil, quiet())
	for _, id := range []string{testID, "", "not an id"} {
		handle(t, l, deletePacket(id))
	}
	if sent := answerAll(t, q); len(sent) != 0 {
		t.Errorf("sent %+v", sent)
	}
}

// flakyStore fails a set number of times, as a full disk would.
type flakyStore struct {
	*queue.Queue
	failures atomic.Int32
}

func (s *flakyStore) Put(ctx context.Context, c contact.Contact) (bool, error) {
	if s.failures.Add(-1) >= 0 {
		return false, errors.New("disk full")
	}
	return s.Queue.Put(ctx, c)
}

// running starts a listener on a free port and returns a way to send to it.
func running(t *testing.T, store Store, notify func()) (*Listener, func([]byte), context.CancelFunc, <-chan struct{}) {
	t.Helper()
	l := New("127.0.0.1:0", store, notify, quiet())
	l.storeWait = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	eventually(t, "listening", func() bool { return l.Status().Listening })
	conn, err := net.Dial("udp4", l.Status().Address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return l, func(data []byte) {
		t.Helper()
		if _, err := conn.Write(data); err != nil {
			t.Fatal(err)
		}
	}, cancel, done
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func TestContactsArriveOverUDP(t *testing.T) {
	q := newQueue(t)
	var wakes atomic.Int32
	l, send, _, _ := running(t, q, func() { wakes.Add(1) })

	send(wire("RadioInfo", [2]string{"Freq", "1402530"}))
	send([]byte("garbage"))
	send(make([]byte, maxPacketBytes+1))
	send(contactPacket(rootContact, nil))
	eventually(t, "the contact", func() bool { return l.Status().Contacts == 1 })
	if wakes.Load() != 1 || l.Status().LastContact.IsZero() {
		t.Errorf("woke %d, status %+v", wakes.Load(), l.Status())
	}
	if sent := answerAll(t, q); len(sent) != 1 || sent[0].Callsign != "K1ABC" {
		t.Errorf("sent %+v", sent)
	}
}

func TestAWholeLogRebroadcastArrivesIntact(t *testing.T) {
	q := newQueue(t)
	l, send, _, _ := running(t, q, nil)
	const total = 300
	for i := range total {
		send(contactPacket(rootReplace, map[string]string{"call": fmt.Sprintf("K%dABC", i), "ID": fmt.Sprintf("%032x", i)}))
	}
	eventually(t, "every contact", func() bool { return l.Status().Contacts == total })
	if l.Status().Dropped != 0 {
		t.Errorf("status %+v", l.Status())
	}
}

func TestAFailedSaveKeepsTrying(t *testing.T) {
	store := &flakyStore{Queue: newQueue(t)}
	store.failures.Store(3)
	l, send, _, _ := running(t, store, nil)
	send(contactPacket(rootContact, nil))
	eventually(t, "the contact", func() bool { return l.Status().Contacts == 1 })
}

func TestShuttingDownSavesWhatIsWaiting(t *testing.T) {
	q := newQueue(t)
	l := New(LocalAddress, q, nil, quiet())
	packets := make(chan []byte, 2)
	l.receive(contactPacket(rootContact, map[string]string{"call": "K1ABC", "ID": fmt.Sprintf("%032x", 1)}), packets)
	l.receive(contactPacket(rootContact, map[string]string{"call": "W2XYZ", "ID": fmt.Sprintf("%032x", 2)}), packets)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	close(packets)
	for data := range packets {
		l.save(canceled, data)
	}
	if sent := answerAll(t, q); len(sent) != 2 {
		t.Errorf("sent %+v", sent)
	}
}

func TestCancelingStopsItAndFreesThePort(t *testing.T) {
	_, _, cancel, done := running(t, newQueue(t), nil)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("didn't stop")
	}
}

func TestABusyPortIsReportedAndRetried(t *testing.T) {
	blocker, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := blocker.LocalAddr().String()

	l := New(address, newQueue(t), nil, quiet())
	l.bindWait = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(20 * time.Millisecond)
	if l.Status().Listening {
		t.Error("claims to be listening on a busy port")
	}
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the port to free up", func() bool { return l.Status().Listening })
}

func TestTooManyPacketsAreCounted(t *testing.T) {
	l := New(LocalAddress, newQueue(t), nil, quiet())
	packets := make(chan []byte, 1)
	l.receive(contactPacket(rootContact, nil), packets)
	l.receive(contactPacket(rootContact, nil), packets)
	if l.Status().Dropped != 1 {
		t.Errorf("status %+v", l.Status())
	}
}
