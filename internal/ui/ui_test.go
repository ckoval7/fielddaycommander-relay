// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"context"
	"errors"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/app"
	"github.com/ckoval7/fielddaycommander-relay/internal/n3fjp"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
	"github.com/ckoval7/fielddaycommander-relay/internal/sender"
)

type fakeBackend struct {
	status    app.Status
	enrollErr error
	enrolled  []app.EnrollRequest
	forgotten int
	loggerErr error
	addresses []string
}

func (f *fakeBackend) Status(context.Context) (app.Status, error) { return f.status, nil }
func (f *fakeBackend) Enroll(_ context.Context, request app.EnrollRequest) error {
	f.enrolled = append(f.enrolled, request)
	return f.enrollErr
}
func (f *fakeBackend) Forget(context.Context) error { f.forgotten++; return nil }
func (f *fakeBackend) SetLoggerAddress(address string) error {
	f.addresses = append(f.addresses, address)
	return f.loggerErr
}

const testHost = "127.0.0.1:17311"

func newServer(t *testing.T, backend Backend) *Server {
	t.Helper()
	server, err := New(backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.SetPort(17311)
	return server
}

func get(server *Server, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://"+testHost+path, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func post(server *Server, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://"+testHost+path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// A DNS rebinding page reaches the port under its own host name.
func TestOtherHostNamesAreRefused(t *testing.T) {
	server := newServer(t, &fakeBackend{})
	for host, want := range map[string]int{
		testHost:             http.StatusOK,
		"localhost:17311":    http.StatusOK,
		"evil.example:17311": http.StatusForbidden,
		"evil.example":       http.StatusForbidden,
		"127.0.0.1":          http.StatusForbidden,
		"127.0.0.1:80":       http.StatusForbidden,
	} {
		request := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Errorf("%s: %d", host, recorder.Code)
		}
	}
}

func TestChangesNeedThisRunsTokenAndTheSameOrigin(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)
	good := url.Values{"form_token": {server.formToken}}

	tests := []struct {
		name    string
		form    url.Values
		headers map[string]string
	}{
		{"no token", url.Values{}, nil},
		{"wrong token", url.Values{"form_token": {"nope"}}, nil},
		{"another site's origin", good, map[string]string{"Origin": "http://evil.example"}},
		{"null origin", good, map[string]string{"Origin": "null"}},
		{"cross-site fetch", good, map[string]string{"Sec-Fetch-Site": "cross-site"}},
	}
	for _, tt := range tests {
		for _, path := range []string{"/forget", "/enroll", "/logger"} {
			if got := post(server, path, tt.form, tt.headers).Code; got != http.StatusForbidden {
				t.Errorf("%s %s: %d", tt.name, path, got)
			}
		}
	}
	if backend.forgotten != 0 || len(backend.enrolled) != 0 || len(backend.addresses) != 0 {
		t.Errorf("a refused request changed something: %+v", backend)
	}

	headers := map[string]string{"Origin": "http://" + testHost, "Sec-Fetch-Site": "same-origin"}
	if got := post(server, "/forget", good, headers).Code; got != http.StatusSeeOther {
		t.Errorf("a good request: %d", got)
	}
	if backend.forgotten != 1 {
		t.Error("a good request did nothing")
	}
}

func TestGetCannotChangeAnything(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)
	for _, path := range []string{"/forget", "/enroll", "/logger"} {
		if got := get(server, path).Code; got != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", path, got)
		}
	}
}

func TestPagesCarrySafetyHeaders(t *testing.T) {
	recorder := get(newServer(t, &fakeBackend{}), "/")
	for _, header := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if recorder.Header().Get(header) == "" {
			t.Errorf("no %s", header)
		}
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("the page mustn't be readable from other sites")
	}
}

func TestANewRelayOffersEnrollment(t *testing.T) {
	server := newServer(t, &fakeBackend{status: app.Status{Version: "26.11.1", LoggerAddress: "127.0.0.1:1000"}})
	body := get(server, "/").Body.String()

	for _, want := range []string{`action="/enroll"`, `name="code"`, `name="form_token" value="` + server.formToken, "26.11.1", `value="127.0.0.1:1000"`, "/assets/app.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `action="/forget"`) {
		t.Error("nothing to forget yet")
	}
}

