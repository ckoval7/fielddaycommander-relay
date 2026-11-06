// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package contact defines a contact as the server's relay API takes it, and
// the rules that make one safe to queue.
//
// The server rejects a whole contact when any one field breaks its rules, so
// Normalize drops an optional value that doesn't fit and keeps the contact.
// Only a contact with no usable callsign or time is refused.
package contact

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits from the relay API contract (api/relay-v1.openapi.yaml).
const (
	maxCallsign  = 20
	maxBand      = 10
	maxMode      = 20
	maxOperator  = 20
	maxStation   = 50
	maxClass     = 10
	maxSection   = 10
	maxName      = 50
	maxNotes     = 500
	minFreqHz    = 100_000
	maxFreqHz    = 300_000_000_000
	maxWatts     = 100_000
	timeLayout   = "2006-01-02T15:04:05Z"
	keyVersion   = "v1"
	keyByteCount = 24
)

var (
	callsignPattern = regexp.MustCompile(`^[A-Z0-9/]+$`)
	rstPattern      = regexp.MustCompile(`^([1-5][1-9][1-9]?|[+-][0-9]{1,2})$`)
	gridPattern     = regexp.MustCompile(`^[A-Ra-r]{2}[0-9]{2}([A-Xa-x]{2}([0-9]{2})?)?$`)
)

// ErrInvalid marks a contact that can't be queued.
var ErrInvalid = errors.New("invalid contact")

// Contact is one contact in the relay API's format. An empty optional field
// is left out of the request.
type Contact struct {
	IdempotencyKey string `json:"idempotency_key"`
	ReplacesKey    string `json:"replaces_key,omitempty"`
	Deleted        bool   `json:"deleted,omitempty"`
	UUID           string `json:"uuid,omitempty"`
	Callsign       string `json:"callsign"`
	QSOTime        string `json:"qso_time"`
	Band           string `json:"band,omitempty"`
	Mode           string `json:"mode,omitempty"`
	FrequencyHz    int64  `json:"frequency_hz,omitempty"`
	Operator       string `json:"operator,omitempty"`
	Station        string `json:"station,omitempty"`
	ExchangeClass  string `json:"exchange_class,omitempty"`
	Section        string `json:"section,omitempty"`
	PowerWatts     int    `json:"power_watts,omitempty"`
	RSTSent        string `json:"rst_sent,omitempty"`
	RSTReceived    string `json:"rst_received,omitempty"`
	GridSquare     string `json:"grid_square,omitempty"`
	ContactName    string `json:"contact_name,omitempty"`
	Notes          string `json:"notes,omitempty"`
}

// Normalize returns c cleaned to the API's rules with its idempotency key set,
// and the names of any optional fields it dropped.
//
// It returns an error wrapping ErrInvalid when the callsign or time is unusable.
func Normalize(c Contact) (Contact, []string, error) {
	var dropped []string
	drop := func(field string) string {
		dropped = append(dropped, field)
		return ""
	}
	// fit returns a cleaned value, or drops it when it's too long.
	fit := func(field, value string, limit int) string {
		if value = clean(value); utf8.RuneCountInString(value) > limit {
			return drop(field)
		}
		return value
	}

	out := Contact{ReplacesKey: c.ReplacesKey, Deleted: c.Deleted, UUID: c.UUID}

	out.Callsign = strings.ToUpper(clean(c.Callsign))
	if out.Callsign == "" || len(out.Callsign) > maxCallsign || !callsignPattern.MatchString(out.Callsign) {
		return Contact{}, nil, fmt.Errorf("%w: callsign %q", ErrInvalid, truncate(out.Callsign, 40))
	}

	when, err := time.Parse(time.RFC3339, strings.TrimSpace(c.QSOTime))
	if err != nil {
		return Contact{}, nil, fmt.Errorf("%w: time %q", ErrInvalid, truncate(c.QSOTime, 40))
	}
	out.QSOTime = when.UTC().Format(timeLayout)

	out.Band = fit("band", c.Band, maxBand)
	out.Mode = strings.ToUpper(fit("mode", c.Mode, maxMode))
	out.Operator = strings.ToUpper(fit("operator", c.Operator, maxOperator))
	out.Station = truncate(clean(c.Station), maxStation)
	out.ExchangeClass = strings.ToUpper(fit("exchange_class", c.ExchangeClass, maxClass))
	out.Section = strings.ToUpper(fit("section", c.Section, maxSection))
	out.ContactName = truncate(clean(c.ContactName), maxName)
	out.Notes = truncate(clean(c.Notes), maxNotes)

	switch {
	case c.FrequencyHz == 0:
	case c.FrequencyHz < minFreqHz || c.FrequencyHz > maxFreqHz:
		drop("frequency_hz")
	default:
		out.FrequencyHz = c.FrequencyHz
	}

	switch {
	case c.PowerWatts == 0:
	case c.PowerWatts < 1 || c.PowerWatts > maxWatts:
		drop("power_watts")
	default:
		out.PowerWatts = c.PowerWatts
	}

	out.RSTSent = report("rst_sent", c.RSTSent, drop)
	out.RSTReceived = report("rst_received", c.RSTReceived, drop)

	if grid := clean(c.GridSquare); grid != "" {
		if gridPattern.MatchString(grid) {
			out.GridSquare = formatGrid(grid)
		} else {
			drop("grid_square")
		}
	}

	out.IdempotencyKey = Key(out)
	return out, dropped, nil
}

// Key derives a contact's idempotency key from what makes it that contact: the
// callsign, the second it was logged, the band and the mode. The operator and
// station stay out of it, so two relays watching the same log produce the same
// key. Editing one of the key's fields changes the key, which is what
// Contact.ReplacesKey is for.
func Key(c Contact) string {
	band := strings.ToLower(c.Band)
	// The server lets the frequency decide the band, so the key does too.
	if fromFreq := BandForHz(c.FrequencyHz); fromFreq != "" {
		band = fromFreq
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{keyVersion, c.Callsign, c.QSOTime, band, c.Mode}, "\x00")))
	return base64.RawURLEncoding.EncodeToString(sum[:keyByteCount])
}

func report(field, value string, drop func(string) string) string {
	value = strings.ToUpper(clean(value))
	if value == "" || rstPattern.MatchString(value) {
		return value
	}
	return drop(field)
}

// formatGrid writes a locator the usual way (FN31pr), cut to the six
// characters the API takes.
func formatGrid(grid string) string {
	if len(grid) > 6 {
		grid = grid[:6]
	}
	if len(grid) <= 4 {
		return strings.ToUpper(grid)
	}
	return strings.ToUpper(grid[:4]) + strings.ToLower(grid[4:])
}

// clean makes logger text safe to store: valid UTF-8, no control characters,
// no surrounding space.
func clean(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	return strings.TrimSpace(value)
}

func truncate(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return strings.TrimSpace(string([]rune(value)[:limit]))
}
