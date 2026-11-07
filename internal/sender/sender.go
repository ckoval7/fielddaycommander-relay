// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package sender moves contacts from the queue to the server, and keeps
// waiting and trying through outages for as long as the relay runs.
package sender

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/backoff"
	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

const (
	batchSize     = 50
	apiVersion    = 1
	pollInterval  = 15 * time.Second
	backoffStart  = 2 * time.Second
	backoffMax    = 5 * time.Minute
	blockedWait   = 5 * time.Minute
	clockSkewWarn = 2 * time.Minute
	// How often to ask again for what can change on the server: its stations,
	// permissions and event dates.
	handshakeEvery = 5 * time.Minute

	noResultCode = "no_result"
)

// State says what the sender is doing. The status page turns it into words.
type State string

// States. Every state other than Online leaves contacts safely in the queue.
const (
	Starting        State = "starting"
	Online          State = "online"
	Offline         State = "offline"
	RateLimited     State = "rate_limited"
	NeedsEnrollment State = "needs_enrollment"
	HTTPSRequired   State = "https_required"
	PinMismatch     State = "pin_mismatch"
	Untrusted       State = "untrusted_certificate"
	ServerTooOld    State = "server_too_old"
	RelayTooOld     State = "relay_too_old"
	ServerRefused   State = "server_refused"
)

// Status is a snapshot for the status page.
type Status struct {
	State         State
	LastSent      time.Time
	Event         string
	ServerVersion string
	// ClockSkew is how far this computer's clock is from the server's, set when
	// it's far enough off to put contacts outside the event.
	ClockSkew time.Duration
	// StationsKnown is whether the server has said what's in Stations and
	// CanCreateStations. Stations can be empty when it has.
	StationsKnown     bool
	Stations          []api.Station
	CanCreateStations bool
}

// StationChoice is the station a person picked for this relay.
type StationChoice struct {
	Name string
	// Override also replaces a station the contact names itself.
	Override bool
}

// For returns the station to send for a contact that names station.
func (c StationChoice) For(station string) string {
	if c.Name != "" && (c.Override || station == "") {
		return c.Name
	}
	return station
}

// Client is the part of the API the sender uses.
type Client interface {
	Handshake(ctx context.Context) (api.Handshake, error)
	SendContacts(ctx context.Context, contacts []contact.Contact) (map[int]api.Result, error)
}

// Sender runs the send loop.
type Sender struct {
	queue   *queue.Queue
	client  Client
	version string
	log     *slog.Logger
	wake    chan struct{}

	station       atomic.Pointer[StationChoice]
	askAgain      atomic.Bool
	lastHandshake time.Time

	// Test hooks for the waits.
	poll           time.Duration
	maxWait        time.Duration
	minWait        time.Duration
	blocked        time.Duration
	handshakeEvery time.Duration
	statusMu       sync.Mutex
	status         Status
}

// New returns a Sender for this relay version.
func New(q *queue.Queue, client Client, version string, log *slog.Logger) *Sender {
	return &Sender{
		queue:   q,
		client:  client,
		version: version,
		log:     log,
		wake:    make(chan struct{}, 1),
		poll:    pollInterval,
		minWait: backoffStart,
		maxWait: backoffMax,
		blocked: blockedWait,
		status:  Status{State: Starting},

		handshakeEvery: handshakeEvery,
	}
}

// SetStation sets the station for contacts sent from now on. Queued contacts keep
// what the logger said, so a new choice also reaches those still waiting, and
// nothing already sent goes out again.
func (s *Sender) SetStation(choice StationChoice) {
	s.station.Store(&choice)
}

// AskServerAgain gets the server's stations, permissions and event dates again
// without waiting for the next regular check.
func (s *Sender) AskServerAgain() {
	s.askAgain.Store(true)
	s.Wake()
}

// Wake tells the sender a contact is waiting, so it doesn't wait for its next
// poll.
func (s *Sender) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Status returns the current status.
func (s *Sender) Status() Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

// Run sends until ctx is canceled.
func (s *Sender) Run(ctx context.Context) {
	failures := 0
	shookHands := false

	for ctx.Err() == nil {
		wait, ok := s.step(ctx, &shookHands)
		if ok {
			failures = 0
		} else {
			failures++
			shookHands = false
		}

		switch {
		case !ok && wait == 0:
			wait = backoff.Wait(failures, s.minWait, s.maxWait)
		case ok && wait == 0:
			continue // There may be more to send right away.
		}

		if !s.sleep(ctx, wait, ok) {
			return
		}
	}
}

// step does one round of work. It returns how long to wait before the next
// round, with 0 meaning no particular wait, and whether the server was usable.
func (s *Sender) step(ctx context.Context, shookHands *bool) (time.Duration, bool) {
	if !*shookHands || s.askAgain.Swap(false) || time.Since(s.lastHandshake) >= s.handshakeEvery {
		if wait, ok := s.handshake(ctx); !ok {
			return wait, false
		}
		*shookHands = true
	}

	items, err := s.queue.Next(ctx, batchSize)
	if err != nil {
		s.log.Error("reading the queue failed", "error", err)
		return 0, false
	}
	if len(items) == 0 {
		return s.poll, true
	}

	var choice StationChoice
	if current := s.station.Load(); current != nil {
		choice = *current
	}
	contacts := make([]contact.Contact, len(items))
	for i, item := range items {
		contacts[i] = item.Contact
		contacts[i].Station = choice.For(item.Contact.Station)
	}

	results, err := s.client.SendContacts(ctx, contacts)
	if err != nil {
		return s.classify(err)
	}

	// The server has answered, so shutting down now shouldn't lose the answer.
	// If recording fails anyway, the contacts go out again as repeats.
	if err := s.queue.Apply(context.WithoutCancel(ctx), outcomes(items, results)); err != nil {
		s.log.Error("recording results failed", "error", err)
		return 0, false
	}

	s.setStatus(func(status *Status) {
		status.State = Online
		status.LastSent = time.Now()
	})
	return 0, true
}