func TestAnEnrolledRelayShowsItsState(t *testing.T) {
	server := newServer(t, &fakeBackend{status: app.Status{
		Enrolled: true, ServerURL: "https://logs.example.org", RelayName: "Phone Tent", Pinned: true,
		Sender: sender.Status{State: sender.Offline, Event: "Field Day 2027", ClockSkew: -10 * time.Minute, LastSent: time.Now()},
		Source: n3fjp.Status{Connected: true, LogAnswered: true},
		Counts: queue.Counts{Pending: 12, Accepted: 340, Rejected: 2, Invalid: 1},
	}})
	body := html.UnescapeString(get(server, "/").Body.String())

	for _, want := range []string{
		"https://logs.example.org", "Phone Tent", "Field Day 2027", senderStates[sender.Offline], "state-offline",
		"<dd>12</dd>", "<dd>340</dd>", "<dd>2</dd>", "<dd>1</dd>", `action="/forget"`, pageWords.SelfSigned, "about 10 minutes", pageWords.LoggerConnected,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, `action="/enroll"`) {
		t.Error("already enrolled")
	}
}

func TestEveryStateAndFailureHasWords(t *testing.T) {
	for _, state := range []sender.State{sender.Starting, sender.Online, sender.Offline, sender.RateLimited, sender.NeedsEnrollment, sender.HTTPSRequired, sender.PinMismatch, sender.Untrusted, sender.ServerTooOld, sender.RelayTooOld, sender.ServerRefused} {
		if senderStates[state] == "" {
			t.Errorf("no words for %s", state)
		}
	}
	for _, failure := range []app.EnrollFailure{app.FailBadAddress, app.FailBadCode, app.FailUnreachable, app.FailRateLimited, app.FailHTTPSRequired, app.FailRefused, app.FailKeychain, app.FailPinMismatch, app.FailUntrusted, app.FailSave} {
		if enrollErrors[failure] == "" {
			t.Errorf("no words for %s", failure)
		}
	}
}

func TestTheLoggerLineTellsConnectingFromWaitingFromConnected(t *testing.T) {
	for _, tt := range []struct {
		source n3fjp.Status
		want   string
	}{
		{n3fjp.Status{}, pageWords.LoggerConnecting},
		{n3fjp.Status{Connected: true}, pageWords.LoggerWaiting},
		{n3fjp.Status{Connected: true, LogAnswered: true}, pageWords.LoggerConnected},
	} {
		body := html.UnescapeString(get(newServer(t, &fakeBackend{status: app.Status{Source: tt.source}}), "/status").Body.String())
		if !strings.Contains(body, tt.want) {
			t.Errorf("%+v: page lacks %q", tt.source, tt.want)
		}
	}
}

// Logger data is untrusted, and all of this reaches the page.
func TestLoggerDataIsEscaped(t *testing.T) {
	attack := "<script>alert(1)</script>"
	server := newServer(t, &fakeBackend{status: app.Status{
		Attention: []queue.Attention{{Callsign: attack, QSOTime: attack, Station: attack, Code: "new_code", Message: attack}},
		RelayName: attack, Enrolled: true, ServerURL: attack,
	}})
	body := get(server, "/").Body.String()
	if strings.Contains(body, attack) {
		t.Error("logger data reached the page unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the data should still show, escaped")
	}
}

func TestAttentionUsesTheRelaysWordsThenTheServers(t *testing.T) {
	if got := attentionText(queue.Attention{Code: "unknown_station", Message: "server words"}); got != attentionCodes["unknown_station"] {
		t.Errorf("got %q", got)
	}
	if got := attentionText(queue.Attention{Code: "new_code", Message: "server words"}); got != "server words" {
		t.Errorf("got %q", got)
	}
}

