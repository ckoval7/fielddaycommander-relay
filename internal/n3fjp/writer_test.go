// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

func serverContact(t *testing.T, call string, change func(*contact.Contact)) contact.Contact {
	t.Helper()
	c := contact.Contact{Callsign: call, QSOTime: "2026-06-27T20:42:07Z", Band: "20m", Mode: "PH", Section: "CT", ExchangeClass: "2A", Station: "Web", Operator: "K3CPK"}
	if change != nil {
		change(&c)
	}
	normalized, _, err := contact.Normalize(c)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

// loadedSource is a source whose server has finished sending the log.
func loadedSource(t *testing.T, server *fakeServer, sink *fakeSink, records ...string) *Source {
	t.Helper()
	source := runSource(t, server, sink, nil)
	messages := []string{tx("CLEAR", "")}
	for _, record := range records {
		messages = append(messages, tx("GET", record))
	}
	server.send(append(messages, tx("LIST", ""))...)
	eventually(t, "the log to load", source.Ready)
	return source
}

func sentMessages(server *fakeServer, kind string) []string {
	var found []string
	for _, message := range server.sent() {
		if transaction(message) == kind {
			found = append(found, message)
		}
	}
	return found
}

func TestNothingIsWrittenBeforeTheLogLoads(t *testing.T) {
	server := newFakeServer(t)
	source := runSource(t, server, &fakeSink{}, nil)

	if err := source.Add(context.Background(), serverContact(t, "W1AW", nil)); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v", err)
	}

	server.send(tx("CLEAR", ""), tx("GET", record2("K1ABC", 1, "")))
	time.Sleep(20 * time.Millisecond)
	if source.Ready() {
		t.Error("the log hasn't finished loading")
	}
	if got := sentMessages(server, "ADD"); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
}