func (s *Sender) handshake(ctx context.Context) (time.Duration, bool) {
	handshake, err := s.client.Handshake(ctx)
	if err != nil {
		return s.classify(err)
	}

	if !slices.Contains(handshake.SupportedAPIVersions, apiVersion) {
		s.setState(ServerTooOld)
		return s.blocked, false
	}
	if minimum := handshake.MinimumRelayVersion; minimum != nil && versionLess(s.version, *minimum) {
		s.setState(RelayTooOld)
		return s.blocked, false
	}

	if event := handshake.Event; event != nil {
		if err := s.queue.SetEventPeriod(ctx, event.StartsAt, event.EndsAt); err != nil {
			s.log.Error("saving the event's dates failed", "error", err)
		}
	}

	s.lastHandshake = time.Now()
	s.setStatus(func(status *Status) {
		status.State = Online
		status.StationsKnown = true
		status.Stations = handshake.Stations
		status.CanCreateStations = handshake.Permissions.CreateStations
		status.ServerVersion = handshake.ServerVersion
		status.Event = ""
		if handshake.Event != nil {
			status.Event = handshake.Event.Name
		}
		status.ClockSkew = 0
		if skew := time.Since(handshake.ServerTime); skew > clockSkewWarn || skew < -clockSkewWarn {
			status.ClockSkew = skew
		}
	})
	return 0, true
}

// classify decides what a failed request means. Nothing here touches the
// queue: whatever went wrong, the contacts stay until the server answers them.
func (s *Sender) classify(err error) (time.Duration, bool) {
	if errors.Is(err, context.Canceled) {
		return 0, false
	}

	var statusErr *api.StatusError
	switch {
	case errors.Is(err, api.ErrPinMismatch):
		s.setState(PinMismatch)
		return s.blocked, false
	case api.IsCertificateError(err):
		s.log.Warn("server's certificate isn't trusted", "error", err)
		s.setState(Untrusted)
		return s.blocked, false
	case !errors.As(err, &statusErr):
		s.log.Warn("couldn't reach the server", "error", err)
		s.setState(Offline)
		return 0, false
	}

	s.log.Warn("server refused the request", "status", statusErr.StatusCode, "code", statusErr.Code)
	switch {
	case statusErr.StatusCode == http.StatusUnauthorized:
		s.setState(NeedsEnrollment)
		return s.blocked, false
	case statusErr.StatusCode == http.StatusForbidden && statusErr.Code == "https_required":
		s.setState(HTTPSRequired)
		return s.blocked, false
	case statusErr.StatusCode == http.StatusTooManyRequests:
		s.setState(RateLimited)
		return statusErr.RetryAfter, false
	case statusErr.StatusCode >= http.StatusInternalServerError:
		s.setState(Offline)
		return statusErr.RetryAfter, false
	default:
		s.setState(ServerRefused)
		return s.blocked, false
	}
}

// outcomes pairs each item with its result. A contact the server didn't report
// on wasn't stored as far as the relay can tell, so it goes out again.
func outcomes(items []queue.Item, results map[int]api.Result) []queue.Outcome {
	out := make([]queue.Outcome, len(items))
	for i, item := range items {
		result, reported := results[i]
		out[i] = queue.Outcome{ID: item.ID, Status: queue.Retry, Code: noResultCode}
		if !reported {
			continue
		}

		out[i].Code = result.Code
		out[i].Message = result.Message
		switch result.Status {
		case api.StatusAccepted:
			out[i].Status = queue.Accepted
			out[i].ContactUUID = result.ContactUUID
		case api.StatusRejected:
			out[i].Status = queue.Rejected
		}
	}
	return out
}

// sleep waits and reports whether to keep running. While the server is
// unusable a new contact doesn't cut the wait short: the contact is already
// safe, and each attempt costs battery on a bad network.
func (s *Sender) sleep(ctx context.Context, wait time.Duration, wakeable bool) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	var wake <-chan struct{}
	if wakeable {
		wake = s.wake
	}
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	case <-wake:
	}
	return true
}

func (s *Sender) setState(state State) {
	s.setStatus(func(status *Status) { status.State = state })
}

func (s *Sender) setStatus(change func(*Status)) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	change(&s.status)
}

// versionLess compares dotted numeric versions like 26.11.1. A version that
// isn't one, such as a development build, is never considered too old.
func versionLess(have, want string) bool {
	haveParts, ok := versionParts(have)
	if !ok {
		return false
	}
	wantParts, ok := versionParts(want)
	if !ok {
		return false
	}
	return slices.Compare(haveParts, wantParts) < 0
}

func versionParts(version string) ([]int, bool) {
	fields := strings.Split(strings.TrimPrefix(version, "v"), ".")
	parts := make([]int, len(fields))
	for i, field := range fields {
		part, err := strconv.Atoi(field)
		if err != nil {
			return nil, false
		}
		parts[i] = part
	}
	return parts, true
}
