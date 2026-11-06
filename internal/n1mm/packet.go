// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n1mm

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

// The packets the relay acts on. N1MM also broadcasts radio status, spectrum, spots
// and more, none of which is a contact.
const (
	rootContact = "contactinfo"
	rootReplace = "contactreplace"
	rootDelete  = "contactdelete"

	timeLayout = "2006-01-02 15:04:05"
	// noDate is N1MM's way of writing no date at all.
	noDate = "1900-01-01 00:00:00"
)

var (
	operatorPattern = regexp.MustCompile(`^[A-Z0-9/]{3,20}$`)
	namedBand       = regexp.MustCompile(`^[0-9.]+(m|cm|mm)$`)
	idPattern       = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// packet is one broadcast's values by element name.
type packet struct {
	root   string
	fields map[string]string
}

// rootOf returns a packet's lower-case root element. Some packets N1MM builds by
// hand aren't valid XML, so this reads no further than it has to.
func rootOf(data []byte) string {
	decoder := newDecoder(data)
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		if start, ok := token.(xml.StartElement); ok {
			return strings.ToLower(start.Name.Local)
		}
	}
}

// parsePacket reads what it can. N1MM sizes each datagram by characters and not by
// bytes, so every accented letter cuts a byte off the end. Everything before the
// cut is good, and a value that was cut is left out, since it would be wrong.
func parsePacket(data []byte) packet {
	parsed := packet{fields: make(map[string]string)}
	decoder := newDecoder(data)

	depth := 0
	var name string
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return parsed
		}
		switch element := token.(type) {
		case xml.StartElement:
			depth++
			switch depth {
			case 1:
				parsed.root = strings.ToLower(element.Name.Local)
			case 2:
				name = element.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if depth == 2 {
				text.Write(element)
			}
		case xml.EndElement:
			if depth == 2 {
				parsed.fields[name] = strings.TrimSpace(text.String())
			}
			depth--
		}
	}
}

func newDecoder(data []byte) *xml.Decoder {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))))
	decoder.Strict = false
	decoder.Entity = xml.HTMLEntity
	return decoder
}

// id returns N1MM's id for the contact, or "" if there's no usable one. An edit
// keeps its contact's id, whatever else it changes.
func (p packet) id() string {
	if id := p.fields["ID"]; idPattern.MatchString(id) {
		return strings.ToLower(id)
	}
	return ""
}

// toContact turns a contact packet into a contact that still needs
// contact.Normalize. A contact whose time can't be read is an error wrapping
// contact.ErrInvalid, because a guessed time would be a wrong one.
func (p packet) toContact() (contact.Contact, error) {
	when, err := qsoTime(p.fields["timestamp"])
	if err != nil {
		return contact.Contact{}, err
	}

	band := bandName(p.fields["band"])
	operator := strings.ToUpper(p.fields["operator"])
	if !operatorPattern.MatchString(operator) {
		operator = ""
	}

	return contact.Contact{
		Callsign:      p.fields["call"],
		QSOTime:       when,
		Band:          band,
		Mode:          p.fields["mode"],
		FrequencyHz:   frequencyHz(p.fields["rxfreq"], band),
		Operator:      operator,
		Station:       p.station(),
		ExchangeClass: p.fields["exchange1"],
		Section:       p.fields["section"],
		RSTSent:       p.fields["snt"],
		RSTReceived:   p.fields["rcv"],
		GridSquare:    p.fields["gridsquare"],
		ContactName:   p.fields["name"],
		Notes:         p.fields["comment"],
	}, nil
}

// station is the PC that logged the contact. A PC passing on a contact from
// elsewhere on N1MM's network sends its own name as StationName, with the PC that
// logged it in NetBiosName. N1MM also HTML-encodes StationName.
func (p packet) station() string {
	if origin := p.fields["NetBiosName"]; origin != "" && strings.EqualFold(p.fields["IsOriginal"], "false") {
		return html.UnescapeString(origin)
	}
	return html.UnescapeString(p.fields["StationName"])
}

func qsoTime(value string) (string, error) {
	when, err := time.Parse(timeLayout, value)
	if err != nil || value == noDate {
		return "", fmt.Errorf("%w: time %q", contact.ErrInvalid, cut(value))
	}
	return when.Format(time.RFC3339), nil
}

// bandName returns the server's name for N1MM's band, which is the band's lower
// edge in MHz, written with the PC's decimal separator ("14", "1.8", "3,5").
func bandName(band string) string {
	band = strings.ToLower(strings.ReplaceAll(band, " ", ""))
	if namedBand.MatchString(band) {
		return band
	}
	megahertz, err := strconv.ParseFloat(strings.ReplaceAll(band, ",", "."), 64)
	if err != nil || megahertz <= 0 || megahertz > 1e6 {
		return ""
	}
	return contact.BandForHz(int64(math.Round(megahertz * 1_000_000)))
}

// frequencyHz reads N1MM's frequency, in units of 10 Hz. It returns 0 when it's
// blank, zero or outside the contact's band, since the server lets the frequency
// decide the band.
func frequencyHz(value, band string) int64 {
	tens, err := strconv.ParseInt(value, 10, 64)
	if err != nil || tens <= 0 || tens > 1e11 {
		return 0
	}
	hz := tens * 10
	found := contact.BandForHz(hz)
	if found == "" || (band != "" && !strings.EqualFold(found, band)) {
		return 0
	}
	return hz
}

func cut(value string) string {
	if len(value) > 40 {
		return value[:40]
	}
	return value
}
