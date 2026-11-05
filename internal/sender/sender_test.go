// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package sender

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

// fakeClient answers from functions the test sets.
type fakeClient struct {
	mu        sync.Mutex
	handshake func() (api.Handshake, error)
	send      func([]contact.Contact) (map[int]api.Result, error)
	sent      [][]contact.Contact
	shakes    int
}

func (f *fakeClient) Handshake(context.Context) (api.Handshake, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shakes++
	if f.handshake != nil {
		return f.handshake()
	}
	return api.Handshake{SupportedAPIVersions: []int{1}, ServerTime: time.Now()}, nil
}

func (f *fakeClient) SendContacts(_ context.Context, contacts []contact.Contact) (map[int]api.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, contacts)
	if f.send != nil {
		return f.send(contacts)
	}
	return acceptAll(contacts), nil
}

func (f *fakeClient) setSend(send func([]contact.Contact) (map[int]api.Result, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.send = send
}

func (f *fakeClient) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func acceptAll(contacts []contact.Contact) map[int]api.Result {
	results := make(map[int]api.Result, len(contacts))
	for i := range contacts {
		results[i] = api.Result{Index: i, Status: api.StatusAccepted}
	}
	return results
}

type harness struct {
	queue  *queue.Queue
	client *fakeClient
	sender *Sender
}

func start(t *testing.T, client *fakeClient, version string) *harness {
	t.Helper()
	return startPolling(t, client, version, 5*time.Millisecond)
}

func startPolling(t *testing.T, client *fakeClient, version string, poll time.Duration) *harness {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}

	s := New(q, client, version, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.poll = poll
	s.minWait = time.Millisecond
	s.maxWait = 5 * time.Millisecond
	s.blocked = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = q.Close()
	})
	return &harness{queue: q, client: client, sender: s}
}

func (h *harness) put(t *testing.T, call string) {
	t.Helper()
	c, _, err := contact.Normalize(contact.Contact{Callsign: call, QSOTime: "2026-06-27T18:05:00Z", Band: "20m", Mode: "SSB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.queue.Put(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) counts(t *testing.T) queue.Counts {
	t.Helper()
	counts, err := h.queue.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func TestSendsAndRecordsResults(t *testing.T) {
	client := &fakeClient{send: func(contacts []contact.Contact) (map[int]api.Result, error) {
		results := acceptAll(contacts)
		for i, c := range contacts {
			if c.Callsign == "K1ABC" {
				results[i] = api.Result{Index: i, Status: api.StatusRejected, Code: "out_of_period"}
			}
		}
		return results, nil
	}}
	h := start(t, client, "26.11.1")
	h.put(t, "W1AW")
	h.put(t, "K1ABC")

	eventually(t, "contacts answered", func() bool {
		counts := h.counts(t)
		return counts.Accepted == 1 && counts.Rejected == 1 && counts.Pending == 0
	})
	if h.sender.Status().State != Online {
		t.Errorf("status = %+v", h.sender.Status())
	}
}

func TestContactsWaitOutAnOutageAndThenGoThrough(t *testing.T) {
	var outage = errors.New("no route to host")
	client := &fakeClient{send: func([]contact.Contact) (map[int]api.Result, error) { return nil, outage }}
	h := start(t, client, "26.11.1")
	h.put(t, "W1AW")

	eventually(t, "several failed attempts", func() bool { return client.sendCount() >= 3 })
	eventually(t, "offline status", func() bool { return h.sender.Status().State == Offline })
	if counts := h.counts(t); counts.Pending != 1 {
		t.Fatalf("counts during outage = %+v", counts)
	}

	client.setSend(nil)
	eventually(t, "contact sent after the outage", func() bool { return h.counts(t).Accepted == 1 })
	eventually(t, "online status", func() bool { return h.sender.Status().State == Online })
}

func TestAContactWithoutAResultStaysQueued(t *testing.T) {
	client := &fakeClient{send: func([]contact.Contact) (map[int]api.Result, error) { return map[int]api.Result{}, nil }}
	h := start(t, client, "26.11.1")
	h.put(t, "W1AW")

	eventually(t, "a send", func() bool { return client.sendCount() >= 1 })
	if counts := h.counts(t); counts.Pending != 1 || counts.Accepted != 0 {
		t.Errorf("counts = %+v", counts)
	}
}

func TestUnknownResultStatusIsARetry(t *testing.T) {
	got := outcomes([]queue.Item{{ID: 1}}, map[int]api.Result{0: {Status: "something_new"}})
	if got[0].Status != queue.Retry {
		t.Errorf("outcome = %+v", got[0])
	}
}

func TestServerRefusalsSetStateAndKeepContacts(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want State
	}{
		{"revoked", &api.StatusError{StatusCode: 401}, NeedsEnrollment},
		{"plain http", &api.StatusError{StatusCode: 403, Code: "https_required"}, HTTPSRequired},
		{"rate limit", &api.StatusError{StatusCode: 429, RetryAfter: time.Millisecond}, RateLimited},
		{"server error", &api.StatusError{StatusCode: 503}, Offline},
		{"not found", &api.StatusError{StatusCode: 404}, ServerRefused},
		{"malformed batch", &api.StatusError{StatusCode: 422}, ServerRefused},
		{"wrong key", api.ErrPinMismatch, PinMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeClient{send: func([]contact.Contact) (map[int]api.Result, error) { return nil, tt.err }}
			h := start(t, client, "26.11.1")
			h.put(t, "W1AW")

			eventually(t, string(tt.want), func() bool { return h.sender.Status().State == tt.want })
			if counts := h.counts(t); counts.Pending != 1 {
				t.Errorf("counts = %+v", counts)
			}
		})
	}
}

func TestIncompatibleServerOrRelaySendsNothing(t *testing.T) {
	minimum := "27.1.0"
	tests := []struct {
		name      string
		handshake api.Handshake
		version   string
		want      State
	}{
		{"server without v1", api.Handshake{SupportedAPIVersions: []int{2}}, "26.11.1", ServerTooOld},
		{"relay too old", api.Handshake{SupportedAPIVersions: []int{1}, MinimumRelayVersion: &minimum}, "26.11.1", RelayTooOld},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeClient{handshake: func() (api.Handshake, error) { return tt.handshake, nil }}
			h := start(t, client, tt.version)
			h.put(t, "W1AW")

			eventually(t, string(tt.want), func() bool { return h.sender.Status().State == tt.want })
			if client.sendCount() != 0 {
				t.Error("contacts were sent to an incompatible server")
			}
		})
	}
}