func TestAddSendsARecordAndItsEchoIsNotUploaded(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	source := loadedSource(t, server, sink)

	c := serverContact(t, "W1AW", func(c *contact.Contact) {
		c.FrequencyHz = 14_250_000
		c.Notes = "needs <EOR> care"
	})
	if err := source.Add(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ADD", func() bool { return len(sentMessages(server, "ADD")) == 1 })

	message := sentMessages(server, "ADD")[0]
	fields := parseRecord(field(message, "XMLDATA"))
	want := map[string]string{"CALL": "W1AW", "BAND": "20", "MODE": "PH", "MODECONTEST": "PH", "DATESTR": "2026/06/27", "TIMEONSTR": "20:42:07", "FREQUENCY": "14.25000", "SECTION": "CT", "CLASS": "2A", "STATION": "Web", "OPERATOR": "K3CPK", "PRIMARYKEY": "1", "COMMENTS": "needs  EOR  care"}
	for name, value := range want {
		if fields[name] != value {
			t.Errorf("%s = %q, want %q", name, fields[name], value)
		}
	}
	if strings.Contains(message, "<EOR> care") {
		t.Error("a value could break the message for every PC")
	}

	sink.mu.Lock()
	remembered := sink.remembered[len(sink.remembered)-1]
	sink.mu.Unlock()
	echo, err := toContact(fields, "FDRELAY")
	if err != nil {
		t.Fatal(err)
	}
	if echo != remembered {
		t.Errorf("the echo reads differently from what was remembered:\n%+v\n%+v", echo, remembered)
	}
	if echo.IdempotencyKey != c.IdempotencyKey {
		t.Errorf("the round trip changed the key:\n%+v\n%+v", echo, c)
	}
}

func TestAddSkipsAContactTheLogAlreadyHas(t *testing.T) {
	server := newFakeServer(t)
	source := loadedSource(t, server, &fakeSink{}, record2("W1AW", 1, ""))

	existing, err := toContact(parseRecord(record2("W1AW", 1, "")), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Add(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	if got := sentMessages(server, "ADD"); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
}

// The trip through the server isn't exact, so an edit touches only what the server
// changed, and leaves what the operator typed.
func TestEditChangesOnlyWhatTheServerChanged(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	logged := fieldDayFields("W1AW", 7, map[string]string{"RSTS": "5NN", "RSTR": "599", "COUNTRYWORKED": "USA"})
	source := loadedSource(t, server, sink, clientRecord(logged))

	current, err := toContact(logged, "")
	if err != nil {
		t.Fatal(err)
	}
	before := current
	after := current
	after.Section = "NH"

	if err := source.Edit(context.Background(), current.IdempotencyKey, before, after); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the EDIT", func() bool { return len(sentMessages(server, "EDIT")) == 1 })

	fields := parseRecord(field(sentMessages(server, "EDIT")[0], "XMLDATA"))
	if fields["SECTION"] != "NH" || fields["PRIMARYKEY"] != "7" {
		t.Errorf("fields = %v", fields)
	}
	if fields["RSTS"] != "5NN" || fields["RSTR"] != "599" || fields["COUNTRYWORKED"] != "USA" || fields["SERIALNOS"] != "001" {
		t.Errorf("the edit changed what the operator typed: %v", fields)
	}
}

func TestAnEditThatChangesNothingSendsNothing(t *testing.T) {
	server := newFakeServer(t)
	logged := fieldDayFields("W1AW", 1, map[string]string{"SECTION": "NH"})
	source := loadedSource(t, server, &fakeSink{}, clientRecord(logged))
	current, _ := toContact(logged, "")

	before := current
	before.Section = "CT"
	if err := source.Edit(context.Background(), current.IdempotencyKey, before, current); err != nil {
		t.Fatal(err)
	}
	if got := sentMessages(server, "EDIT"); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
}

func TestAClearedValueLeavesTheRecord(t *testing.T) {
	server := newFakeServer(t)
	logged := fieldDayFields("W1AW", 1, map[string]string{"COMMENTS": "old"})
	source := loadedSource(t, server, &fakeSink{}, clientRecord(logged))
	current, _ := toContact(logged, "")

	after := current
	after.Notes = ""
	if err := source.Edit(context.Background(), current.IdempotencyKey, current, after); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the EDIT", func() bool { return len(sentMessages(server, "EDIT")) == 1 })
	if _, there := parseRecord(field(sentMessages(server, "EDIT")[0], "XMLDATA"))["COMMENTS"]; there {
		t.Error("the note should be gone")
	}
}

func TestEditOfARecordTheLogLacksAddsIt(t *testing.T) {
	server := newFakeServer(t)
	source := loadedSource(t, server, &fakeSink{})
	c := serverContact(t, "W1AW", nil)

	if err := source.Edit(context.Background(), "unknown-key", c, c); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the ADD", func() bool { return len(sentMessages(server, "ADD")) == 1 })
}

func TestDeleteUsesTheRecordNumberAndItsEchoIsNotUploaded(t *testing.T) {
	server := newFakeServer(t)
	sink := &fakeSink{}
	source := loadedSource(t, server, sink, record2("K1ABC", 3, ""), record2("W1AW", 9, ""))
	target, _ := toContact(parseRecord(record2("W1AW", 9, "")), "")

	if err := source.Delete(context.Background(), target.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the DELETE", func() bool { return len(sentMessages(server, "DELETE")) == 1 })
	if got := field(sentMessages(server, "DELETE")[0], "XMLDATA"); got != "9" {
		t.Errorf("deleted %q", got)
	}

	sink.mu.Lock()
	remembered := sink.remembered[len(sink.remembered)-1]
	sink.mu.Unlock()
	echoed := deleteOf(recordFor(target, nil))
	if echoed != remembered {
		t.Errorf("the echo reads differently:\n%+v\n%+v", echoed, remembered)
	}
}

func TestDeleteOfAContactTheLogLacksDoesNothing(t *testing.T) {
	server := newFakeServer(t)
	source := loadedSource(t, server, &fakeSink{})
	if err := source.Delete(context.Background(), "unknown-key"); err != nil {
		t.Fatal(err)
	}
	if got := sentMessages(server, "DELETE"); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
}

func TestWritesFailOnceDisconnected(t *testing.T) {
	server := newFakeServer(t)
	source := loadedSource(t, server, &fakeSink{})
	server.dropConnection()
	eventually(t, "the source to notice", func() bool { return !source.Ready() })

	if err := source.Add(context.Background(), serverContact(t, "W1AW", nil)); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v", err)
	}
}

func TestBandsSurviveTheTripThroughN3FJP(t *testing.T) {
	for _, band := range []string{"2200m", "630m", "160m", "80m", "40m", "20m", "15m", "10m", "6m", "2m", "1.25m", "70cm", "33cm", "23cm", "13cm", "9cm", "5cm", "3cm", "1.25cm", "6mm", "4mm", "2.5mm", "2mm", "1mm", "Satellite"} {
		if got := serverBand(n3fjpBand(band)); got != band {
			t.Errorf("%s came back as %s (sent %s)", band, got, n3fjpBand(band))
		}
	}
}

func TestModeGroupsMatchTheServer(t *testing.T) {
	for mode, want := range map[string]string{"PH": "PH", "ssb": "PH", "FM": "PH", "DMR": "PH", "CW": "CW", "CW-R": "CW", "FT8": "DIG", "DIG": "DIG", "SSTV": "DIG", "": ""} {
		if got := modeGroup(mode); got != want {
			t.Errorf("%q = %q, want %q", mode, got, want)
		}
	}
}
