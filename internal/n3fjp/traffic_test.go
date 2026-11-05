// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// These tests replay the traffic of real N3FJP client PCs, as the app's wire
// tests model it (tests/Support/ExternalLoggers/N3fjp.php in the app repo):
// fields in alphabetical order, raw values with no escaping, empty fields left
// out, and an EDIT that adds QSL fields and trims every value.

func fieldDayFields(call string, number int, overrides map[string]string) map[string]string {
	fields := map[string]string{
		"BAND": "20", "CALL": call, "CLASS": "2A", "COMPUTERNAME": "GOTA-PC", "CONTESTID": "ARRL-FD",
		"CONTINENT": "NA", "COUNTRYDXCC": "291", "COUNTRYWORKED": "USA", "CQZONE": "5",
		"DATESTR": "2026/06/27", "ITUZONE": "8", "MODE": "PH", "MODECONTEST": "PH", "OPERATOR": "K3CPK",
		"POINTS": "0", "PREFIX": "W1", "PRIMARYKEY": fmt.Sprint(number), "SECTION": "CT", "SERIALNOS": "001",
		"STATE": "CT", "STATION": "GOTA-PC", "TIMEONSTR": "20:42:07", "TRANSMITTERID": "0",
	}
	maps.Copy(fields, overrides)
	return fields
}

func clientRecord(fields map[string]string) string {
	var out strings.Builder
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if fields[name] != "" {
			fmt.Fprintf(&out, "<FLD%s>%s</FLD%s>", name, fields[name], name)
		}
	}
	return out.String()
}

func clientTx(kind, data string) string {
	return "<BOR><NTWK><FROM>GOTA-PC</FROM><TRANSACTION>" + kind + "</TRANSACTION><XMLDATA>" + data + "</XMLDATA><EOR>"
}

// N3FJP's server relays every message to every client, so the relay sees what
// the other PCs send. None of it is a contact, and none of it needs an answer.
func TestOtherPCsTrafficChangesNothing(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(
		"<BOR><BAMS><STATION>GOTA-PC</STATION><BAND>20</BAND><MODE>PH</MODE><EOR>",
		"<BOR><NTWK><OPEN>",
		"<BOR><WHO><EOR>",
		"<BOR><NTWK><CHECK>",
		"<BOR><SCLK><YEAR>2026</YEAR><MONTH>6</MONTH><DAY>27</DAY><HOUR>20</HOUR><MINUTE>42</MINUTE><SECOND>7</SECOND><MILLISECOND>412</MILLISECOND><EOR>",
		"<BOR><MESG><TO></TO><FROM>CW-PC</FROM><MSGTXT>need <NTWK> help <TRANSACTION>DELETE</TRANSACTION> on 40</MSGTXT><EOR>",
		"<BOR><DXSP>W1AW 14.250</DXSP><EOR>",
		clientTx("ADD", clientRecord(fieldDayFields("W1AW", 1, nil))),
	)
	eventually(t, "the contact", func() bool { return len(sink.snapshot()) == 1 })

	if got := server.count(messageOpen); got != 1 {
		t.Errorf("reloads = %d", got)
	}
	for _, sent := range server.sent() {
		if sent != messageOpen && sent != messageCheck {
			t.Errorf("sent %q", sent)
		}
	}
}

func TestARealContactMapsCorrectly(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	server.send(clientTx("ADD", clientRecord(fieldDayFields("W1AW/3", 1, map[string]string{
		"COMMENTS": "QRP & <5W> tnx 😀", "FREQUENCY": "14,25000", "MODE": "USB", "RSTS": "5NN", "RSTR": "59",
	}))))
	eventually(t, "the contact", func() bool { return len(sink.snapshot()) == 1 })

	got := sink.snapshot()[0]
	if got.Callsign != "W1AW/3" || got.QSOTime != "2026-06-27T20:42:07Z" || got.Band != "20m" || got.Mode != "USB" {
		t.Errorf("got %+v", got)
	}
	if got.Notes != "QRP & <5W> tnx 😀" || got.FrequencyHz != 14_250_000 || got.Operator != "K3CPK" || got.Station != "GOTA-PC" {
		t.Errorf("got %+v", got)
	}
	if got.RSTSent != "" || got.RSTReceived != "59" || got.ExchangeClass != "2A" || got.Section != "CT" {
		t.Errorf("got %+v", got)
	}
}

