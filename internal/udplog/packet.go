// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package udplog

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// wsjtxMagic starts every message of WSJT-X's UDP protocol, which JTDX shares.
	wsjtxMagic = 0xADBCCBDA
	// wsjtxLoggedADIF is the message WSJT-X sends with each logged contact as ADIF.
	// It also sends the same contact as a QSO Logged message, which is left alone so
	// one contact isn't read twice.
	wsjtxLoggedADIF = 12
	wsjtxHeader     = 12
	nullLength      = 0xFFFFFFFF

	// The longest station name, as the server's stations table allows, and how much
	// of a callsign goes in front of the sender's address.
	maxStation = 50
	maxLabel   = 30
)

// defaultClientIDs are the ids WSJT-X and JTDX use unless started with --rig-name,
// so they can't tell two PCs apart.
var defaultClientIDs = []string{"WSJT-X", "JTDX"}

// isWSJTX reports whether data is a message of WSJT-X's protocol.
func isWSJTX(data []byte) bool {
	return len(data) >= 4 && binary.BigEndian.Uint32(data) == wsjtxMagic
}

// wsjtxLogged returns the client id and ADIF text of a Logged ADIF message. A
// message cut short, or of any other type, isn't one.
func wsjtxLogged(data []byte) (id string, adif []byte, ok bool) {
	if len(data) < wsjtxHeader || !isWSJTX(data) || binary.BigEndian.Uint32(data[8:]) != wsjtxLoggedADIF {
		return "", nil, false
	}
	idBytes, rest, ok := readUTF8(data[wsjtxHeader:])
	if !ok {
		return "", nil, false
	}
	adif, _, ok = readUTF8(rest)
	if !ok || len(adif) == 0 {
		return "", nil, false
	}
	return string(idBytes), adif, true
}

// readUTF8 reads a string the way Qt writes one: a big-endian length, where all
// ones means a null string, then that many bytes.
func readUTF8(data []byte) (value, rest []byte, ok bool) {
	if len(data) < 4 {
		return nil, nil, false
	}
	length := binary.BigEndian.Uint32(data)
	data = data[4:]
	if length == nullLength {
		return nil, data, true
	}
	if uint64(length) > uint64(len(data)) {
		return nil, nil, false
	}
	return data[:length], data[length:], true
}

// looksLikeADIF reports whether a datagram could hold an ADIF record.
func looksLikeADIF(data []byte) bool {
	return !isWSJTX(data) && bytes.IndexByte(data, '<') >= 0
}

// adifStation is the station for a plain ADIF contact. Most senders put the club's
// call in STATION_CALLSIGN on every PC, so each PC that sends gets its own station.
// A contact from this computer gets none, so the station picked on the page applies.
func adifStation(fields map[string]string, from netip.Addr) string {
	if isLocal(from) {
		return ""
	}
	label := cleanName(fields["STATION_CALLSIGN"], maxLabel)
	if label == "" {
		label = "ADIF"
	}
	return label + " @ " + from.Unmap().String()
}

// wsjtxStation is the station for a WSJT-X contact. An instance started with
// --rig-name has an id of its own, which names it anywhere. A default id only names
// the program, so other PCs are told apart by address.
func wsjtxStation(id string, from netip.Addr) string {
	id = cleanName(id, maxStation)
	isDefault := false
	for _, name := range defaultClientIDs {
		isDefault = isDefault || strings.EqualFold(id, name)
	}
	switch {
	case id == "" || (isDefault && isLocal(from)):
		return ""
	case isDefault:
		return cleanName(id, maxLabel) + " @ " + from.Unmap().String()
	}
	return id
}

func isLocal(from netip.Addr) bool {
	return !from.IsValid() || from.Unmap().IsLoopback()
}

// cleanName drops control characters and cuts a name to limit characters.
func cleanName(value string, limit int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, value))
	if utf8.RuneCountInString(value) > limit {
		value = strings.TrimSpace(string([]rune(value)[:limit]))
	}
	return value
}
