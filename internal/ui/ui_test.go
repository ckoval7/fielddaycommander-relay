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
	role      string
	listen    bool
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
		for _, path := range []string{"/forget", "/enroll", "/n3fjp"} {
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
	for _, path := range []string{"/forget", "/enroll", "/n3fjp"} {
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
	for _, want := range []string{pageWords.ServerReadyPill, pageWords.ServerClients + ": 3", pageWords.ServerRecords + ": 12", `name="listen_on_network"`, `value="server" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !regexp.MustCompile(`data-role="client" hidden`).MatchString(body) || regexp.MustCompile(`data-role="server" hidden`).MatchString(body) {
		t.Error("only the server role's settings should show")
	}
}

func TestOtherPCsCanOnlyConnectInTheServerRole(t *testing.T) {
	checkbox := regexp.MustCompile(`<input type="checkbox"[^>]*name="listen_on_network"[^>]*>`)

	client := checkbox.FindString(get(newServer(t, &fakeBackend{}), "/").Body.String())
	if !strings.Contains(client, " disabled") {
		t.Errorf("client role: %s", client)
	}
	server := checkbox.FindString(get(newServer(t, &fakeBackend{status: app.Status{ServerRole: true}}), "/").Body.String())
	if server == "" || strings.Contains(server, " disabled") {
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
	if regexp.MustCompile(`<form|<html`).MatchString(body) {
		t.Error("the refreshed part must leave the forms alone")
	}
}
