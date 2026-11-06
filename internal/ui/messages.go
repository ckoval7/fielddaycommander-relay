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
	sender.Starting:        "Contacts waiting to send stay on this computer.",
	sender.Online:          "Connected to Field Day Commander.",
	sender.Offline:         "Check this computer's connection. Saved contacts will go out when the server is reachable.",
	sender.RateLimited:     "The server asked for a pause. Contacts stay saved here, and the relay will retry automatically.",
	sender.NeedsEnrollment: "This relay was revoked or its event was deleted. Click Disconnect, then connect with a new enrollment code from the server's Relays page. Queued contacts stay on this computer.",
	sender.HTTPSRequired:   "Click Disconnect, then connect with the server's https address and a new code. Queued contacts are kept.",
	sender.PinMismatch:     "The server's key changed. Contacts stay saved here. Ask whoever runs the server to confirm the new key before you click Disconnect and connect with a new code.",
	sender.Untrusted:       "The relay couldn't verify the server's certificate. Queued contacts are kept. Ask whoever runs the server to check its certificate before you connect again.",
	sender.ServerTooOld:    "The server doesn't support this relay version. Ask whoever runs it to update Field Day Commander. Contacts will wait on this computer.",
	sender.RelayTooOld:     "The server needs a newer relay version. Update Field Day Commander Relay. Saved contacts are kept through the restart.",
	sender.ServerRefused:   "Check the server address with whoever runs it. Contacts stay queued. See relay.log in the relay's data folder for details.",
}

// senderLabels are the short forms shown on the status pill.
var senderLabels = map[sender.State]string{
	sender.Starting:        "Starting",
	sender.Online:          "Connected",
	sender.Offline:         "Offline",
	sender.RateLimited:     "Waiting",
	sender.NeedsEnrollment: "Connect again",
	sender.HTTPSRequired:   "Needs https",
	sender.PinMismatch:     "Sending stopped",
	sender.Untrusted:       "Sending stopped",
	sender.ServerTooOld:    "Update server",
	sender.RelayTooOld:     "Update relay",
	sender.ServerRefused:   "Refused",
}

// senderBadgeClass is the daisyUI color for a state's status badge.
func senderBadgeClass(state sender.State) string {
	switch state {
	case sender.Online:
		return "badge-success"
	case sender.Starting, sender.RateLimited:
		return "badge-neutral"
	}
	return "badge-error"
}

func senderLabelText(state sender.State) string {
	if text, ok := senderLabels[state]; ok {
		return text
	}
	return senderLabels[sender.Starting]
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
	RoleHeading, RoleClientLabel, RoleServerLabel, RoleHelp             string
	ListenLabel, ListenHelp, RoleButton                                 string
	ServerRoleListening, ServerRoleFailed, ServerClients, ServerRecords string
	ConnectHeading, SetupHeading, ServerSetupHeading                    string
	NotConnectedPill, LoggerLookingPill, LoggerWaitingPill              string
	LoggerConnectedPill, ServerReadyPill, ServerBlockedPill             string
	ThemeToLight, ThemeToDark                                           string
}

var pageWords = pageText{
	Title:               "Field Day Commander Relay",
	ServerHeading:       "Server",
	LoggerHeading:       "N3FJP",
	QueueHeading:        "Contacts",
	AttentionHeading:    "Contacts to check",
	NotEnrolled:         "Contacts are being collected and will go out once you connect.",
	EnrollHelp:          "Create an enrollment code on the server's Relays page, then enter it with the server's https address. Codes work once and expire after 15 minutes. Each relay is tied to one event. For a new event, disconnect and connect with a new code.",
	ServerLabel:         "Server address",
	CodeLabel:           "Enrollment code",
	EnrollButton:        "Connect",
	KeyLabel:            "Server's key",
	AcceptButton:        "The keys match, connect",
	RelayNameLabel:      "This relay",
	EventLabel:          "Event",
	ServerAddressLabel:  "Server",
	LastSentLabel:       "Last sent",
	SelfSigned:          "Using the server key you accepted.",
	ForgetButton:        "Disconnect",
	ForgetHelp:          "Disconnect removes the token from this computer and keeps queued contacts. To revoke the relay on the server too, open its Relays page and choose Revoke.",
	LoggerConnecting:    "Check that N3FJP's network server is running, select TCP networking in Settings > Network, and check the \"N3FJP address\" field under N3FJP setup. Collected contacts stay saved here.",
	LoggerWaiting:       "Select TCP networking in Settings > Network. File share mode won't work.",
	LoggerConnected:     "Connected to N3FJP and receiving log data.",
	LoggerHelp:          "Enter the N3FJP network server PC's address. The default is this computer, 127.0.0.1:1000. Select TCP networking in Settings > Network. File share mode won't work. While the relay is disconnected from N3FJP, edits and deletes won't reach Field Day Commander because N3FJP's log doesn't record them. New contacts are picked up on reconnect.",
	LoggerAddressLabel:  "N3FJP address",
	LoggerButton:        "Save",
	LoggerError:         "That N3FJP address isn't usable. Enter a computer name or address, with an optional port, like 192.168.1.5 or 192.168.1.5:1000.",
	Waiting:             "Waiting",
	Sent:                "Sent",
	Rejected:            "Rejected",
	Unreadable:          "Unreadable",
	ActionError:         "The relay couldn't save that change. Check relay.log in the relay's data folder before trying again.",
	ClockWarning:        "This computer's clock differs from the server's by about %d minutes. Check the clocks on the logging PC and server. A wrong logging PC clock can put contacts outside the event's operating period.",
	AttentionCall:       "Call",
	AttentionTime:       "Time",
	AttentionStation:    "Station",
	AttentionWhy:        "What to check",
	Version:             "Version",
	ThemeToLight:        "Switch to light mode",
	ThemeToDark:         "Switch to dark mode",
	ConnectHeading:      "Connect to your server",
	SetupHeading:        "N3FJP setup",
	ServerSetupHeading:  "Server connection",
	NotConnectedPill:    "Not connected",
	LoggerLookingPill:   "Looking for N3FJP",
	LoggerWaitingPill:   "Waiting for log",
	LoggerConnectedPill: "Connected",
	ServerReadyPill:     "Ready",
	ServerBlockedPill:   "Port blocked",
	RoleHeading:         "How should the relay connect to N3FJP?",
	RoleClientLabel:     "Join an N3FJP network server",
	RoleServerLabel:     "Have N3FJP PCs connect here",
	RoleHelp:            "On each N3FJP PC, select TCP networking and set the server to this computer in Settings > Network. File share mode won't work. N3FJP clients don't keep their own log, so logging stops while the relay is closed. The relay saves contacts to disk and works without internet. Both roles sync contacts, corrections, and deletes both ways with Field Day Commander. Other values, such as a 5NN report, stay as entered in N3FJP.",
	ListenLabel:         "Let other PCs connect",
	ListenHelp:          "N3FJP on this computer can already connect. Turn this on for other PCs. N3FJP's network doesn't have a password. Anyone who can reach port 1000 can add, change, or delete contacts. Keep it on your club's own network. Never forward port 1000 from the internet. Windows may ask you to allow the relay through its firewall. This page can't confirm that permission.",
	RoleButton:          "Save",
	ServerRoleListening: "Listening for N3FJP connections on port 1000.",
	ServerRoleFailed:    "Check whether N3FJP's network server is running on this computer. If so, turn it off in N3FJP to free port 1000. The relay keeps retrying.",
	ServerClients:       "N3FJP PCs connected",
	ServerRecords:       "Contacts saved on the relay",
}
