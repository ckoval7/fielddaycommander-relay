// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package n3fjp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

// senderName is what the relay calls itself in the FROM of messages it sends.
const senderName = "FDRELAY"

var (
	// ErrNotReady means there's no connection with a loaded log to write to.
	ErrNotReady = errors.New("n3fjp: the log isn't ready")
	// errOutOfStep means the record table needs reloading before a change is safe.
	errOutOfStep = errors.New("n3fjp: the record table is out of step")
)

type writeKind int

const (
	writeAdd writeKind = iota
	writeEdit
	writeDelete
)

// writeRequest crosses to the goroutine that owns the connection's record table.
type writeRequest struct {
	kind   writeKind
	key    string
	before contact.Contact
	after  contact.Contact
	done   chan error
}

// Ready reports whether the log is loaded, so writes can be matched to records.
func (s *Source) Ready() bool {
	return s.ready.Load()
}

// Add puts a contact in the log, unless a record already has its key.
func (s *Source) Add(ctx context.Context, c contact.Contact) error {
	return s.write(ctx, writeRequest{kind: writeAdd, after: c})
}

// Edit applies the fields that differ between before and after to the record with
// this key. A contact the log doesn't have is added.
func (s *Source) Edit(ctx context.Context, key string, before, after contact.Contact) error {
	return s.write(ctx, writeRequest{kind: writeEdit, key: key, before: before, after: after})
}

// Delete removes every record with this key.
func (s *Source) Delete(ctx context.Context, key string) error {
	return s.write(ctx, writeRequest{kind: writeDelete, key: key})
}

