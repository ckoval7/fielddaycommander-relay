// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestDecoderHandlesReadsSplitAnywhere(t *testing.T) {
	want := []string{"<BOR>first é 𝄞<EOR>", "<BOR>second<EOR>"}
	wire := append(encode(want[0]), encode(want[1])...)

	for split := range len(wire) {
		var frames decoder
		first, err := frames.feed(wire[:split])
		if err != nil {
			t.Fatal(err)
		}
		second, err := frames.feed(wire[split:])
		if err != nil {
			t.Fatal(err)
		}
		if got := append(first, second...); !slices.Equal(got, want) {
			t.Fatalf("split at %d: %q", split, got)
		}
	}
}

// Bytes that look like the terminator but straddle two characters aren't one.
func TestDecoderIgnoresAMisalignedTerminator(t *testing.T) {
	var frames decoder
	frame := []byte{'A', 3, 0, 4, 0, 7, 0, 0}
	frame = append(frame, terminator...)

	messages, err := frames.feed(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Errorf("messages = %q", messages)
	}
}

func TestDecoderRefusesAnEndlessMessage(t *testing.T) {
	var frames decoder
	if _, err := frames.feed(make([]byte, maxFrameBytes+1)); !errors.Is(err, errFrameTooLong) {
		t.Errorf("err = %v", err)
	}
	if len(frames.buffer) != 0 {
		t.Error("the buffer should be emptied")
	}
}

