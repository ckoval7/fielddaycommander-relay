// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/app"
	"github.com/ckoval7/fielddaycommander-relay/internal/autostart"
	"github.com/ckoval7/fielddaycommander-relay/internal/logfile"
	"github.com/ckoval7/fielddaycommander-relay/internal/n1mm"
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
	role      string
	listen    bool
	choosing  int
	removed   []string
	removeErr error
	n1mmOn    bool
	n1mmLAN   bool
	n1mmErr   error

	station    string
	override   bool
	stationSet int
	stationErr error
	asked      int

	startup    autostart.Status
	startupSet []bool
	startupErr error
}

func (f *fakeBackend) Status(context.Context) (app.Status, error) { return f.status, nil }
func (f *fakeBackend) Enroll(_ context.Context, request app.EnrollRequest) error {
	f.enrolled = append(f.enrolled, request)
	return f.enrollErr
}
func (f *fakeBackend) Forget(context.Context) error { f.forgotten++; return nil }
func (f *fakeBackend) SetLogger(role, address string, listen bool) error {
	f.role, f.listen = role, listen
	f.addresses = append(f.addresses, address)
	return f.loggerErr
}

func (f *fakeBackend) SetN1MM(enabled, onNetwork bool) error {
	f.n1mmOn, f.n1mmLAN = enabled, onNetwork
	return f.n1mmErr
}
func (f *fakeBackend) SetStation(name string, override bool) error {
	f.station, f.override = name, override
	f.stationSet++
	return f.stationErr
}
func (f *fakeBackend) AskServerAgain() { f.asked++ }
func (f *fakeBackend) ChooseLogFile()  { f.choosing++ }
func (f *fakeBackend) RemoveLogFile(path string) error {
	f.removed = append(f.removed, path)
	return f.removeErr
}

func (f *fakeBackend) StartAtLogin() autostart.Status { return f.startup }
func (f *fakeBackend) SetStartAtLogin(on bool) error {
	f.startupSet = append(f.startupSet, on)
	return f.startupErr
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
		for _, path := range []string{"/forget", "/enroll", "/n3fjp", "/n1mm", "/logfiles/choose", "/logfiles/remove"} {
			if got := post(server, path, tt.form, tt.headers).Code; got != http.StatusForbidden {
				t.Errorf("%s %s: %d", tt.name, path, got)
			}
		}
	}
	if backend.forgotten != 0 || len(backend.enrolled) != 0 || len(backend.addresses) != 0 || backend.n1mmOn || backend.choosing != 0 || len(backend.removed) != 0 {
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

// A browser's own form post, as it really arrives. Referrer-Policy: no-referrer
// makes browsers send "Origin: null" here, which refused every Save.
func TestThePagesOwnFormsWorkTheWayABrowserSendsThem(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)

	if got := get(server, "/").Header().Get("Referrer-Policy"); got == "no-referrer" || got == "" {
		t.Errorf("Referrer-Policy %q makes browsers send Origin: null on form posts", got)
	}

	browser := map[string]string{"Origin": "http://" + testHost, "Sec-Fetch-Site": "same-origin", "Referer": "http://" + testHost + "/"}
	for path, form := range map[string]url.Values{
		"/n3fjp":  {"role": {"server"}, "address": {"192.168.1.5"}},
		"/forget": {},
	} {
		form.Set("form_token", server.formToken)
		if got := post(server, path, form, browser).Code; got != http.StatusSeeOther {
			t.Errorf("%s: %d", path, got)
		}
	}
	if len(backend.addresses) != 1 || backend.role != "server" || backend.forgotten != 1 {
		t.Errorf("backend = %+v", backend)
	}
}

func TestATabLeftOpenAcrossARestartGetsAFreshPageAndChangesNothing(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)
	form := url.Values{"form_token": {"token-from-the-last-run"}, "address": {"192.168.1.5"}}

	recorder := post(server, "/n3fjp", form, map[string]string{"Origin": "http://" + testHost, "Sec-Fetch-Site": "same-origin"})
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/" {
		t.Errorf("got %d to %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if len(backend.addresses) != 0 {
		t.Error("an old token changed something")
	}
}

