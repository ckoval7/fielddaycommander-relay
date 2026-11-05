// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

// bandNames are N3FJP band names that aren't a number of meters or already the
// server's name.
var bandNames = map[string]string{
	"70":  "70cm",
	"33":  "33cm",
	"23":  "23cm",
	"13":  "13cm",
	"9":   "9cm",
	"6CM": "5cm",
	"3":   "3cm",
	"SAT": "Satellite",
}

var (
	// .NET writes the PC's own separators in place of "/" and ":", so a German
	// PC sends 2026.06.27 and some locales 18.42.07. A contact typed in
	// post-contest mode can carry a bare 1842.
	datePattern = regexp.MustCompile(`^(\d{4})\D(\d{1,2})\D(\d{1,2})$`)
	timePattern = regexp.MustCompile(`^(\d{1,2})\D?(\d{2})(?:\D?(\d{2}))?$`)

	frequencyPattern = regexp.MustCompile(`^[\d.,]+$`)
	operatorPattern  = regexp.MustCompile(`^[A-Z0-9/]{3,20}$`)
)

// toContact turns a record's fields into a normalized contact. A record whose
// time can't be read is an error. N3FJP itself falls back to the current time,
// but a guessed time would put the contact in the wrong place and give it a
// different idempotency key on every read.
func toContact(fields map[string]string, sentFrom string) (contact.Contact, error) {
	when, err := qsoTime(fields["DATESTR"], fields["TIMEONSTR"])
	if err != nil {
		return contact.Contact{}, err
	}

	band := serverBand(fields["BAND"])
	mode := strings.TrimSpace(fields["MODE"])
	if mode == "" {
		mode = strings.TrimSpace(fields["MODECONTEST"])
	}
	operator := strings.ToUpper(fields["OPERATOR"])
	if !operatorPattern.MatchString(operator) {
		operator = ""
	}

	normalized, _, err := contact.Normalize(contact.Contact{
		Callsign:      fields["CALL"],
		QSOTime:       when,
		Band:          band,
		Mode:          mode,
		FrequencyHz:   frequencyHz(fields["FREQUENCY"], band),
		Operator:      operator,
		Station:       firstNonEmpty(fields["STATION"], fields["COMPUTERNAME"], sentFrom, "N3FJP"),
		ExchangeClass: fields["CLASS"],
		Section:       fields["SECTION"],
		PowerWatts:    powerWatts(fields["POWER"]),
		RSTSent:       fields["RSTS"],
		RSTReceived:   fields["RSTR"],
		GridSquare:    fields["GRIDR"],
		ContactName:   fields["NAMER"],
		Notes:         fields["COMMENTS"],
	})
	return normalized, err
}

func qsoTime(date, clock string) (string, error) {
	d := datePattern.FindStringSubmatch(strings.TrimSpace(date))
	t := timePattern.FindStringSubmatch(strings.TrimSpace(clock))
	if d == nil || t == nil {
		return "", fmt.Errorf("%w: date %q time %q", contact.ErrInvalid, date, clock)
	}

	seconds := t[3]
	if seconds == "" {
		seconds = "00"
	}
	layout := fmt.Sprintf("%s-%02s-%02sT%02s:%s:%sZ", d[1], d[2], d[3], t[1], t[2], seconds)
	parsed, err := time.Parse(time.RFC3339, layout)
	if err != nil {
		return "", fmt.Errorf("%w: date %q time %q", contact.ErrInvalid, date, clock)
	}
	return parsed.Format(time.RFC3339), nil
}

// serverBand returns the server's name for an N3FJP band, or "" when blank.
func serverBand(band string) string {
	band = strings.ToUpper(strings.TrimSpace(band))
	if band == "" {
		return ""
	}
	if name, ok := bandNames[band]; ok {
		return name
	}
	if _, err := strconv.ParseFloat(band, 64); err == nil {
		return band + "m"
	}
	return strings.ToLower(band)
}

// frequencyHz returns an N3FJP frequency in Hz, or 0 when it's blank,
// unreadable or not in the record's band. The server lets the frequency decide
// the band, so a misread one must not reach it.
//
// N3FJP writes MHz with the Windows locale's separators ("14.07400",
// "1,296.10000", "14,07400"), so whichever of "." and "," comes last is the
// decimal point. A lone "," is a decimal point unless reading it as thousands
// is what puts the frequency in the record's band.
func frequencyHz(frequency, band string) int64 {
	for _, megahertz := range frequencyReadings(frequency) {
		hz := int64(math.Round(megahertz * 1_000_000))
		found := contact.BandForHz(hz)
		if found != "" && (band == "" || strings.EqualFold(found, band)) {
			return hz
		}
	}
	return 0
}

func frequencyReadings(frequency string) []float64 {
	frequency = strings.NewReplacer(" ", "", " ", "", " ", "", "'", "", "\t", "").Replace(frequency)
	if !frequencyPattern.MatchString(frequency) {
		return nil
	}

	lastDot := strings.LastIndexByte(frequency, '.')
	lastComma := strings.LastIndexByte(frequency, ',')

	var candidates []string
	switch {
	case lastDot >= 0 && lastComma >= 0 && lastDot > lastComma:
		candidates = []string{strings.ReplaceAll(frequency, ",", "")}
	case lastDot >= 0 && lastComma >= 0:
		candidates = []string{strings.ReplaceAll(strings.ReplaceAll(frequency, ".", ""), ",", ".")}
	case lastComma >= 0:
		candidates = []string{strings.ReplaceAll(frequency, ",", "."), strings.ReplaceAll(frequency, ",", "")}
	default:
		candidates = []string{frequency}
	}

	var readings []float64
	for _, candidate := range candidates {
		if value, err := strconv.ParseFloat(candidate, 64); err == nil {
			readings = append(readings, value)
		}
	}
	return readings
}

// powerWatts returns whole watts, rounded up so a power limit is never
// under-reported, or 0 when the field isn't a positive number.
func powerWatts(power string) int {
	power = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(power), "wW"))
	value, err := strconv.ParseFloat(strings.ReplaceAll(power, ",", "."), 64)
	if err != nil || value <= 0 || value > math.MaxInt32 {
		return 0
	}
	return int(math.Ceil(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
