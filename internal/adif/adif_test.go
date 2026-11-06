// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package adif

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

func scanOne(t *testing.T, text string) map[string]string {
	t.Helper()
	records, _, _ := Scan([]byte(text))
	if len(records) != 1 {
		t.Fatalf("%q: got %d records", text, len(records))
	}
	return records[0].Fields
}

func TestTheHeaderIsNotARecord(t *testing.T) {
	text := "Exported by a logger <with a note>\n<ADIF_VER:5>3.1.4 <PROGRAMID:6>WSJT-X <EOH>\n<call:5>K1ABC <eor>\n"
	records, consumed, sawTag := Scan([]byte(text))
	if len(records) != 1 || records[0].Fields["CALL"] != "K1ABC" || records[0].Fields["PROGRAMID"] != "" {
		t.Fatalf("got %+v", records)
	}
	if records[0].Raw != "<call:5>K1ABC <eor>" {
		t.Errorf("raw %q", records[0].Raw)
	}
	if !sawTag || text[consumed:] != "\n" {
		t.Errorf("consumed %d, sawTag %v", consumed, sawTag)
	}
}

func TestOnlyFinishedRecordsAreConsumed(t *testing.T) {
	first := "<CALL:5>K1ABC<EOR>"
	full := first + "<CALL:5>W2XYZ<QSO_DATE:8>20260627<EOR>"
	for cut := len(first); cut < len(full); cut++ {
		records, consumed, _ := Scan([]byte(full[:cut]))
		if len(records) != 1 || consumed != len(first) {
			t.Fatalf("cut at %d (%q): %d records, consumed %d", cut, full[:cut], len(records), consumed)
		}
	}
	records, consumed, _ := Scan([]byte(full))
	if len(records) != 2 || consumed != len(full) || records[1].Fields["QSO_DATE"] != "20260627" {
		t.Errorf("%d records, consumed %d", len(records), consumed)
	}
}

func TestValuesAreReadByLength(t *testing.T) {
	tests := []struct{ name, text, want string }{
		{"a record end inside a value", "<NOTES:10>see <EOR>.<CALL:1>K<EOR>", "see <EOR>."},
		{"a tag inside a value", "<NOTES:14>a <CALL:1>Z b.<CALL:1>K<EOR>", "a <CALL:1>Z b."},
		{"a type indicator", "<NOTES:2:S>hi<CALL:1>K<EOR>", "hi"},
		{"length counted in bytes", "<NOTES:5>Zoë!<CALL:1>K<EOR>", "Zoë!"},
		{"length counted in characters", "<NOTES:4>Zoë!<CALL:1>K<EOR>", "Zoë!"},
		{"length too long", "<NOTES:50>short<CALL:1>K<EOR>", "short"},
		{"length too short", "<NOTES:2>short<CALL:1>K<EOR>", "sh"},
		{"an enormous length", "<NOTES:99999999999999999999>short<CALL:1>K<EOR>", "short"},
		{"a zero length", "<NOTES:0><CALL:1>K<EOR>", ""},
		{"Windows-1252 text", "<NOTES:4>caf\xe9<CALL:1>K<EOR>", "café"},
		{"Windows-1252 punctuation", "<NOTES:3>\x93a\x94<CALL:1>K<EOR>", "“a”"},
		{"a stray bracket", "<NOTES:5>1 < 2<CALL:1>K<EOR>", "1 < 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := scanOne(t, tt.text)
			if fields["NOTES"] != tt.want || fields["CALL"] != "K" {
				t.Errorf("got %q and call %q", fields["NOTES"], fields["CALL"])
			}
		})
	}
}

func TestOtherTextIsNotADIF(t *testing.T) {
	for _, text := range []string{"", "SQLite format 3\x00", "<html><body>", "no brackets at all", "<>< >", "<:5>abc"} {
		records, consumed, sawTag := Scan([]byte(text))
		if len(records) != 0 || consumed != 0 || sawTag {
			t.Errorf("%q: %d records, consumed %d, sawTag %v", text, len(records), consumed, sawTag)
		}
	}
	// UTF-16 has a NUL after every letter, so no tag can match.
	utf16 := strings.Join(strings.Split("<CALL:5>K1ABC<EOR>", ""), "\x00")
	if _, _, sawTag := Scan([]byte(utf16)); sawTag {
		t.Error("read UTF-16 as tags")
	}
}

