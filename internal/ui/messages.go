// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"github.com/ckoval7/fielddaycommander-relay/internal/app"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
	"github.com/ckoval7/fielddaycommander-relay/internal/sender"
)

// TrayText is the wording of the tray icon.
var TrayText = struct {
	Tooltip string
	Open    string
	Quit    string
}{
	Tooltip: "Field Day Commander Relay",
	Open:    "Open status page",
	Quit:    "Quit relay",
}

var senderStates = map[sender.State]string{
	sender.Starting:        "Starting the connection. Contacts waiting to send stay on this computer.",
	sender.Online:          "Connected to Field Day Commander.",
	sender.Offline:         "Can't reach the server. Check this computer's connection. Saved contacts will be sent when the server is reachable again.",
	sender.RateLimited:     "The server asked the relay to wait. Contacts remain saved here. The relay will try again automatically.",
	sender.NeedsEnrollment: "This relay was revoked or its event was deleted. Click Disconnect, then connect with a new enrollment code from the server's Relays page. Queued contacts stay on this computer.",
	sender.HTTPSRequired:   "The connection needs https. Click Disconnect, then connect with the server's https address and a new code. Queued contacts are kept.",
	sender.PinMismatch:     "The server's key changed, so sending has stopped. Contacts remain saved here. Ask whoever runs the server to confirm the new key before you disconnect and connect again with a new code.",
	sender.Untrusted:       "The relay couldn't verify the server's certificate and has stopped sending. Queued contacts are kept. Ask whoever runs the server to check its certificate before you connect again.",
	sender.ServerTooOld:    "The server doesn't support this relay version. Ask whoever runs it to update Field Day Commander. Contacts will wait on this computer.",
	sender.RelayTooOld:     "The server needs a newer relay version. Update Field Day Commander Relay. Saved contacts are kept through the restart.",
	sender.ServerRefused:   "The server refused the request. Check the server address shown here with whoever runs it. Contacts remain in the queue. See relay.log in the relay's data folder for details.",
}

func senderStateText(state sender.State) string {
	if text, ok := senderStates[state]; ok {
		return text
	}
	return senderStates[sender.Starting]
}

var enrollErrors = map[app.EnrollFailure]string{
	app.FailBadAddress:    "That server address isn't usable. Enter an https address, like https://logs.example.org.",
	app.FailBadCode:       "That code didn't work. Create a new enrollment code on the server's Relays page. Codes work once and expire after 15 minutes.",
	app.FailUnreachable:   "Couldn't reach that server. Check the address and this computer's connection, then try Connect again. Collected contacts stay on this computer.",
	app.FailRateLimited:   "There have been too many connection attempts. Wait a minute, then try Connect again.",
	app.FailHTTPSRequired: "The connection needs https. Enter the server's https address and try Connect again.",
	app.FailRefused:       "The server refused the connection request. Check the address with whoever runs the server, then try Connect again.",
	app.FailKeychain:      "The relay couldn't save its connection token in Windows Credential Manager. Check relay.log in the relay's data folder with whoever looks after this PC. You may need a new enrollment code when you try again. Collected contacts are kept.",
	app.FailPinMismatch:   "The server's key changed before the connection was finished. The relay didn't send your code. Ask whoever runs the server to confirm its key before you try again.",
	app.FailUntrusted:     "The relay couldn't verify this server's certificate. If your club uses a self-signed certificate, compare the key below with whoever runs the server. Accept it only if the keys match.",
	app.FailSave:          "The server accepted the code, but the relay couldn't save its settings. Check relay.log in the relay's data folder with whoever looks after this PC, then connect with a new code. Collected contacts are still saved here.",
}

func enrollErrorText(failure app.EnrollFailure) string {
	if text, ok := enrollErrors[failure]; ok {
		return text
	}
	return enrollErrors[app.FailRefused]
}

var attentionCodes = map[string]string{
	"out_of_period":   "This contact is outside the event's operating period. Check its date and time in N3FJP and the logging PC's clock.",
	"invalid":         "The server couldn't accept this contact's fields. Check the contact in N3FJP and ask whoever runs the server to review the error.",
	"invalid_contact": "The server couldn't store this contact. Check it in N3FJP with whoever runs the server.",
	"unknown_station": "This contact is waiting in the queue because its station isn't on the server. Ask someone with manage-stations permission to create a station matching the name or hostname shown here.",
}