func TestEnrollFailureKeepsTheFormFilled(t *testing.T) {
	backend := &fakeBackend{enrollErr: &app.EnrollError{Failure: app.FailBadCode, Err: errors.New("no")}}
	server := newServer(t, backend)
	form := url.Values{"form_token": {server.formToken}, "server_url": {"logs.example.org"}, "code": {" ABCD "}}

	recorder := post(server, "/enroll", form, nil)
	body := html.UnescapeString(recorder.Body.String())

	if !strings.Contains(body, enrollErrors[app.FailBadCode]) {
		t.Error("no explanation shown")
	}
	if !strings.Contains(body, `value="logs.example.org"`) || !strings.Contains(body, `value="ABCD"`) {
		t.Error("the form lost what was typed")
	}
	if backend.enrolled[0].Code != "ABCD" {
		t.Errorf("code = %q", backend.enrolled[0].Code)
	}
}

func TestAcceptingAServersKeyTakesAnExtraStep(t *testing.T) {
	fingerprint := strings.Repeat("ab", 32)
	backend := &fakeBackend{enrollErr: &app.EnrollError{Failure: app.FailUntrusted, Fingerprint: fingerprint, Err: errors.New("x509")}}
	server := newServer(t, backend)
	form := url.Values{"form_token": {server.formToken}, "server_url": {"https://club.local"}, "code": {"C"}}

	body := post(server, "/enroll", form, nil).Body.String()
	if !strings.Contains(body, "abab abab") || !strings.Contains(body, `name="pin" value="`+fingerprint) {
		t.Fatal("the key isn't offered")
	}

	backend.enrollErr = nil
	form.Set("pin", fingerprint)
	if got := post(server, "/enroll", form, nil).Code; got != http.StatusSeeOther {
		t.Errorf("accepting: %d", got)
	}
	if last := backend.enrolled[len(backend.enrolled)-1]; last.PinSHA256 != fingerprint {
		t.Errorf("request = %+v", last)
	}
}

func TestABadLoggerAddressIsExplained(t *testing.T) {
	backend := &fakeBackend{loggerErr: app.ErrBadLoggerAddress}
	server := newServer(t, backend)
	form := url.Values{"form_token": {server.formToken}, "address": {"a b"}}

	body := post(server, "/logger", form, nil).Body.String()
	if !strings.Contains(html.UnescapeString(body), pageWords.LoggerError) {
		t.Error("no explanation")
	}

	backend.loggerErr = nil
	if got := post(server, "/logger", form, nil).Code; got != http.StatusSeeOther {
		t.Errorf("good address: %d", got)
	}
}

func TestAnOversizedFormIsRefused(t *testing.T) {
	server := newServer(t, &fakeBackend{})
	form := url.Values{"form_token": {server.formToken}, "code": {strings.Repeat("x", maxFormBytes+1)}}
	if got := post(server, "/enroll", form, nil).Code; got != http.StatusBadRequest {
		t.Errorf("got %d", got)
	}
}

func TestAssetsAndHealthAreServed(t *testing.T) {
	server := newServer(t, &fakeBackend{})
	if body := get(server, "/health").Body.String(); body != HealthMarker {
		t.Errorf("health = %q", body)
	}
	for _, path := range []string{"/assets/app.js", "/assets/app.css", "/assets/favicon.png"} {
		if got := get(server, path).Code; got != http.StatusOK {
			t.Errorf("%s: %d", path, got)
		}
	}
	if got := get(server, "/assets/../index.html").Code; got == http.StatusOK {
		t.Error("escaped the assets folder")
	}
}

func TestItOnlyServesOnLoopback(t *testing.T) {
	server := newServer(t, &fakeBackend{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	outside, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	if err := server.Serve(ctx, outside); err == nil {
		t.Error("served on every interface")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	var response *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if response, err = http.Get("http://" + listener.Addr().String() + "/health"); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d", response.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestTheScriptSwapsOnlyTheStatus(t *testing.T) {
	body := get(newServer(t, &fakeBackend{}), "/status").Body.String()
	if regexp.MustCompile(`<form|<html`).MatchString(body) {
		t.Error("the refreshed part must leave the forms alone")
	}
}
