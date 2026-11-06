// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/settings"
)

// stationServer knows some stations and, like the real server, holds a contact back
// when it names a station it doesn't have and may not create.
type stationServer struct {
	mu     sync.Mutex
	create bool
	sent   map[string]string
}

func (s *stationServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.URL.Path {
	case "/api/relay/v1/enroll":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"fdca_secret_token","relay":{"id":1,"name":"Phone Tent"}}`))
	case "/api/relay/v1/handshake":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server_version": "1", "supported_api_versions": []int{1}, "minimum_relay_version": nil,
			"server_time": time.Now().UTC().Format(time.RFC3339), "relay": map[string]any{"id": 1, "name": "Phone Tent"},
			"event": nil, "permissions": map[string]any{"create_stations": s.create},
			"stations": []any{map[string]any{"name": "CW Tent", "hostname": "CW-PC"}, map[string]any{"name": "GOTA", "hostname": nil}},
		})
	case "/api/relay/v1/contacts":
		var body struct {
			Contacts []struct {
				Callsign string  `json:"callsign"`
				Station  *string `json:"station"`
			} `json:"contacts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		results := []map[string]any{}
		for i, c := range body.Contacts {
			if c.Station == nil && !s.create {
				results = append(results, map[string]any{"index": i, "status": "retry", "code": "unknown_station", "message": "create it"})
				continue
			}
			if s.sent == nil {
				s.sent = map[string]string{}
			}
			if c.Station != nil {
				s.sent[c.Callsign] = *c.Station
			}
			results = append(results, map[string]any{"index": i, "status": "accepted", "contact_uuid": "uuid-" + c.Callsign})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"contacts":[],"next_cursor":"","has_more":false}`))
	}
}

func (s *stationServer) stationOf(call string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	station, ok := s.sent[call]
	return station, ok
}

func enrolledWithStations(t *testing.T, create bool) (*harness, *stationServer) {
	t.Helper()
	server := &stationServer{create: create}
	api := httptest.NewServer(server)
	t.Cleanup(api.Close)

	h := start(t, t.TempDir(), &memoryStore{})
	if err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL, Code: "X"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the station list", func() bool {
		status, err := h.app.Status(context.Background())
		return err == nil && status.Sender.StationsKnown
	})
	return h, server
}

func putContact(t *testing.T, h *harness, call, station string) {
	t.Helper()
	c, _, err := contact.Normalize(contact.Contact{Callsign: call, QSOTime: "2026-06-27T18:05:00Z", Band: "20m", Mode: "SSB", Station: station})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.queue.Put(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	h.app.wakeSender()
}

func TestAnOperatorCanOnlyPickFromTheList(t *testing.T) {
	h, _ := enrolledWithStations(t, false)

	if err := h.app.SetStation("Night Tent", false); !errors.Is(err, ErrStationNotAllowed) {
		t.Errorf("err = %v", err)
	}
	if err := h.app.SetStation("GOTA", false); err != nil {
		t.Fatal(err)
	}
	saved, err := settings.Load(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Station != "GOTA" {
		t.Errorf("saved = %+v", saved)
	}
}

func TestPermissionToCreateAllowsANewName(t *testing.T) {
	h, _ := enrolledWithStations(t, true)
	if err := h.app.SetStation("  Night Tent ", false); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.app.Status(context.Background()); status.Station != "Night Tent" {
		t.Errorf("station = %q", status.Station)
	}
}

// The server would treat these as the existing station, so the page should too.
func TestANameTheServerWouldMatchUsesItsSpelling(t *testing.T) {
	h, _ := enrolledWithStations(t, true)
	for _, typed := range []string{"cw tent", "cw-pc"} {
		if err := h.app.SetStation(typed, false); err != nil {
			t.Fatal(err)
		}
		if status, _ := h.app.Status(context.Background()); status.Station != "CW Tent" {
			t.Errorf("%q became %q", typed, status.Station)
		}
		if err := h.app.SetStation("", false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnusableStationNamesAreRefused(t *testing.T) {
	h, _ := enrolledWithStations(t, true)
	for _, name := range []string{strings.Repeat("x", 51), "two\nlines"} {
		if err := h.app.SetStation(name, false); !errors.Is(err, ErrBadStation) {
			t.Errorf("%q: err = %v", name, err)
		}
	}
	if err := h.app.SetStation(strings.Repeat("é", 50), false); err != nil {
		t.Errorf("fifty characters should fit: %v", err)
	}
}

func TestARelayThatHasNotHeardFromTheServerCannotPick(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	if err := h.app.SetStation("GOTA", false); !errors.Is(err, ErrStationNotAllowed) {
		t.Errorf("err = %v", err)
	}
}

func TestOverrideNeedsAStation(t *testing.T) {
	h, _ := enrolledWithStations(t, false)
	if err := h.app.SetStation("GOTA", true); err != nil {
		t.Fatal(err)
	}
	if err := h.app.SetStation("", true); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.app.Status(context.Background()); status.Station != "" || status.StationOverride {
		t.Errorf("status = %q, %v", status.Station, status.StationOverride)
	}
}

func TestPickingAStationFreesContactsWaitingForOne(t *testing.T) {
	h, server := enrolledWithStations(t, false)
	putContact(t, h, "W1AW", "")
	eventually(t, "the contact to wait for a station", func() bool {
		status, err := h.app.Status(context.Background())
		return err == nil && len(status.Attention) == 1 && status.Attention[0].Station == "Phone Tent"
	})

	if err := h.app.SetStation("GOTA", false); err != nil {
		t.Fatal(err)
	}
	// Well inside the retry delay, so this was the pick and not the timer.
	eventually(t, "the contact to reach the station", func() bool {
		station, ok := server.stationOf("W1AW")
		return ok && station == "GOTA"
	})
}

func TestOverrideDecidesWhetherALoggersStationIsKept(t *testing.T) {
	h, server := enrolledWithStations(t, false)
	if err := h.app.SetStation("GOTA", false); err != nil {
		t.Fatal(err)
	}
	putContact(t, h, "K1ABC", "PC1")
	eventually(t, "the logger's station", func() bool {
		station, ok := server.stationOf("K1ABC")
		return ok && station == "PC1"
	})

	if err := h.app.SetStation("GOTA", true); err != nil {
		t.Fatal(err)
	}
	putContact(t, h, "K2ABC", "PC1")
	eventually(t, "the picked station", func() bool {
		station, ok := server.stationOf("K2ABC")
		return ok && station == "GOTA"
	})
}

func TestTheStationIsForgottenWithTheServer(t *testing.T) {
	h, _ := enrolledWithStations(t, false)
	if err := h.app.SetStation("GOTA", true); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Forget(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := settings.Load(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Station != "" || saved.StationOverride {
		t.Errorf("saved = %+v", saved)
	}
}

func TestThePickSurvivesARestart(t *testing.T) {
	h, server := enrolledWithStations(t, false)
	if err := h.app.SetStation("GOTA", false); err != nil {
		t.Fatal(err)
	}
	h.app.mu.Lock()
	h.app.restartLocked()
	h.app.mu.Unlock()

	putContact(t, h, "W1AW", "")
	eventually(t, "the picked station", func() bool {
		station, ok := server.stationOf("W1AW")
		return ok && station == "GOTA"
	})
}