func TestGetCannotChangeAnything(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)
	for _, path := range []string{"/forget", "/enroll", "/n3fjp", "/n1mm", "/logfiles/choose", "/logfiles/remove"} {
		if got := get(server, path).Code; got != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", path, got)
		}
	}
}

func TestPagesCarrySafetyHeaders(t *testing.T) {
	recorder := get(newServer(t, &fakeBackend{}), "/")
	// daisyUI draws some controls with inline SVG, and nothing else may load.
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "img-src 'self' data:") || !strings.Contains(got, "default-src 'self'") {
		t.Errorf("policy = %q", got)
	}
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
		"https://logs.example.org", "Phone Tent", "Field Day 2027", senderStates[sender.Offline], "badge-error",
		`<div class="stat-value">12</div>`, `<div class="stat-value">340</div>`, `<div class="stat-value text-error">2</div>`, pageWords.Unreadable, `action="/forget"`, pageWords.SelfSigned, "about 10 minutes", pageWords.LoggerConnectedPill,
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
		if senderLabels[state] == "" {
			t.Errorf("no label for %s", state)
		}
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
		{n3fjp.Status{Connected: true, LogAnswered: true}, pageWords.LoggerConnectedPill},
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

	body := post(server, "/n3fjp", form, nil).Body.String()
	if !strings.Contains(html.UnescapeString(body), pageWords.LoggerError) {
		t.Error("no explanation")
	}

	backend.loggerErr = nil
	if got := post(server, "/n3fjp", form, nil).Code; got != http.StatusSeeOther {
		t.Errorf("good address: %d", got)
	}
}

func TestStartingAtSignInCanBeChosen(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)

	on := url.Values{"form_token": {server.formToken}, "start_at_login": {"on"}}
	if got := post(server, "/startup", on, nil).Code; got != http.StatusSeeOther {
		t.Errorf("turning on: %d", got)
	}
	off := url.Values{"form_token": {server.formToken}}
	if got := post(server, "/startup", off, nil).Code; got != http.StatusSeeOther {
		t.Errorf("turning off: %d", got)
	}
	if want := []bool{true, false}; !slices.Equal(backend.startupSet, want) {
		t.Errorf("got %v, want %v", backend.startupSet, want)
	}
	if got := post(server, "/startup", url.Values{"start_at_login": {"on"}}, nil).Code; got != http.StatusForbidden {
		t.Errorf("no token: %d", got)
	}
	if len(backend.startupSet) != 2 {
		t.Error("a request without the token changed something")
	}
}

func TestAFailedStartupChoiceSaysSo(t *testing.T) {
	backend := &fakeBackend{startupErr: errors.New("no access")}
	server := newServer(t, backend)
	form := url.Values{"form_token": {server.formToken}, "start_at_login": {"on"}}

	body := html.UnescapeString(post(server, "/startup", form, nil).Body.String())
	if !strings.Contains(body, pageWords.ActionError) {
		t.Error("the page doesn't say the change failed")
	}
}

func TestThePageShowsWhatTheNextSignInWillDo(t *testing.T) {
	checkbox := regexp.MustCompile(`<input[^>]*name="start_at_login"[^>]*>`)
	other := `C:\Old Copyelay.exe`
	tests := []struct {
		name    string
		status  autostart.Status
		offered bool
		checked bool
		text    string
	}{
		{name: "this build can't", status: autostart.Status{}},
		{name: "off", status: autostart.Status{Available: true}, offered: true},
		{name: "on", status: autostart.Status{Available: true, On: true}, offered: true, checked: true},
		{name: "another copy", status: autostart.Status{Available: true, OtherCopy: other}, offered: true, text: fmt.Sprintf(pageWords.StartAtLoginOtherCopy, other)},
		{name: "turned off in Windows", status: autostart.Status{Available: true, TurnedOffInWindows: true}, offered: true, text: pageWords.StartAtLoginTurnedOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := html.UnescapeString(get(newServer(t, &fakeBackend{startup: tt.status}), "/").Body.String())
			box := checkbox.FindString(body)
			if (box != "") != tt.offered {
				t.Fatalf("offered = %v, want %v", box != "", tt.offered)
			}
			if strings.Contains(box, " checked") != tt.checked {
				t.Errorf("checkbox: %s", box)
			}
			if tt.text != "" && !strings.Contains(body, tt.text) {
				t.Errorf("missing %q", tt.text)
			}
		})
	}
}

