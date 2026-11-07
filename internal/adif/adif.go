// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package adif reads ADIF, the text format nearly every amateur radio logger can
// write. Records are read by each value's declared length, so "<EOR>" or a
// tag-like string inside a value stays text, and a wrong length never runs into
// the next field.
package adif

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// maxDeclared is the longest declared length worth following. Anything longer is
// read up to the next tag, which keeps a hostile file from making each tag walk
// the whole buffer.
const maxDeclared = 4096

// Record is one complete record.
type Record struct {
	// Fields holds the values by upper-case tag name.
	Fields map[string]string
	// Raw is the record's text as it appeared.
	Raw string
}

// Scan returns the complete records in data and how many bytes they took up, so
// the caller can start the next read there. Text after the last <EOR> or <EOH>
// isn't consumed, because a logger may still be writing it. sawTag reports
// whether data held anything that looks like ADIF.
func Scan(data []byte) (records []Record, consumed int, sawTag bool) {
	position, start := 0, 0
	var fields map[string]string

	for {
		next := bytes.IndexByte(data[position:], '<')
		if next < 0 {
			return records, consumed, sawTag
		}
		open := position + next

		parsed, ok := parseTag(data, open)
		if !ok {
			position = open + 1
			continue
		}

		if parsed.hasLength {
			sawTag = true
			var value []byte
			value, position = readValue(data, parsed.end, parsed.length)
			if fields == nil {
				fields = make(map[string]string)
			}
			fields[parsed.name] = strings.TrimSpace(toUTF8(value))
			continue
		}

		position = parsed.end
		switch parsed.name {
		case "EOH":
			// Everything so far was the header.
			sawTag = true
		case "EOR":
			sawTag = true
			if len(fields) > 0 {
				records = append(records, Record{Fields: fields, Raw: string(bytes.TrimSpace(data[start:position]))})
			}
		default:
			continue
		}
		fields = nil
		start, consumed = position, position
	}
}

type tag struct {
	name      string
	hasLength bool
	length    int
	end       int
}

// parseTag reads <NAME>, <NAME:LEN> or <NAME:LEN:T> at offset, which holds "<".
func parseTag(data []byte, offset int) (tag, bool) {
	at := offset + 1
	nameStart := at
	for at < len(data) && isNameByte(data[at]) {
		at++
	}
	if at == nameStart || at >= len(data) {
		return tag{}, false
	}
	parsed := tag{name: strings.ToUpper(string(data[nameStart:at]))}

	if data[at] == ':' {
		var ok bool
		if at, ok = parseLength(data, at+1, &parsed); !ok {
			return tag{}, false
		}
	}

	if data[at] != '>' {
		return tag{}, false
	}
	parsed.end = at + 1
	return parsed, true
}

// parseLength reads the LEN or LEN:T of a tag at offset, and returns the offset
// just after it, which is inside the data.
func parseLength(data []byte, offset int, parsed *tag) (int, bool) {
	at := offset
	for at < len(data) && data[at] >= '0' && data[at] <= '9' {
		// Stop counting once it's past any length that gets followed.
		if parsed.length <= maxDeclared {
			parsed.length = parsed.length*10 + int(data[at]-'0')
		}
		at++
	}
	if at == offset || at >= len(data) {
		return 0, false
	}
	parsed.hasLength = true

	if data[at] == ':' {
		at++
		if at >= len(data) || !isLetter(data[at]) {
			return 0, false
		}
		at++
		if at >= len(data) {
			return 0, false
		}
	}
	return at, true
}

// readValue returns the value starting at start and the offset just after it.
// Loggers disagree on whether lengths count bytes or characters, so the reading
// that ends where a tag or the end of the data begins wins. When neither does,
// the value stops at the next tag.
func readValue(data []byte, start, declared int) ([]byte, int) {
	if declared <= maxDeclared {
		if end := start + declared; end <= len(data) && endsAtBoundary(data, end) {
			return data[start:end], end
		}
		if end, ok := charactersEnd(data, start, declared); ok && endsAtBoundary(data, end) {
			return data[start:end], end
		}
	}

	stop := nextFieldTag(data, start)
	segment := data[start:stop]
	if end, ok := charactersEnd(segment, 0, declared); ok && declared <= maxDeclared {
		return segment[:end], stop
	}
	if declared < len(segment) {
		return segment[:declared], stop
	}
	return segment, stop
}

// charactersEnd is the offset just after count UTF-8 characters from start.
func charactersEnd(data []byte, start, count int) (int, bool) {
	at := start
	for range count {
		if at >= len(data) {
			return 0, false
		}
		r, size := utf8.DecodeRune(data[at:])
		if r == utf8.RuneError && size <= 1 {
			return 0, false
		}
		at += size
	}
	return at, true
}

// endsAtBoundary reports whether only space lies between offset and a tag or the
// end of the data.
func endsAtBoundary(data []byte, offset int) bool {
	for offset < len(data) && isSpace(data[offset]) {
		offset++
	}
	if offset >= len(data) {
		return true
	}
	if data[offset] != '<' {
		return false
	}
	_, ok := parseTag(data, offset)
	return ok
}

// nextFieldTag finds the next tag that holds a value or ends a record or header,
// or the end of the data.
func nextFieldTag(data []byte, from int) int {
	for position := from; ; {
		next := bytes.IndexByte(data[position:], '<')
		if next < 0 {
			return len(data)
		}
		open := position + next
		if parsed, ok := parseTag(data, open); ok && (parsed.hasLength || parsed.name == "EOR" || parsed.name == "EOH") {
			return open
		}
		position = open + 1
	}
}

func isNameByte(b byte) bool {
	return isLetter(b) || b == '_' || (b >= '0' && b <= '9')
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// windows1252High holds the characters of Windows-1252 from 0x80 to 0x9F, where
// it differs from Latin-1. Undefined bytes become the replacement character.
var windows1252High = [32]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
}

// toUTF8 reads text that isn't UTF-8 as Windows-1252, the code page Windows
// loggers use in the US and Western Europe.
func toUTF8(value []byte) string {
	if utf8.Valid(value) {
		return string(value)
	}
	var out strings.Builder
	out.Grow(len(value) + len(value)/4)
	for _, b := range value {
		switch {
		case b < 0x80:
			out.WriteByte(b)
		case b < 0xA0:
			out.WriteRune(windows1252High[b-0x80])
		default:
			out.WriteRune(rune(b))
		}
	}
	return out.String()
}
