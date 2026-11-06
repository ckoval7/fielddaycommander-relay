// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package adif

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

var (
	datePattern     = regexp.MustCompile(`^\d{8}$`)
	timePattern     = regexp.MustCompile(`^\d{4}(\d{2})?$`)
	operatorPattern = regexp.MustCompile(`^[A-Z0-9/]{3,20}$`)
	classPattern    = regexp.MustCompile(`(?i)^[0-9]{1,2}[A-FHIMO]$`)
)

// ToContact turns a record's fields into a contact that still needs
// contact.Normalize, and returns its time so the caller can check it. A record
// whose time can't be read is an error wrapping contact.ErrInvalid.
//
// STATION_CALLSIGN stays out: it's usually the club's call on every PC, which
// would merge them into one station. The server uses the relay's name instead.
func ToContact(fields map[string]string) (contact.Contact, time.Time, error) {
	when, err := qsoTime(fields["QSO_DATE"], fields["TIME_ON"])
	if err != nil {
		return contact.Contact{}, time.Time{}, err
	}

	band := strings.ToLower(fields["BAND"])
	mode := fields["SUBMODE"]
	if mode == "" {
		mode = fields["MODE"]
	}
	operator := strings.ToUpper(fields["OPERATOR"])
	if !operatorPattern.MatchString(operator) {
		operator = ""
	}
	notes := fields["COMMENT"]
	if notes == "" {
		notes = fields["NOTES"]
	}
	class, section := exchange(fields)

	return contact.Contact{
		Callsign:      fields["CALL"],
		QSOTime:       when.Format(time.RFC3339),
		Band:          band,
		Mode:          mode,
		FrequencyHz:   frequencyHz(fields["FREQ"], band),
		Operator:      operator,
		ExchangeClass: class,
		Section:       section,
		PowerWatts:    powerWatts(fields["TX_PWR"]),
		RSTSent:       fields["RST_SENT"],
		RSTReceived:   fields["RST_RCVD"],
		GridSquare:    fields["GRIDSQUARE"],
		ContactName:   fields["NAME"],
		Notes:         notes,
	}, when, nil
}

func qsoTime(date, clock string) (time.Time, error) {
	invalid := fmt.Errorf("%w: date %q time %q", contact.ErrInvalid, cut(date), cut(clock))
	if !datePattern.MatchString(date) || !timePattern.MatchString(clock) {
		return time.Time{}, invalid
	}
	if len(clock) == 4 {
		clock += "00"
	}
	when, err := time.Parse("20060102150405", date+clock)
	if err != nil {
		return time.Time{}, invalid
	}
	return when, nil
}

// exchange returns the received class and section. WSJT-X and others write only
// SRX_STRING ("2H EMA"), with no CLASS or ARRL_SECT.
func exchange(fields map[string]string) (class, section string) {
	class, section = fields["CLASS"], fields["ARRL_SECT"]
	if class != "" && section != "" {
		return class, section
	}
	tokens := strings.Fields(fields["SRX_STRING"])
	if len(tokens) != 2 || !classPattern.MatchString(tokens[0]) {
		return class, section
	}
	if class == "" {
		class = tokens[0]
	}
	if section == "" {
		section = tokens[1]
	}
	return class, section
}

// frequencyHz returns a frequency given in MHz, or 0 when it's blank, unreadable
// or not in the record's band. The server lets the frequency decide the band, so
// a misread one must not reach it.
func frequencyHz(megahertz, band string) int64 {
	value, err := strconv.ParseFloat(strings.ReplaceAll(megahertz, ",", "."), 64)
	if err != nil || value <= 0 || value > 1e6 {
		return 0
	}
	hz := int64(math.Round(value * 1_000_000))
	found := contact.BandForHz(hz)
	if found == "" || (band != "" && !strings.EqualFold(found, band)) {
		return 0
	}
	return hz
}

// powerWatts rounds to the whole watts the API takes, keeping QRP at 1 and not 0.
func powerWatts(value string) int {
	watts, err := strconv.ParseFloat(value, 64)
	if err != nil || watts <= 0 || watts > 1e6 {
		return 0
	}
	return max(int(math.Round(watts)), 1)
}

func cut(value string) string {
	if len(value) > 40 {
		return value[:40]
	}
	return value
}