func TestTheRoleCanBeChosen(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)
	form := url.Values{"form_token": {server.formToken}, "role": {"server"}, "listen_on_network": {"on"}}

	if got := post(server, "/n3fjp", form, nil).Code; got != http.StatusSeeOther {
		t.Errorf("got %d", got)
	}
	if backend.role != "server" || !backend.listen {
		t.Errorf("backend = %+v", backend)
	}
	if got := post(server, "/n3fjp", url.Values{"role": {"server"}}, nil).Code; got != http.StatusForbidden {
		t.Errorf("no token: %d", got)
	}
}

func TestTheServerRoleShowsItsConnections(t *testing.T) {
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: app.Status{ServerRole: true, N3FJPServer: n3fjp.ServerStatus{Listening: true, Clients: 3, Records: 12}}}), "/").Body.String())
	for _, want := range []string{pageWords.ServerReadyPill, pageWords.ServerClients + ": 3", pageWords.ServerRecords + ": 12", `name="listen_on_network"`, `value="server" data-autosave checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, pageWords.LoggerAddressLabel) {
		t.Error("only the server role's settings should show")
	}
}

func TestOtherPCsCanOnlyConnectInTheServerRole(t *testing.T) {
	checkbox := regexp.MustCompile(`<input type="checkbox"[^>]*name="listen_on_network"[^>]*>`)

	if client := checkbox.FindString(get(newServer(t, &fakeBackend{}), "/").Body.String()); client != "" {
		t.Errorf("client role: %s", client)
	}
	server := checkbox.FindString(get(newServer(t, &fakeBackend{status: app.Status{ServerRole: true}}), "/").Body.String())
	if server == "" {
		t.Errorf("server role: %s", server)
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
	for _, path := range []string{"/assets/app.js", "/assets/theme.js", "/assets/app.css", "/assets/favicon.png"} {
		if got := get(server, path).Code; got != http.StatusOK {
			t.Errorf("%s: %d", path, got)
		}
	}
	if get(server, "/assets/../index.html").Code == http.StatusOK {
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

// The stylesheet is compiled, so a class added to the page without rebuilding it
// would silently do nothing. Run scripts/build-css.cjs if this fails.
func TestTheCompiledStylesheetCoversThePage(t *testing.T) {
	css, err := webFiles.ReadFile("web/assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	page, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	withoutActions := regexp.MustCompile(`\{\{.*?\}\}`).ReplaceAllString(string(page), " ")
	classes := []string{"badge-success", "badge-neutral", "badge-error", "badge-warning", "alert-error", "text-error"}
	for _, match := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(withoutActions, -1) {
		classes = append(classes, strings.Fields(match[1])...)
	}
	// CSS escapes characters like ":" in class names.
	styles := strings.ReplaceAll(string(css), `\`, "")
	for _, class := range classes {
		if !strings.Contains(styles, "."+class) {
			t.Errorf("no style for %q", class)
		}
	}
}

// A theme script that loads after the page paints would flash the other theme.
func TestTheThemeLoadsBeforeThePagePaints(t *testing.T) {
	body := get(newServer(t, &fakeBackend{}), "/").Body.String()
	theme := strings.Index(body, `<script src="/assets/theme.js"></script>`)
	if theme < 0 || theme > strings.Index(body, "</head>") {
		t.Error("theme.js must load in the head, without defer")
	}
	if !strings.Contains(body, `id="theme-toggle"`) {
		t.Error("no theme switcher")
	}
}

func TestTheScriptSwapsOnlyTheStatus(t *testing.T) {
	body := get(newServer(t, &fakeBackend{}), "/status").Body.String()
	if strings.Contains(body, "<html") {
		t.Error("the refreshed part is a whole page")
	}
	// Buttons can be redrawn, but anything a person types into can't be.
	body = get(newServer(t, &fakeBackend{status: app.Status{LogFiles: []logfile.Status{{Path: `C:\a.adi`}}}}), "/status").Body.String()
	inputs := regexp.MustCompile(`<input `).FindAllString(body, -1)
	hidden := regexp.MustCompile(`<input type="hidden"`).FindAllString(body, -1)
	if len(inputs) != len(hidden) || strings.Contains(body, "<textarea") || strings.Contains(body, "<select") {
		t.Error("the refreshed part holds a field a person could be typing in")
	}
}

func TestTheFileWindowCanBeAskedForAndAFileRemoved(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)
	form := url.Values{"form_token": {server.formToken}}

	if got := post(server, "/logfiles/choose", form, nil).Code; got != http.StatusSeeOther {
		t.Errorf("choose: %d", got)
	}
	if backend.choosing != 1 {
		t.Errorf("choosing = %d", backend.choosing)
	}

	path := `C:\Logs\wsjtx_log.adi`
	form.Set("path", path)
	if got := post(server, "/logfiles/remove", form, nil).Code; got != http.StatusSeeOther {
		t.Errorf("remove: %d", got)
	}
	if len(backend.removed) != 1 || backend.removed[0] != path {
		t.Errorf("removed = %v", backend.removed)
	}

	backend.removeErr = errors.New("disk full")
	body := html.UnescapeString(post(server, "/logfiles/remove", form, nil).Body.String())
	if !strings.Contains(body, pageWords.ActionError) {
		t.Error("no explanation")
	}
}

func TestThePageCannotNameAFileToWatch(t *testing.T) {
	body := get(newServer(t, &fakeBackend{}), "/").Body.String()
	for _, unwanted := range []string{"<textarea", `type="file"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page has %q", unwanted)
		}
	}
}