func (s *Source) write(ctx context.Context, request writeRequest) error {
	if !s.Ready() {
		return ErrNotReady
	}
	request.done = make(chan error, 1)

	timeout := time.NewTimer(writeRequestTimeout)
	defer timeout.Stop()
	select {
	case s.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-timeout.C:
		return ErrNotReady
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// applyWrite runs on the session's goroutine. The record table only changes when
// the server relays the write back, the same as for every other client.
func (s *session) applyWrite(ctx context.Context, request writeRequest) error {
	if s.loading != nil {
		return errOutOfStep
	}
	switch request.kind {
	case writeAdd:
		return s.writeAdd(ctx, request.after)
	case writeEdit:
		return s.writeEdit(ctx, request)
	default:
		return s.writeDelete(ctx, request.key)
	}
}

func (s *session) writeAdd(ctx context.Context, c contact.Contact) error {
	if s.numberOf(c.IdempotencyKey) != 0 {
		return nil
	}
	fields := fieldsFor(c)
	fields["PRIMARYKEY"] = strconv.Itoa(s.counter + 1)

	if err := s.remember(ctx, fields); err != nil {
		return err
	}
	return s.send(transactionMessage("ADD", recordText(fields)))
}

func (s *session) writeEdit(ctx context.Context, request writeRequest) error {
	number := s.numberOf(request.key)
	if number == 0 {
		return s.writeAdd(ctx, request.after)
	}

	fields, changed := mergedFields(s.records[number].fields, request.before, request.after)
	if !changed {
		return nil
	}
	fields["PRIMARYKEY"] = strconv.Itoa(number)

	if err := s.remember(ctx, fields); err != nil {
		return err
	}
	return s.send(transactionMessage("EDIT", recordText(fields)))
}

func (s *session) writeDelete(ctx context.Context, key string) error {
	numbers := s.numbersOf(key)
	for _, number := range numbers {
		if err := s.source.sink.Remember(ctx, deleteOf(s.records[number])); err != nil {
			return err
		}
		if err := s.send(transactionMessage("DELETE", strconv.Itoa(number))); err != nil {
			return err
		}
	}
	return nil
}

// remember records what the server will relay back, as the source will read it,
// so that seeing it doesn't send the contact to Field Day Commander again.
func (s *session) remember(ctx context.Context, fields map[string]string) error {
	c, err := toContact(parseRecord(recordText(fields)), senderName)
	if err != nil {
		return fmt.Errorf("the contact can't be written: %w", err)
	}
	return s.source.sink.Remember(ctx, c)
}

func (s *session) numberOf(key string) int {
	if numbers := s.numbersOf(key); len(numbers) > 0 {
		return numbers[0]
	}
	return 0
}

func (s *session) numbersOf(key string) []int {
	var numbers []int
	for number, candidate := range s.records {
		if key != "" && candidate.key == key {
			numbers = append(numbers, number)
		}
	}
	slices.Sort(numbers)
	return numbers
}

// mergedFields applies what changed between before and after to a record, and
// leaves everything else as the operator entered it.
func mergedFields(current map[string]string, before, after contact.Contact) (map[string]string, bool) {
	from, to := fieldsFor(before), fieldsFor(after)
	fields := maps.Clone(current)
	changed := false
	for _, name := range changedFields(from, to) {
		if fields[name] == to[name] {
			continue
		}
		changed = true
		if to[name] == "" {
			delete(fields, name)
		} else {
			fields[name] = to[name]
		}
	}
	return fields, changed
}

func changedFields(before, after map[string]string) []string {
	names := make(map[string]struct{})
	for name := range before {
		names[name] = struct{}{}
	}
	for name := range after {
		names[name] = struct{}{}
	}
	var changed []string
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if before[name] != after[name] {
			changed = append(changed, name)
		}
	}
	return changed
}

// fieldsFor is a contact as N3FJP record fields. Empty fields are left out when
// the record is written, as N3FJP does.
func fieldsFor(c contact.Contact) map[string]string {
	fields := map[string]string{
		"CALL":        c.Callsign,
		"BAND":        n3fjpBand(c.Band),
		"MODE":        c.Mode,
		"MODECONTEST": modeGroup(c.Mode),
		"CLASS":       c.ExchangeClass,
		"SECTION":     c.Section,
		"OPERATOR":    c.Operator,
		"STATION":     c.Station,
		"RSTS":        c.RSTSent,
		"RSTR":        c.RSTReceived,
		"GRIDR":       c.GridSquare,
		"NAMER":       c.ContactName,
		"COMMENTS":    c.Notes,
	}
	if when, err := time.Parse(time.RFC3339, c.QSOTime); err == nil {
		fields["DATESTR"] = when.Format("2006/01/02")
		fields["TIMEONSTR"] = when.Format("15:04:05")
		fields["TIMEOFFSTR"] = when.Format("15:04:05")
	}
	if c.FrequencyHz != 0 {
		fields["FREQUENCY"] = strconv.FormatFloat(float64(c.FrequencyHz)/1_000_000, 'f', 5, 64)
	}
	if c.PowerWatts != 0 {
		fields["POWER"] = strconv.Itoa(c.PowerWatts)
	}
	return fields
}

// n3fjpBand is the reverse of serverBand.
func n3fjpBand(band string) string {
	for n3fjp, name := range bandNames {
		if strings.EqualFold(name, band) {
			if n3fjp == "6CM" {
				return "6cm"
			}
			return n3fjp
		}
	}
	if meters, found := strings.CutSuffix(strings.ToLower(band), "m"); found && !strings.HasSuffix(meters, "c") && !strings.HasSuffix(meters, "m") {
		return meters
	}
	return band
}

// modeGroup files a mode under the contest's CW, phone or digital, using the same
// rule as the server.
func modeGroup(mode string) string {
	mode = strings.ToUpper(mode)
	switch mode {
	case "":
		return ""
	case "CW", "CW-R", "CWR", "CW-U", "CW-L":
		return "CW"
	case "PH", "SSB", "USB", "LSB", "FM", "AM", "PM", "NFM", "WFM", "DIGITALVOICE", "FREEDV", "DSTAR", "DMR", "C4FM", "M17":
		return "PH"
	}
	return "DIG"
}

// recordText writes fields the way N3FJP does: alphabetical, with no escaping and
// empty fields left out. N3FJP finds a field by its first closing tag and a message
// by its first <EOR>, so angle brackets and control characters in a value would
// break the record for every PC. They're replaced.
func recordText(fields map[string]string) string {
	var out strings.Builder
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if value := cleanValue(fields[name]); value != "" {
			fmt.Fprintf(&out, "<FLD%s>%s</FLD%s>", name, value, name)
		}
	}
	return out.String()
}

func cleanValue(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value))
}

func transactionMessage(kind, data string) string {
	return "<BOR><NTWK><FROM>" + senderName + "</FROM><TRANSACTION>" + kind + "</TRANSACTION><XMLDATA>" + data + "</XMLDATA><EOR>"
}

// Chat replies an operator sees when the relay is the N3FJP server. Each takes
// the contact's call.
const (
	rejectAddText   = "Field Day Commander Relay: %s was not saved. Couldn't read the call, date, or time. It's back in the entry boxes. Fix it and enter it again."
	rejectEditText  = "Field Day Commander Relay: your edit to %s was not saved. Your copy goes back to what it was."
	rejectStoreText = "Field Day Commander Relay: %s was not saved. The relay couldn't save to disk. Check the relay's computer."
)
