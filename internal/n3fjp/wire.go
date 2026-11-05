// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package n3fjp reads contacts from an N3FJP network server (TCP port 1000) as
// a listening client.
//
// Wire format: UTF-16LE text with no BOM, each message ending in the characters
// ETX, EOT and BEL. Messages are <BOR>...<EOR> runs of XML-like tags that N3FJP
// neither escapes nor nests consistently, so a field is the text between the
// first opening tag and the first closing tag, ignoring case.
package n3fjp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"unicode/utf16"
)

// maxFrameBytes is the longest unterminated message kept. A full record is well
// under 4 KB, so anything this long isn't N3FJP traffic.
const maxFrameBytes = 256 << 10

var terminator = []byte{3, 0, 4, 0, 7, 0}

// errFrameTooLong means the peer sent too much without a terminator.
var errFrameTooLong = errors.New("n3fjp: message too long")

// decoder splits a connection's bytes into messages. It keeps raw bytes until a
// whole terminator arrives on a character boundary, so a read that splits a
// character or the terminator can't garble a message.
type decoder struct {
	buffer []byte
}

// feed adds bytes and returns every message they complete.
func (d *decoder) feed(data []byte) ([]string, error) {
	d.buffer = append(d.buffer, data...)
	var messages []string

	for {
		position := findTerminator(d.buffer)
		if position < 0 {
			break
		}
		if message := decodeUTF16(d.buffer[:position]); message != "" {
			messages = append(messages, message)
		}
		d.buffer = d.buffer[position+len(terminator):]
	}

	if len(d.buffer) > maxFrameBytes {
		d.buffer = nil
		return messages, errFrameTooLong
	}
	return messages, nil
}

func findTerminator(buffer []byte) int {
	offset := 0
	for {
		found := bytes.Index(buffer[offset:], terminator)
		if found < 0 {
			return -1
		}
		if position := offset + found; position%2 == 0 {
			return position
		}
		offset += found + 1
	}
}

func decodeUTF16(frame []byte) string {
	units := make([]uint16, len(frame)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(frame[i*2:])
	}
	return strings.ReplaceAll(string(utf16.Decode(units)), "\x00", "")
}

// encode returns one message as it goes on the wire.
func encode(message string) []byte {
	units := utf16.Encode([]rune(message + "\x03\x04\x07"))
	out := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(out[i*2:], unit)
	}
	return out
}

// upperASCII upper-cases ASCII letters only, so byte offsets in the result are
// also offsets in the original. strings.ToUpper can change a string's length.
func upperASCII(value string) string {
	out := []byte(value)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - ('a' - 'A')
		}
	}
	return string(out)
}

// field returns the text between the first <TAG> and the first </TAG>.
func field(message, tag string) string {
	upper := upperASCII(message)
	open := "<" + tag + ">"
	start := strings.Index(upper, open)
	end := strings.Index(upper, "</"+tag+">")
	if start < 0 || end < start+len(open) {
		return ""
	}
	return message[start+len(open) : end]
}

// transaction returns a network message's TRANSACTION, upper-cased, or "" for
// anything else. A chat says what it is right after <BOR>, and its text may hold
// anything, so a chat never counts as a transaction, whatever it contains.
func transaction(message string) string {
	upper := upperASCII(message)
	if strings.HasPrefix(strings.TrimSpace(upper), "<BOR><MESG>") || !strings.Contains(upper, "<NTWK>") {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(field(message, "TRANSACTION")))
}

// parseRecord returns a record's <FLDNAME>value</FLDNAME> fields, keyed by
// upper-case name without the FLD prefix. The first of a repeated field wins.
func parseRecord(record string) map[string]string {
	fields := make(map[string]string)
	upper := upperASCII(record)
	position := 0

	for {
		start := strings.Index(upper[position:], "<FLD")
		if start < 0 {
			return fields
		}
		start += position
		nameEnd := strings.IndexByte(upper[start:], '>')
		if nameEnd < 0 {
			return fields
		}
		name := upper[start+len("<FLD") : start+nameEnd]
		valueStart := start + nameEnd + 1

		closing := strings.Index(upper[valueStart:], "</FLD"+name+">")
		if name == "" || !isFieldName(name) || closing < 0 {
			position = valueStart
			continue
		}
		if _, seen := fields[name]; !seen {
			fields[name] = strings.TrimSpace(record[valueStart : valueStart+closing])
		}
		position = valueStart + closing + len("</FLD") + len(name) + 1
	}
}

func isFieldName(name string) bool {
	for _, b := range []byte(name) {
		if (b < 'A' || b > 'Z') && (b < '0' || b > '9') {
			return false
		}
	}
	return true
}