func TestLogFilesShowWhatTheyAreDoing(t *testing.T) {
	path := `C:\Logs\wsjtx_log.adi`
	files := []logfile.Status{
		{Path: path, State: logfile.Watching, Contacts: 12, Outside: 3400},
		{Path: `C:\Logs\gone.adi`, State: logfile.Missing},
	}
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: app.Status{LogFiles: files}}), "/status").Body.String())
	for _, want := range []string{
		path,
		"<td>12</td><td>3400</td>",
		pageWords.LogFilesHeading,
		pageWords.LogFileAddButton,
		pageWords.LogFileRemoveButton,
		`name="path" value="` + path + `"`,
		fileLabels[logfile.Watching],
		fileStates[logfile.Missing],
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	empty := html.UnescapeString(get(newServer(t, &fakeBackend{}), "/status").Body.String())
	if strings.Contains(empty, "<th>"+pageWords.LogFilePath) {
		t.Error("an empty list of files takes up room")
	}
	if !strings.Contains(empty, pageWords.LogFileAddButton) {
		t.Error("no way to add the first file")
	}
}

func TestTheAddButtonExplainsWhenItCannotBeUsed(t *testing.T) {
	tests := []struct {
		status app.Status
		want   string
	}{
		{app.Status{ChoosingLogFile: true}, pageWords.LogFileChoosing},
		{app.Status{LogFilesFull: true}, pageWords.LogFilesFullText},
	}
	for _, tt := range tests {
		body := html.UnescapeString(get(newServer(t, &fakeBackend{status: tt.status}), "/status").Body.String())
		if !strings.Contains(body, tt.want) || !strings.Contains(body, "disabled") {
			t.Errorf("%+v: wrong page", tt.status)
		}
	}
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: app.Status{LogFileChooseFailed: true}}), "/status").Body.String())
	if !strings.Contains(body, pageWords.LogFileChooseFailed) || strings.Contains(body, "disabled") {
		t.Error("a failed try should explain itself and let the person try again")
	}
}

func TestEveryFileStateHasWords(t *testing.T) {
	for _, state := range []logfile.State{logfile.Starting, logfile.WaitingForEvent, logfile.Watching, logfile.Missing, logfile.Unreadable, logfile.NotADIF} {
		if fileLabelText(state) == "" {
			t.Errorf("no label for %s", state)
		}
	}
}

