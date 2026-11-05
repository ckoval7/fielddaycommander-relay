// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package contact

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func valid() Contact {
	return Contact{Callsign: "w1aw", QSOTime: "2026-06-27T18:05:00Z", Band: "20m", Mode: "ph"}
}

func TestNormalizeCleansFields(t *testing.T) {
	c := valid()
	c.QSOTime = "2026-06-27T14:05:00-04:00"
	c.Section = " ct "
	c.ExchangeClass = "3a"
	c.GridSquare = "fn31PR"
	c.Notes = "line\x07one"

	got, dropped, err := Normalize(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped %v", dropped)
	}
	if got.Callsign != "W1AW" || got.QSOTime != "2026-06-27T18:05:00Z" || got.Mode != "PH" {
		t.Errorf("got %+v", got)
	}
	if got.Section != "CT" || got.ExchangeClass != "3A" || got.GridSquare != "FN31pr" || got.Notes != "lineone" {
		t.Errorf("got %+v", got)
	}
}

// A contact that would lose a value here would otherwise be rejected whole.
func TestNormalizeDropsOptionalValuesThatDoNotFit(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Contact)
		field  string
	}{
		{"cw style report", func(c *Contact) { c.RSTSent = "5NN" }, "rst_sent"},
		{"zero report digit", func(c *Contact) { c.RSTReceived = "509" }, "rst_received"},
		{"unreadable grid", func(c *Contact) { c.GridSquare = "ZZ99" }, "grid_square"},
		{"low frequency", func(c *Contact) { c.FrequencyHz = 5 }, "frequency_hz"},
		{"huge power", func(c *Contact) { c.PowerWatts = 100_001 }, "power_watts"},
		{"long section", func(c *Contact) { c.Section = "ABCDEFGHIJK" }, "section"},
		{"long mode", func(c *Contact) { c.Mode = strings.Repeat("M", 21) }, "mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.change(&c)
			_, dropped, err := Normalize(c)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(dropped, tt.field) {
				t.Errorf("dropped %v, want %s", dropped, tt.field)
			}
		})
	}
}

func TestNormalizeKeepsLongTextShortened(t *testing.T) {
	c := valid()
	c.GridSquare = "FN31pr45"
	c.Notes = strings.Repeat("é", 600)
	c.Station = strings.Repeat("s", 60)

	got, dropped, err := Normalize(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped %v", dropped)
	}
	if got.GridSquare != "FN31pr" || len([]rune(got.Notes)) != 500 || len(got.Station) != 50 {
		t.Errorf("grid %q, notes %d, station %d", got.GridSquare, len([]rune(got.Notes)), len(got.Station))
	}
}

func TestNormalizeRefusesAContactWithoutCallOrTime(t *testing.T) {
	tests := map[string]func(*Contact){
		"blank call":  func(c *Contact) { c.Callsign = " " },
		"markup call": func(c *Contact) { c.Callsign = "<script>" },
		"long call":   func(c *Contact) { c.Callsign = strings.Repeat("W", 21) },
		"blank time":  func(c *Contact) { c.QSOTime = "" },
		"local time":  func(c *Contact) { c.QSOTime = "2026-06-27 18:05:00" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			c := valid()
			change(&c)
			if _, _, err := Normalize(c); !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestKeyIsStableAcrossFieldsThatDoNotIdentifyTheContact(t *testing.T) {
	a, _, _ := Normalize(valid())

	other := valid()
	other.Callsign = " W1AW "
	other.Mode = "PH"
	other.Station = "Phone Tent"
	other.Operator = "K3CPK"
	other.Section = "CT"
	b, _, _ := Normalize(other)

	if a.IdempotencyKey != b.IdempotencyKey {
		t.Errorf("%s != %s", a.IdempotencyKey, b.IdempotencyKey)
	}
}

func TestKeyChangesWithFieldsThatIdentifyTheContact(t *testing.T) {
	base, _, _ := Normalize(valid())
	changes := map[string]func(*Contact){
		"call": func(c *Contact) { c.Callsign = "K1ABC" },
		"time": func(c *Contact) { c.QSOTime = "2026-06-27T18:05:01Z" },
		"band": func(c *Contact) { c.Band = "40m" },
		"mode": func(c *Contact) { c.Mode = "CW" },
	}
	for name, change := range changes {
		c := valid()
		change(&c)
		got, _, _ := Normalize(c)
		if got.IdempotencyKey == base.IdempotencyKey {
			t.Errorf("%s: key did not change", name)
		}
	}
}

func TestKeyUsesTheBandTheFrequencyIsIn(t *testing.T) {
	named, _, _ := Normalize(valid())

	byFreq := valid()
	byFreq.Band = ""
	byFreq.FrequencyHz = 14_250_000
	got, _, _ := Normalize(byFreq)

	if got.IdempotencyKey != named.IdempotencyKey {
		t.Error("a 20m frequency and the band name 20m should give the same key")
	}
}

// The pinned contract is what the server promises, so the struct and rules are
// checked against it.
func TestMatchesThePinnedContract(t *testing.T) {
	raw, err := os.ReadFile("../../api/relay-v1.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(raw)
	start := strings.Index(spec, "\n    Contact:\n")
	end := strings.Index(spec, "\n    ContactResult:\n")
	if start < 0 || end < start {
		t.Fatal("Contact schema not found")
	}
	schema := spec[start:end]

	properties := regexp.MustCompile(`(?m)^        ([a-z_]+):`).FindAllStringSubmatch(schema, -1)
	var want []string
	for _, match := range properties {
		want = append(want, match[1])
	}

	var got []string
	for _, field := range reflect.VisibleFields(reflect.TypeFor[Contact]()) {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		got = append(got, name)
	}

	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("contract fields %v\nstruct fields %v", want, got)
	}

	for _, pattern := range []string{rstPattern.String(), callsignNoCase()} {
		if !strings.Contains(schema, pattern) {
			t.Errorf("contract doesn't contain pattern %s", pattern)
		}
	}

	key, _, _ := Normalize(valid())
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`).MatchString(key.IdempotencyKey) {
		t.Errorf("key %q breaks the contract's pattern", key.IdempotencyKey)
	}
}

// The contract allows either case; the relay always sends upper case.
func callsignNoCase() string {
	return strings.Replace(callsignPattern.String(), "A-Z", "A-Za-z", 1)
}