func TestHandshakeAgainAfterAFailure(t *testing.T) {
	failing := true
	var mu sync.Mutex
	client := &fakeClient{send: func(contacts []contact.Contact) (map[int]api.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			failing = false
			return nil, errors.New("connection reset")
		}
		return acceptAll(contacts), nil
	}}
	h := start(t, client, "26.11.1")
	h.put(t, "W1AW")

	eventually(t, "contact sent", func() bool { return h.counts(t).Accepted == 1 })
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.shakes < 2 {
		t.Errorf("handshakes = %d", client.shakes)
	}
}

func TestStatusReportsEventAndClockSkew(t *testing.T) {
	client := &fakeClient{handshake: func() (api.Handshake, error) {
		return api.Handshake{
			SupportedAPIVersions: []int{1},
			ServerVersion:        "26.11.2",
			ServerTime:           time.Now().Add(-time.Hour),
			Event:                &api.Event{Name: "Field Day 2027"},
		}, nil
	}}
	h := start(t, client, "26.11.1")

	eventually(t, "handshake", func() bool { return h.sender.Status().Event != "" })
	status := h.sender.Status()
	if status.Event != "Field Day 2027" || status.ServerVersion != "26.11.2" || status.ClockSkew < 59*time.Minute {
		t.Errorf("status = %+v", status)
	}
}

func TestWakeSendsWithoutWaitingForThePoll(t *testing.T) {
	client := &fakeClient{}
	h := startPolling(t, client, "26.11.1", time.Hour)

	h.put(t, "W1AW")
	h.sender.Wake()
	eventually(t, "contact sent", func() bool { return h.counts(t).Accepted == 1 })
}

func TestVersionLess(t *testing.T) {
	tests := []struct {
		have, want string
		less       bool
	}{
		{"26.11.1", "26.11.2", true},
		{"26.9.1", "26.11.0", true},
		{"26.11", "26.11.0", true},
		{"26.11.2", "26.11.2", false},
		{"v27.1.0", "26.11.2", false},
		{"dev", "99.0.0", false},
		{"26.11.1", "not-a-version", false},
	}
	for _, tt := range tests {
		if got := versionLess(tt.have, tt.want); got != tt.less {
			t.Errorf("versionLess(%q, %q) = %v", tt.have, tt.want, got)
		}
	}
}