func TestFieldIgnoresCaseAndKeepsNonASCIIOffsets(t *testing.T) {
	message := "<bor>ıß<ntwk><from>Köln-PC</FROM><Transaction>add</TRANSACTION>"
	if got := field(message, "FROM"); got != "Köln-PC" {
		t.Errorf("got %q", got)
	}
	if got := transaction(message); got != "ADD" {
		t.Errorf("got %q", got)
	}
	if got := field("<FROM>x", "FROM"); got != "" {
		t.Errorf("got %q", got)
	}
	if got := field("</FROM>x<FROM>", "FROM"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestAChatIsNeverATransaction(t *testing.T) {
	chat := "<BOR><MESG><TO></TO><FROM>X</FROM><MSGTXT><NTWK><TRANSACTION>DELETE</TRANSACTION><XMLDATA>1</XMLDATA></MSGTXT><EOR>"
	if got := transaction(chat); got != "" {
		t.Errorf("got %q", got)
	}
	if got := transaction("<BOR><WHO><EOR>"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestParseRecord(t *testing.T) {
	fields := parseRecord("<FLDBAND>20</FLDBAND><fldCall> w1aw </fldCALL><FLDCALL>K1ABC</FLDCALL><FLDNOTHING>open<FLDMODE>PH</FLDMODE><FLD>x</FLD><FLDA B>y</FLDA B>")
	want := map[string]string{"BAND": "20", "CALL": "w1aw", "MODE": "PH"}
	if len(fields) != len(want) {
		t.Errorf("fields = %v", fields)
	}
	for name, value := range want {
		if fields[name] != value {
			t.Errorf("%s = %q", name, fields[name])
		}
	}
	if got := parseRecord(strings.Repeat("<FLD", 1000)); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestFrequencyIsReadInAnyLocale(t *testing.T) {
	tests := []struct {
		frequency, band string
		want            int64
	}{
		{"14.07400", "20m", 14_074_000},
		{"14,07400", "20m", 14_074_000},
		{"1,296.10000", "23cm", 1_296_100_000},
		{"1.296,10000", "23cm", 1_296_100_000},
		{"1,296", "23cm", 1_296_000_000},
		{"1 296,10000", "23cm", 1_296_100_000},
		{"14.07400", "", 14_074_000},
		{"7.07400", "20m", 0},
		{"99.0", "20m", 0},
		{"abc", "20m", 0},
		{"", "20m", 0},
	}
	for _, tt := range tests {
		if got := frequencyHz(tt.frequency, tt.band); got != tt.want {
			t.Errorf("frequencyHz(%q, %q) = %d, want %d", tt.frequency, tt.band, got, tt.want)
		}
	}
}

func TestBandNames(t *testing.T) {
	tests := map[string]string{"20": "20m", "1.25": "1.25m", "70": "70cm", "6CM": "5cm", "6cm": "5cm", "6mm": "6mm", "sat": "Satellite", " ": ""}
	for band, want := range tests {
		if got := serverBand(band); got != want {
			t.Errorf("serverBand(%q) = %q, want %q", band, got, want)
		}
	}
}

func TestQSOTimeAcceptsThePCsSeparators(t *testing.T) {
	tests := []struct{ date, clock, want string }{
		{"2026/06/27", "18:42:07", "2026-06-27T18:42:07Z"},
		{"2026.06.27", "18.42.07", "2026-06-27T18:42:07Z"},
		{"2026-6-7", "1842", "2026-06-07T18:42:00Z"},
		{"2026/06/27", "8:05:00", "2026-06-27T08:05:00Z"},
	}
	for _, tt := range tests {
		got, err := qsoTime(tt.date, tt.clock)
		if err != nil || got != tt.want {
			t.Errorf("qsoTime(%q, %q) = %q, %v", tt.date, tt.clock, got, err)
		}
	}
	for _, bad := range [][2]string{{"", ""}, {"2026/13/01", "18:00:00"}, {"2026/06/31", "18:00:00"}, {"2026/06/27", "25:00:00"}, {"27/06/2026", "18:00:00"}} {
		if got, err := qsoTime(bad[0], bad[1]); err == nil {
			t.Errorf("qsoTime(%q, %q) = %q", bad[0], bad[1], got)
		}
	}
}

func TestPowerWatts(t *testing.T) {
	tests := map[string]int{"100": 100, "4.5": 5, "4,5": 5, "5W": 5, " 5 w ": 5, "0": 0, "-1": 0, "": 0, "abc": 0, "1e99": 0}
	for power, want := range tests {
		if got := powerWatts(power); got != want {
			t.Errorf("powerWatts(%q) = %d, want %d", power, got, want)
		}
	}
}

func TestToContactMapsARecord(t *testing.T) {
	got, err := toContact(map[string]string{
		"CALL": "w1aw", "BAND": "20", "MODE": "", "MODECONTEST": "PH", "CLASS": "2a", "SECTION": "ct",
		"DATESTR": "2026/06/27", "TIMEONSTR": "18:42:07", "FREQUENCY": "14,25000", "OPERATOR": "k3cpk",
		"POWER": "100", "RSTS": "59", "RSTR": "5NN", "GRIDR": "fn31", "NAMER": "Hiram", "COMMENTS": "hi",
		"COMPUTERNAME": "GOTA-PC",
	}, "FROM-PC")
	if err != nil {
		t.Fatal(err)
	}
	if got.Callsign != "W1AW" || got.Band != "20m" || got.Mode != "PH" || got.FrequencyHz != 14_250_000 {
		t.Errorf("got %+v", got)
	}
	if got.Operator != "K3CPK" || got.Station != "GOTA-PC" || got.PowerWatts != 100 || got.ExchangeClass != "2A" || got.Section != "CT" {
		t.Errorf("got %+v", got)
	}
	if got.RSTSent != "59" || got.RSTReceived != "" || got.GridSquare != "FN31" || got.ContactName != "Hiram" || got.Notes != "hi" {
		t.Errorf("got %+v", got)
	}
	if got.QSOTime != "2026-06-27T18:42:07Z" || got.IdempotencyKey == "" {
		t.Errorf("got %+v", got)
	}
}

func TestToContactPicksTheStation(t *testing.T) {
	base := map[string]string{"CALL": "W1AW", "DATESTR": "2026/06/27", "TIMEONSTR": "18:42:07"}
	got, _ := toContact(base, "FROM-PC")
	if got.Station != "FROM-PC" {
		t.Errorf("station = %q", got.Station)
	}
	got, _ = toContact(base, "")
	if got.Station != "N3FJP" {
		t.Errorf("station = %q", got.Station)
	}
}

func TestToContactRefusesWhatItCannotRead(t *testing.T) {
	for name, fields := range map[string]map[string]string{
		"no time": {"CALL": "W1AW"},
		"no call": {"DATESTR": "2026/06/27", "TIMEONSTR": "18:42:07"},
	} {
		if got, err := toContact(fields, ""); err == nil {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}