func TestWithN3FJPOffNothingLooksForIt(t *testing.T) {
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: app.Status{N3FJPOff: true}}), "/").Body.String())
	for _, unwanted := range []string{pageWords.LoggerConnecting, pageWords.LoggerLookingPill} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page says %q", unwanted)
		}
	}
	for _, want := range []string{pageWords.LoggerOffPill, pageWords.N3FJPUseButton} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestN1MMCanBeTurnedOn(t *testing.T) {
	backend := &fakeBackend{}
	server := newServer(t, backend)
	form := url.Values{"form_token": {server.formToken}, "enabled": {"on"}, "on_network": {"on"}}
	if got := post(server, "/n1mm", form, nil).Code; got != http.StatusSeeOther {
		t.Errorf("got %d", got)
	}
	if !backend.n1mmOn || !backend.n1mmLAN {
		t.Errorf("backend = %+v", backend)
	}
	backend.n1mmErr = errors.New("disk full")
	body := html.UnescapeString(post(server, "/n1mm", url.Values{"form_token": {server.formToken}}, nil).Body.String())
	if !strings.Contains(body, pageWords.ActionError) || backend.n1mmOn {
		t.Error("no explanation, or the setting wasn't passed on")
	}
}

func TestN1MMShowsWhatItIsDoing(t *testing.T) {
	status := func(s app.Status) string {
		return html.UnescapeString(get(newServer(t, &fakeBackend{status: s}), "/").Body.String())
	}

	off := status(app.Status{})
	for _, unwanted := range []string{pageWords.N1MMListeningPill, pageWords.N1MMBlocked} {
		if strings.Contains(off, unwanted) {
			t.Errorf("with N1MM off, the page says %q", unwanted)
		}
	}
	if !strings.Contains(off, pageWords.N1MMEnableLabel) || strings.Contains(off, `name="enabled" data-autosave checked`) {
		t.Error("no way to turn it on, or it starts on")
	}

	blocked := status(app.Status{N1MMEnabled: true})
	for _, want := range []string{pageWords.N1MMBlocked, pageWords.N1MMBlockedPill, `name="enabled" data-autosave checked`} {
		if !strings.Contains(blocked, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	listening := status(app.Status{N1MMEnabled: true, N1MMOnNetwork: true, N1MM: n1mm.Status{Listening: true, Contacts: 7, Dropped: 2}})
	for _, want := range []string{pageWords.N1MMListeningPill, pageWords.N1MMContacts + ": 7", fmt.Sprintf(pageWords.N1MMDropped, 2), `name="on_network" data-autosave checked`} {
		if !strings.Contains(listening, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestFilesShowTheirNameFirst(t *testing.T) {
	for path, want := range map[string][2]string{
		`C:\Users\Ann\AppData\Local\WSJT-X\wsjtx_log.adi`: {"wsjtx_log.adi", `C:\Users\Ann\AppData\Local\WSJT-X\`},
		"/home/ann/log.adi": {"log.adi", "/home/ann/"},
		"bare.adi":          {"bare.adi", ""},
	} {
		if got := fileName(path); got != want[0] {
			t.Errorf("%s: name %q", path, got)
		}
		if got := fileFolder(path); got != want[1] {
			t.Errorf("%s: folder %q", path, got)
		}
	}
}

func TestLongHelpStaysBehindTheHelpButton(t *testing.T) {
	status := stationStatus("GOTA", true, "GOTA")
	status.ServerRole = true
	status.N1MMEnabled = true
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: status}), "/").Body.String())
	for _, help := range []string{
		pageWords.StationHelp, pageWords.StationOverrideHelp, pageWords.StationNewHelp, pageWords.ForgetHelp,
		pageWords.LogFilesHelp, pageWords.LogFilesLimits, pageWords.RoleHelp, pageWords.ListenHelp,
		pageWords.N1MMHelp, pageWords.N1MMLimits, pageWords.N1MMNetworkHelp,
	} {
		at := strings.Index(body, help)
		if at < 0 {
			t.Errorf("page lacks %q", help)
			continue
		}
		if opened, closed := strings.LastIndex(body[:at], "<details"), strings.LastIndex(body[:at], "</details>"); opened < closed || opened < 0 {
			t.Errorf("%q isn't behind a help button", help)
		}
	}
}

func TestN1MMSaysLittleUntilItIsOn(t *testing.T) {
	off := html.UnescapeString(get(newServer(t, &fakeBackend{}), "/").Body.String())
	for _, unwanted := range []string{pageWords.N1MMHelp, pageWords.N1MMNetworkLabel, pageWords.N1MMNetworkHelp} {
		if strings.Contains(off, unwanted) {
			t.Errorf("with N1MM off, the page says %q", unwanted)
		}
	}
	on := html.UnescapeString(get(newServer(t, &fakeBackend{status: app.Status{N1MMEnabled: true}}), "/").Body.String())
	for _, want := range []string{pageWords.N1MMHelp, pageWords.N1MMNetworkLabel, pageWords.N1MMNetworkHelp, pageWords.N1MMLimits} {
		if !strings.Contains(on, want) {
			t.Errorf("with N1MM on, the page lacks %q", want)
		}
	}
	// The warning has to be readable before the box is ticked, since ticking applies at once.
	if at := strings.Index(on, pageWords.N1MMNetworkWarning); at < 0 || strings.LastIndex(on[:at], "<details") > strings.LastIndex(on[:at], "</details>") {
		t.Error("the network warning should be on the page without opening help")
	}
}

func TestChoicesSaveThemselvesButATypedAddressKeepsItsButton(t *testing.T) {
	client := get(newServer(t, &fakeBackend{}), "/").Body.String()
	if !strings.Contains(client, `name="address"`) || !strings.Contains(client, `value="client" data-autosave checked`) || !strings.Contains(client, `name="enabled" data-autosave`) {
		t.Error("the client role's choices aren't set to save")
	}
	if got := strings.Count(client, `action="/n1mm"`); got != 1 {
		t.Errorf("%d N1MM forms", got)
	}
	n1mmForm := client[strings.Index(client, `action="/n1mm"`):]
	n1mmForm = n1mmForm[:strings.Index(n1mmForm, "</form>")]
	if strings.Contains(n1mmForm, "<button") {
		t.Error("the N1MM form still has a save button")
	}
	addressForm := client[strings.Index(client, `action="/n3fjp"`):]
	addressForm = addressForm[:strings.Index(addressForm, "</form>")]
	if !strings.Contains(addressForm, "<button") {
		t.Error("a typed address has no way to be saved")
	}

	server := get(newServer(t, &fakeBackend{status: app.Status{ServerRole: true, LoggerAddress: "127.0.0.1:1000"}}), "/").Body.String()
	if !strings.Contains(server, `name="listen_on_network" data-autosave`) || !strings.Contains(server, `type="hidden" name="address" value="127.0.0.1:1000"`) {
		t.Error("the server role loses the client address or its network choice doesn't save")
	}
}

func TestN3FJPCanBeTurnedBackOn(t *testing.T) {
	body := html.UnescapeString(get(newServer(t, &fakeBackend{status: app.Status{N3FJPOff: true, LoggerAddress: "192.168.1.5:1000"}}), "/").Body.String())
	for _, want := range []string{pageWords.N3FJPOffText, pageWords.N3FJPUseButton, `name="role" value="client"`, `name="address" value="192.168.1.5:1000"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, pageWords.RoleHeading) {
		t.Error("the role list takes up room while N3FJP is off")
	}
}

func TestTheServerComesBeforeLogFiles(t *testing.T) {
	for name, status := range map[string]app.Status{
		"not connected": {},
		"connected":     {Enrolled: true, ServerURL: "https://logs.example.org"},
	} {
		body := html.UnescapeString(get(newServer(t, &fakeBackend{status: status}), "/").Body.String())
		heading := pageWords.ConnectHeading
		if status.Enrolled {
			heading = pageWords.ServerSetupHeading
		}
		card := func(title string) int { return strings.Index(body, ">"+title+"</h2>") }
		strip, server, files, n3fjpCard := strings.Index(body, pageWords.Waiting), card(heading), card(pageWords.LogFilesHeading), card(pageWords.SetupHeading)
		if !(strip < server && server < files && files < n3fjpCard) {
			t.Errorf("%s: status %d, server %d, files %d, N3FJP %d", name, strip, server, files, n3fjpCard)
		}
	}
}

func TestTheRefreshCoversBothPartsAndNotTheServerForm(t *testing.T) {
	body := get(newServer(t, &fakeBackend{}), "/status").Body.String()
	for _, want := range []string{`<div id="status">`, `<div id="log-files">`} {
		if !strings.Contains(body, want) {
			t.Errorf("refresh lacks %q", want)
		}
	}
	if strings.Contains(body, `name="code"`) || strings.Contains(body, `name="server_url"`) {
		t.Error("refreshing would wipe a half-typed enrollment code")
	}
}
