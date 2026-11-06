// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/app"
	"github.com/ckoval7/fielddaycommander-relay/internal/sender"
)

func stationStatus(picked string, create bool, names ...string) app.Status {
	stations := make([]api.Station, len(names))
	for i, name := range names {
		stations[i] = api.Station{Name: name}
	}
	return app.Status{
		Enrolled: true, ServerURL: "https://logs.example.org", RelayName: "Phone Tent", Station: picked,
		Sender: sender.Status{State: sender.Online, StationsKnown: true, Stations: stations, CanCreateStations: create},
	}
}

func stationForm(values map[string]string, token string) url.Values {
	form := url.Values{"form_token": {token}}
	for name, value := range values {
		form.Set(name, value)
	}
	return form
}

func TestOperatorsGetAFixedListOfStations(t *testing.T) {
	server := newServer(t, &fakeBackend{status: stationStatus("", false, "CW Tent", "GOTA")})
	body := html.UnescapeString(get(server, "/").Body.String())

	for _, want := range []string{
		`action="/station"`, `<option value="CW Tent">CW Tent</option>`, `<option value="GOTA">GOTA</option>`,
		fmt.Sprintf(pageWords.StationDefault, "Phone Tent"),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, unwanted := range []string{`name="new_station"`, `name="override"`, `data-new-station`, pageWords.StationNewOption} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page offers %q", unwanted)
		}
	}
}

func TestPermissionToCreateStationsOffersANewOne(t *testing.T) {
	server := newServer(t, &fakeBackend{status: stationStatus("", true, "CW Tent")})
	body := get(server, "/").Body.String()
	for _, want := range []string{`data-new-station`, `name="new_station"`, `id="new-station"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The name field waits until someone picks the option.
	if form := body[strings.Index(body, `id="new-station"`):]; !strings.Contains(form[:strings.Index(form, ">")], "hidden") {
		t.Error("the new station form should start hidden")
	}
}

func TestPickingNewStationKeepsThePickUntilItIsNamed(t *testing.T) {
	// The option's value is the current pick, so nothing can save it as a station's name.
	body := get(newServer(t, &fakeBackend{status: stationStatus("GOTA", true, "GOTA")}), "/").Body.String()
	if !strings.Contains(body, `<option value="GOTA" data-new-station>`) {
		t.Error("the new station option should carry the current pick")
	}
}

func TestARefusedNewStationStaysOpenWithItsName(t *testing.T) {
	server := newServer(t, &fakeBackend{status: stationStatus("", true, "GOTA"), stationErr: app.ErrBadStation})
	body := post(server, "/station", stationForm(map[string]string{"new_station": "Night Tent"}, server.formToken), nil).Body.String()
	for _, want := range []string{`value="Night Tent"`, `data-new-station selected`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if form := body[strings.Index(body, `id="new-station"`):]; strings.Contains(form[:strings.Index(form, ">")], "hidden") {
		t.Error("the new station form should stay open")
	}
}

func TestThePickedStationIsSelectedAndCanOverride(t *testing.T) {
	status := stationStatus("GOTA", false, "CW Tent", "GOTA")
	status.StationOverride = true
	server := newServer(t, &fakeBackend{status: status})
	body := get(server, "/").Body.String()

	for _, want := range []string{`<option value="GOTA" selected>`, `name="override" data-autosave checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestAPickedStationTheServerLacksIsExplained(t *testing.T) {
	missing := stationStatus("Old Tent", false, "CW Tent")
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: missing}), "/").Body.String())
	if !strings.Contains(body, pageWords.StationMissing) || !strings.Contains(body, `<option value="Old Tent" selected>`) {
		t.Error("a missing station should stay selected and be explained")
	}

	coming := stationStatus("New Tent", true, "CW Tent")
	body = html.UnescapeString(get(newServer(t, &fakeBackend{status: coming}), "/").Body.String())
	if !strings.Contains(body, pageWords.StationUnlisted) || strings.Contains(body, pageWords.StationMissing) {
		t.Error("a station about to be created isn't a problem")
	}
}