func TestEmptyRecordsAreSkipped(t *testing.T) {
	text := "<EOR><EOR><CALL:1>K<EOR>"
	records, consumed, _ := Scan([]byte(text))
	if len(records) != 1 || consumed != len(text) {
		t.Errorf("%d records, consumed %d", len(records), consumed)
	}
}

func TestToContactMapsAWSJTXRecord(t *testing.T) {
	fields := scanOne(t, `<call:5>K1ABC <gridsquare:4>FN42 <mode:4>MFSK <submode:3>FT4 <rst_sent:3>-12 <rst_rcvd:3>+05 `+
		`<qso_date:8>20260627 <time_on:6>184207 <qso_date_off:8>20260627 <time_off:6>184237 <band:3>20M <freq:9>14.080000 `+
		`<station_callsign:5>W1AW <my_gridsquare:4>FN31 <tx_pwr:3>100 <comment:5>hello <name:3>Ann <operator:5>n2egb <srx_string:6>2H EMA <eor>`)
	got, when, err := ToContact(fields)
	if err != nil {
		t.Fatal(err)
	}
	if !when.Equal(time.Date(2026, 6, 27, 18, 42, 7, 0, time.UTC)) {
		t.Errorf("time %v", when)
	}
	got, dropped, err := contact.Normalize(got)
	if err != nil || len(dropped) != 0 {
		t.Fatalf("%v, dropped %v", err, dropped)
	}
	want := contact.Contact{
		IdempotencyKey: got.IdempotencyKey, Callsign: "K1ABC", QSOTime: "2026-06-27T18:42:07Z", Band: "20m", Mode: "FT4",
		FrequencyHz: 14_080_000, Operator: "N2EGB", ExchangeClass: "2H", Section: "EMA", PowerWatts: 100,
		RSTSent: "-12", RSTReceived: "+05", GridSquare: "FN42", ContactName: "Ann", Notes: "hello",
	}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestToContactReadsTheExchangeTags(t *testing.T) {
	got, _, err := ToContact(map[string]string{
		"CALL": "K1ABC", "QSO_DATE": "20260627", "TIME_ON": "1842", "CLASS": "3A", "ARRL_SECT": "CT", "SRX_STRING": "2H EMA", "NOTES": "from notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExchangeClass != "3A" || got.Section != "CT" || got.Notes != "from notes" || got.QSOTime != "2026-06-27T18:42:00Z" {
		t.Errorf("got %+v", got)
	}
}

func TestToContactLeavesOutValuesItCannotTrust(t *testing.T) {
	got, _, err := ToContact(map[string]string{
		"QSO_DATE": "20260627", "TIME_ON": "1842", "BAND": "40m", "FREQ": "14.074", "OPERATOR": "not a call", "TX_PWR": "lots", "SRX_STRING": "59 001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.FrequencyHz != 0 || got.Operator != "" || got.PowerWatts != 0 || got.ExchangeClass != "" || got.Section != "" {
		t.Errorf("got %+v", got)
	}
}

func TestPower(t *testing.T) {
	for value, want := range map[string]int{"100": 100, "99.6": 100, "0.5": 1, "0.1": 1, "0": 0, "-5": 0, "": 0, "5W": 0} {
		if got := powerWatts(value); got != want {
			t.Errorf("%q: got %d, want %d", value, got, want)
		}
	}
}

func TestAnUnreadableTimeIsInvalid(t *testing.T) {
	for _, fields := range []map[string]string{
		{},
		{"QSO_DATE": "20260627"},
		{"TIME_ON": "1842"},
		{"QSO_DATE": "20261399", "TIME_ON": "1842"},
		{"QSO_DATE": "20260627", "TIME_ON": "2561"},
		{"QSO_DATE": "2026-06-27", "TIME_ON": "1842"},
		{"QSO_DATE": "20260627", "TIME_ON": "18"},
	} {
		if _, _, err := ToContact(fields); !errors.Is(err, contact.ErrInvalid) {
			t.Errorf("%v: got %v", fields, err)
		}
	}
}
