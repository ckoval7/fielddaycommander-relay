// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package udplog

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

const record = "<CALL:5>K1ABC <QSO_DATE:8>20260627 <TIME_ON:6>184207 <BAND:3>20m <MODE:2>CW " +
	"<FREQ:8>14.02530 <STATION_CALLSIGN:5>W2XYZ <SRX_STRING:6>2A EMA <EOR>"

var (
	local  = netip.MustParseAddr("127.0.0.1")
	remote = netip.MustParseAddr("192.168.1.23")
)

func newQueue(t *testing.T) *queue.Queue {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func newListener(kind Kind, q *queue.Queue) *Listener {
	return New(kind, "127.0.0.1:0", q, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func queued(t *testing.T, q *queue.Queue) []queue.Item {
	t.Helper()
	items, err := q.Next(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// qtString writes a string the way Qt does: its length, then its bytes.
func qtString(value string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(value))), value...)
}

func wsjtxMessage(kind uint32, id string, body ...[]byte) []byte {
	data := binary.BigEndian.AppendUint32(nil, wsjtxMagic)
	data = binary.BigEndian.AppendUint32(data, 3)
	data = binary.BigEndian.AppendUint32(data, kind)
	data = append(data, qtString(id)...)
	for _, part := range body {
		data = append(data, part...)
	}
	return data
}

func loggedADIF(id, text string) []byte {
	return wsjtxMessage(wsjtxLoggedADIF, id, qtString(text))
}

func TestPlainADIFFromThisComputerUsesThePickedStation(t *testing.T) {
	q := newQueue(t)
	l := newListener(ADIF, q)
	if err := l.handle(context.Background(), datagram{data: []byte(record), from: local}); err != nil {
		t.Fatal(err)
	}
	items := queued(t, q)
	if len(items) != 1 {
		t.Fatalf("queued %d contacts, want 1", len(items))
	}
	got := items[0].Contact
	if got.Callsign != "K1ABC" || got.Band != "20m" || got.Mode != "CW" || got.ExchangeClass != "2A" || got.Section != "EMA" {
		t.Errorf("contact = %+v", got)
	}
	if got.Station != "" {
		t.Errorf("station = %q, want none so the picked one applies", got.Station)
	}
	if status := l.Status(); status.Contacts != 1 || status.LastContact.IsZero() {
		t.Errorf("status = %+v", status)
	}
}

func TestPlainADIFFromAnotherPCNamesThatPC(t *testing.T) {
	q := newQueue(t)
	l := newListener(ADIF, q)
	if err := l.handle(context.Background(), datagram{data: []byte(record), from: remote}); err != nil {
		t.Fatal(err)
	}
	if items := queued(t, q); len(items) != 1 || items[0].Contact.Station != "W2XYZ @ 192.168.1.23" {
		t.Fatalf("queued %+v", items)
	}
}

func TestSeveralRecordsInOneDatagramAreAllKept(t *testing.T) {
	q := newQueue(t)
	l := newListener(ADIF, q)
	second := "<CALL:4>N0XX <QSO_DATE:8>20260627 <TIME_ON:4>1900 <BAND:3>40m <MODE:3>SSB <EOR>"
	text := "<ADIF_VER:5>3.1.4 <EOH>\n" + record + "\n" + second
	if err := l.handle(context.Background(), datagram{data: []byte(text), from: local}); err != nil {
		t.Fatal(err)
	}
	if items := queued(t, q); len(items) != 2 {
		t.Fatalf("queued %d contacts, want 2", len(items))
	}
}

func TestWSJTXLoggedADIFIsQueued(t *testing.T) {
	q := newQueue(t)
	l := newListener(WSJTX, q)
	text := "<adif_ver:5>3.1.0 <programid:6>WSJT-X <EOH> " + record
	if err := l.handle(context.Background(), datagram{data: loggedADIF("WSJT-X", text), from: local}); err != nil {
		t.Fatal(err)
	}
	if items := queued(t, q); len(items) != 1 || items[0].Contact.Callsign != "K1ABC" || items[0].Contact.Station != "" {
		t.Fatalf("queued %+v", items)
	}
}

func TestWSJTXStations(t *testing.T) {
	cases := []struct {
		id   string
		from netip.Addr
		want string
	}{
		{"WSJT-X", local, ""},
		{"JTDX", netip.Addr{}, ""},
		{"WSJT-X", remote, "WSJT-X @ 192.168.1.23"},
		{"jtdx", remote, "jtdx @ 192.168.1.23"},
		{"WSJT-X - IC7300", local, "WSJT-X - IC7300"},
		{"WSJT-X - IC7300", remote, "WSJT-X - IC7300"},
		{"WSJT-X - \x00Rig\n", remote, "WSJT-X - Rig"},
		{"", remote, ""},
	}
	for _, c := range cases {
		if got := wsjtxStation(c.id, c.from); got != c.want {
			t.Errorf("wsjtxStation(%q, %v) = %q, want %q", c.id, c.from, got, c.want)
		}
	}
}

func TestADIFStationsFitTheServer(t *testing.T) {
	long := "W2XYZ/ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	got := adifStation(map[string]string{"STATION_CALLSIGN": long}, netip.MustParseAddr("192.168.100.200"))
	if want := long[:maxLabel] + " @ 192.168.100.200"; got != want || len([]rune(got)) > maxStation {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := adifStation(map[string]string{}, remote); got != "ADIF @ 192.168.1.23" {
		t.Errorf("without a station call: %q", got)
	}
	if got := adifStation(map[string]string{"STATION_CALLSIGN": "W2XYZ"}, netip.MustParseAddr("::ffff:192.168.1.23")); got != "W2XYZ @ 192.168.1.23" {
		t.Errorf("mapped address: %q", got)
	}
}

func TestOnlyCompleteLoggedADIFMessagesCount(t *testing.T) {
	whole := loggedADIF("WSJT-X", record)
	cases := map[string][]byte{
		"heartbeat":   wsjtxMessage(0, "WSJT-X", binary.BigEndian.AppendUint32(nil, 3), qtString("2.7.0"), qtString("")),
		"qso logged":  wsjtxMessage(5, "WSJT-X", qtString("K1ABC")),
		"cut short":   whole[:len(whole)-10],
		"no header":   whole[:8],
		"null adif":   wsjtxMessage(wsjtxLoggedADIF, "WSJT-X", binary.BigEndian.AppendUint32(nil, nullLength)),
		"wrong magic": append([]byte{0, 0, 0, 0}, whole[4:]...),
		"huge length": wsjtxMessage(wsjtxLoggedADIF, "WSJT-X", binary.BigEndian.AppendUint32(nil, 0xFFFFFFF0)),
	}
	for name, data := range cases {
		if _, _, ok := wsjtxLogged(data); ok {
			t.Errorf("%s was read as a logged contact", name)
		}
	}
	if id, text, ok := wsjtxLogged(whole); !ok || id != "WSJT-X" || string(text) != record {
		t.Errorf("whole message: %q %q %v", id, text, ok)
	}
}

func TestThePlainADIFListenerSkipsWSJTXMessages(t *testing.T) {
	if looksLikeADIF(loggedADIF("WSJT-X", record)) {
		t.Error("a WSJT-X message was taken for plain ADIF")
	}
	if looksLikeADIF([]byte("no tags here")) {
		t.Error("text with no tags was taken for ADIF")
	}
}

func TestContactsOutsideTheEventAreSkippedOnceItsDatesAreKnown(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	l := newListener(ADIF, q)
	old := "<CALL:4>N0XX <QSO_DATE:8>20240101 <TIME_ON:4>1200 <BAND:3>40m <MODE:3>SSB <EOR>"

	// Before the dates are known, the server decides.
	if err := l.handle(ctx, datagram{data: []byte(old), from: local}); err != nil {
		t.Fatal(err)
	}
	if items := queued(t, q); len(items) != 1 {
		t.Fatalf("queued %d contacts before the dates were known, want 1", len(items))
	}

	start := time.Date(2026, 6, 27, 18, 0, 0, 0, time.UTC)
	if err := q.SetEventPeriod(ctx, start, start.Add(27*time.Hour)); err != nil {
		t.Fatal(err)
	}
	older := "<CALL:4>N1XX <QSO_DATE:8>20240101 <TIME_ON:4>1200 <BAND:3>40m <MODE:3>SSB <EOR>"
	if err := l.handle(ctx, datagram{data: []byte(older + record), from: local}); err != nil {
		t.Fatal(err)
	}
	if items := queued(t, q); len(items) != 2 {
		t.Fatalf("queued %d contacts, want the first and the one in the event", len(items))
	}
	if status := l.Status(); status.Outside != 1 || status.Contacts != 2 {
		t.Errorf("status = %+v", status)
	}
}

func TestAnUnusableRecordIsKeptForSomeoneToCheck(t *testing.T) {
	q := newQueue(t)
	l := newListener(ADIF, q)
	bad := "<CALL:5>K1ABC <QSO_DATE:8>2026062X <TIME_ON:4>1200 <EOR>"
	if err := l.handle(context.Background(), datagram{data: []byte(bad), from: local}); err != nil {
		t.Fatal(err)
	}
	counts, err := q.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Invalid != 1 || counts.Pending != 0 {
		t.Errorf("counts = %+v", counts)
	}
}

func TestTheSameContactFromBothListenersIsOneContact(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	if err := newListener(ADIF, q).handle(ctx, datagram{data: []byte(record), from: local}); err != nil {
		t.Fatal(err)
	}
	if err := newListener(WSJTX, q).handle(ctx, datagram{data: loggedADIF("WSJT-X", record), from: local}); err != nil {
		t.Fatal(err)
	}
	if items := queued(t, q); len(items) != 1 {
		t.Fatalf("queued %d contacts, want 1", len(items))
	}
}

func TestAListenerReceivesOverTheNetworkAndSavesWhatsWaiting(t *testing.T) {
	q := newQueue(t)
	woken := make(chan struct{}, 10)
	l := New(WSJTX, "127.0.0.1:0", q, func() { woken <- struct{}{} }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !l.Status().Listening {
		if time.Now().After(deadline) {
			t.Fatal("the listener didn't start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := net.Dial("udp4", l.Status().Address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, message := range [][]byte{wsjtxMessage(0, "WSJT-X"), loggedADIF("WSJT-X", record)} {
		if _, err := conn.Write(message); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("the contact never arrived")
	}
	cancel()
	<-done
	if items := queued(t, q); len(items) != 1 {
		t.Fatalf("queued %d contacts, want 1", len(items))
	}
	if l.Status().Listening {
		t.Error("still listening after stopping")
	}
}

func TestAnOversizedDatagramIsCountedAsDropped(t *testing.T) {
	l := newListener(ADIF, newQueue(t))
	waiting := make(chan datagram, 1)
	huge := []byte("<CALL:5>K1ABC " + strings.Repeat(" ", maxPacketBytes))
	l.receive(huge, local, waiting)
	if len(waiting) != 0 || l.Status().Dropped != 1 {
		t.Errorf("waiting %d, status %+v", len(waiting), l.Status())
	}
	l.receive([]byte(record), local, waiting)
	if len(waiting) != 1 {
		t.Error("a normal datagram wasn't kept")
	}
}

func TestTheAddressesAreTheServersDefaults(t *testing.T) {
	cases := []struct {
		kind      Kind
		onNetwork bool
		port      int
		want      string
	}{
		{ADIF, false, 0, "127.0.0.1:2238"},
		{ADIF, true, 0, "0.0.0.0:2238"},
		{WSJTX, false, 0, "127.0.0.1:2237"},
		{WSJTX, true, 0, "0.0.0.0:2237"},
		{WSJTX, false, 2333, "127.0.0.1:2333"},
	}
	for _, c := range cases {
		if got := c.kind.Address(c.onNetwork, c.port); got != c.want {
			t.Errorf("%s on network %v: got %s, want %s", c.kind.Source(), c.onNetwork, got, c.want)
		}
	}
}