func TestAnEventWithNoStationsSaysSoOnlyToThoseWhoCannotCreateOne(t *testing.T) {
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: stationStatus("", false)}), "/").Body.String())
	if !strings.Contains(body, pageWords.StationEmpty) {
		t.Error("an operator isn't told to ask for a station")
	}
	body = html.UnescapeString(get(newServer(t, &fakeBackend{status: stationStatus("", true)}), "/").Body.String())
	if strings.Contains(body, pageWords.StationEmpty) {
		t.Error("someone who can create a station is told to ask")
	}
}

func TestBeforeTheServerAnswersThePickIsShownWithoutAList(t *testing.T) {
	status := app.Status{Enrolled: true, ServerURL: "https://logs.example.org", Station: "GOTA", Sender: sender.Status{State: sender.Offline}}
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: status}), "/").Body.String())
	if !strings.Contains(body, "GOTA") || !strings.Contains(body, pageWords.StationWaiting) || strings.Contains(body, `action="/station"`) {
		t.Errorf("page should show the pick and wait for the list")
	}
}

func TestStationNamesAreEscaped(t *testing.T) {
	attack := `"><script>alert(1)</script>`
	server := newServer(t, &fakeBackend{status: stationStatus(attack, false, attack)})
	if strings.Contains(get(server, "/").Body.String(), "<script>alert") {
		t.Error("a station name reached the page unescaped")
	}
}

func TestChoosingAStationSaves(t *testing.T) {
	backend := &fakeBackend{status: stationStatus("", true, "GOTA")}
	server := newServer(t, backend)

	response := post(server, "/station", stationForm(map[string]string{"station": "GOTA", "override": "on"}, server.formToken), nil)
	if response.Code != http.StatusSeeOther || backend.station != "GOTA" || !backend.override {
		t.Errorf("code %d, station %q, override %v", response.Code, backend.station, backend.override)
	}

	post(server, "/station", stationForm(map[string]string{"new_station": "  Night Tent ", "station": "GOTA"}, server.formToken), nil)
	if backend.station != "Night Tent" || backend.override {
		t.Errorf("station %q, override %v", backend.station, backend.override)
	}
}

func TestChoosingAStationNeedsThisRunsToken(t *testing.T) {
	backend := &fakeBackend{status: stationStatus("", true)}
	post(newServer(t, backend), "/station", stationForm(map[string]string{"station": "GOTA"}, "wrong"), nil)
	if backend.stationSet != 0 {
		t.Error("a request without the token changed the station")
	}
}

func TestARefusedStationIsExplained(t *testing.T) {
	for err, want := range map[error]string{
		app.ErrBadStation:        pageWords.StationBadName,
		app.ErrStationNotAllowed: pageWords.StationNotAllowed,
	} {
		server := newServer(t, &fakeBackend{status: stationStatus("", true), stationErr: err})
		body := html.UnescapeString(post(server, "/station", stationForm(map[string]string{"station": "x"}, server.formToken), nil).Body.String())
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestOpeningThePageAsksTheServerAgainButRefreshingDoesNot(t *testing.T) {
	backend := &fakeBackend{status: stationStatus("", false)}
	server := newServer(t, backend)
	get(server, "/status")
	if backend.asked != 0 {
		t.Error("each refresh would reach the server")
	}
	get(server, "/")
	if backend.asked != 1 {
		t.Errorf("asked %d times", backend.asked)
	}
}

func TestTheRefreshCarriesTheStationListOnlyWhenConnected(t *testing.T) {
	if body := get(newServer(t, &fakeBackend{status: stationStatus("", false, "GOTA")}), "/status").Body.String(); !strings.Contains(body, `<option value="GOTA">`) {
		t.Error("a new list wouldn't reach an open page")
	}
	if body := get(newServer(t, &fakeBackend{}), "/status").Body.String(); strings.Contains(body, `action="/station"`) || strings.Contains(body, pageWords.StationWaiting) {
		t.Error("a relay that isn't connected has no station to choose")
	}
}