func TestAClientEditKeepsTheContactsIdentity(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	edited := fieldDayFields("W1AW", 1, map[string]string{"CLASS": "3A", "POINTS": "1", "QSLR": "N", "QSLS": "N"})
	server.send(clientTx("ADD", clientRecord(fieldDayFields("W1AW", 1, nil))), clientTx("EDIT", clientRecord(edited)))
	eventually(t, "the edit", func() bool { return len(sink.snapshot()) == 2 })

	first, second := sink.snapshot()[0], sink.snapshot()[1]
	if second.ReplacesKey != "" || second.IdempotencyKey != first.IdempotencyKey || second.ExchangeClass != "3A" {
		t.Errorf("edit = %+v", second)
	}
}

func TestContactsSurviveThePCsDateAndNumberFormats(t *testing.T) {
	tests := map[string]map[string]string{
		"de-DE": {"DATESTR": "2026.06.27", "FREQUENCY": "14,25000"},
		"sv-SE": {"DATESTR": "2026-06-27"},
		"fi-FI": {"TIMEONSTR": "20.42.07"},
		"en-US": {"FREQUENCY": "14.25000"},
	}
	want := "2026-06-27T20:42:07Z"
	for name, overrides := range tests {
		got, err := toContact(fieldDayFields("W1AW", 1, overrides), "")
		if err != nil || got.QSOTime != want {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}

	bare, err := toContact(fieldDayFields("W1AW", 1, map[string]string{"TIMEONSTR": "2042"}), "")
	if err != nil || bare.QSOTime != "2026-06-27T20:42:00Z" {
		t.Errorf("post-contest entry: %+v, %v", bare, err)
	}
}

// An operator who re-enters a contact after the client's five-second timeout
// leaves two records with the same identity.
func TestAReEnteredContactStaysUntilBothRecordsAreGone(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	same := clientRecord(fieldDayFields("W1AW", 1, nil))
	server.send(clientTx("ADD", same), clientTx("ADD", same), clientTx("ADD", clientRecord(fieldDayFields("K1ABC", 3, nil))),
		clientTx("DELETE", "1"), clientTx("DELETE", "3"))
	eventually(t, "the other contact's delete", func() bool {
		contacts := sink.snapshot()
		return len(contacts) > 0 && contacts[len(contacts)-1].Callsign == "K1ABC" && contacts[len(contacts)-1].Deleted
	})
	for _, c := range sink.snapshot() {
		if c.Deleted && c.Callsign == "W1AW" {
			t.Fatal("deleted the contact while another record still has it")
		}
	}

	server.send(clientTx("DELETE", "2"))
	eventually(t, "the last record's delete", func() bool {
		contacts := sink.snapshot()
		return len(contacts) > 0 && contacts[len(contacts)-1].Deleted && contacts[len(contacts)-1].Callsign == "W1AW"
	})
}

func TestEditingOneOfTwoSameRecordsLeavesTheOther(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	runSource(t, server, sink, nil)

	same := clientRecord(fieldDayFields("W1AW", 1, nil))
	server.send(clientTx("ADD", same), clientTx("ADD", same), clientTx("EDIT", clientRecord(fieldDayFields("K1ABC", 2, nil))))
	eventually(t, "the edit", func() bool {
		contacts := sink.snapshot()
		return len(contacts) > 0 && contacts[len(contacts)-1].Callsign == "K1ABC"
	})

	contacts := sink.snapshot()
	if got := contacts[len(contacts)-1]; got.ReplacesKey != "" {
		t.Errorf("the edit would rename a contact another record still has: %+v", got)
	}
}

// A slow PC's reads land part-way through a message, and a large log has to come
// through whole.
func TestALargeLogArrivesWholeThroughSmallReads(t *testing.T) {
	var stream []byte
	stream = append(stream, encode(clientTx("CLEAR", ""))...)
	for number := 1; number <= 600; number++ {
		stream = append(stream, encode(clientTx("GET", clientRecord(fieldDayFields(fmt.Sprintf("K%dFDC", number), number, nil))))...)
	}
	stream = append(stream, encode(clientTx("LIST", ""))...)

	var frames decoder
	var messages []string
	for start := 0; start < len(stream); start += 4093 {
		got, err := frames.feed(stream[start:min(start+4093, len(stream))])
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, got...)
	}

	if len(messages) != 602 || transaction(messages[0]) != "CLEAR" || transaction(messages[601]) != "LIST" {
		t.Fatalf("messages = %d", len(messages))
	}
	if got := parseRecord(field(messages[600], "XMLDATA"))["CALL"]; got != "K600FDC" {
		t.Errorf("last call = %q", got)
	}
}