// attentionText prefers the relay's own words, and falls back to the server's.
func attentionText(item queue.Attention) string {
	if text, ok := attentionCodes[item.Code]; ok {
		return text
	}
	if item.Message != "" {
		return item.Message
	}
	return item.Code
}

// pageText is the wording of the page that isn't a status or an error.
type pageText struct {
	Title, ServerHeading, LoggerHeading, QueueHeading, AttentionHeading string
	NotEnrolled, EnrollHelp, ServerLabel, CodeLabel, EnrollButton       string
	KeyLabel, AcceptButton                                              string
	RelayNameLabel, EventLabel, ServerAddressLabel, LastSentLabel       string
	SelfSigned, ForgetButton, ForgetHelp                                string
	LoggerConnecting, LoggerWaiting, LoggerConnected, LoggerHelp        string
	LoggerAddressLabel, LoggerButton, LoggerError                       string
	Waiting, Sent, Rejected, Unreadable, ActionError                    string
	ClockWarning, AttentionCall, AttentionTime, AttentionStation        string
	AttentionWhy, Version                                               string
}

var pageWords = pageText{
	Title:              "Field Day Commander Relay",
	ServerHeading:      "Server",
	LoggerHeading:      "N3FJP",
	QueueHeading:       "Contacts",
	AttentionHeading:   "Contacts to check",
	NotEnrolled:        "Connect to your Field Day Commander server to send contacts collected from N3FJP on the logging computer. Collection starts before you connect. Contacts are saved on this computer first and kept through outages and restarts.",
	EnrollHelp:         "On the server, open the Relays page and create an enrollment code. Enter the server's https address and the code below. Codes work once and expire after 15 minutes. Each relay is tied to one event. For a new event, disconnect and connect with a new code.",
	ServerLabel:        "Server address",
	CodeLabel:          "Enrollment code",
	EnrollButton:       "Connect",
	KeyLabel:           "Server's key",
	AcceptButton:       "The keys match, connect",
	RelayNameLabel:     "This relay",
	EventLabel:         "Event",
	ServerAddressLabel: "Server",
	LastSentLabel:      "Last sent",
	SelfSigned:         "Using the server key you accepted.",
	ForgetButton:       "Disconnect",
	ForgetHelp:         "Disconnect removes this relay's token from this computer and keeps queued contacts. It doesn't revoke the relay on the server. To do that, open the server's Relays page and choose Revoke.",
	LoggerConnecting:   "Can't connect to N3FJP yet. Check that it's running, turn on TCP networking in Settings > Network, and check the N3FJP address below. Contacts already collected are saved here.",
	LoggerWaiting:      "Connected to N3FJP, but it hasn't sent the log yet. Check Settings > Network in N3FJP and select TCP networking. File share mode won't work with the relay.",
	LoggerConnected:    "Connected to N3FJP and receiving log data.",
	LoggerHelp:         "Enter the address of the PC running N3FJP's network server. The default is this computer, 127.0.0.1:1000. N3FJP must use TCP networking. File share mode won't work.",
	LoggerAddressLabel: "N3FJP address",
	LoggerButton:       "Save",
	LoggerError:        "That N3FJP address isn't usable. Enter a computer name or address, with an optional port, like 192.168.1.5 or 192.168.1.5:1000.",
	Waiting:            "Waiting to send",
	Sent:               "Sent",
	Rejected:           "Rejected by the server",
	Unreadable:         "N3FJP records the relay couldn't read",
	ActionError:        "The relay couldn't save that change. Check relay.log in the relay's data folder before trying again.",
	ClockWarning:       "This computer's clock differs from the server's by about %d minutes. Check the clocks on the logging PC and server. A wrong logging PC clock can put contacts outside the event's operating period.",
	AttentionCall:      "Call",
	AttentionTime:      "Time",
	AttentionStation:   "Station",
	AttentionWhy:       "What to check",
	Version:            "Version",
}
