// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/logfile"
	"github.com/ckoval7/fielddaycommander-relay/internal/secret"
	"github.com/ckoval7/fielddaycommander-relay/internal/settings"
	"github.com/ckoval7/fielddaycommander-relay/internal/udplog"
)

// memoryStore stands in for the keychain.
type memoryStore struct {
	mu     sync.Mutex
	value  string
	setErr error
}

func (m *memoryStore) Get() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.value == "" {
		return "", secret.ErrNotFound
	}
	return m.value, nil
}

func (m *memoryStore) Set(value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setErr != nil {
		return m.setErr
	}
	m.value = value
	return nil
}

func (m *memoryStore) Delete() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value = ""
	return nil
}

// fakeServer answers the relay API.
type fakeServer struct {
	mu          sync.Mutex
	enrollments int
	authSeen    []string
	enroll      func(w http.ResponseWriter)
	received    []string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/relay/v1/enroll":
		f.enrollments++
		if f.enroll != nil {
			f.enroll(w)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"fdca_secret_token","relay":{"id":1,"name":"Phone Tent"}}`))
	case "/api/relay/v1/handshake":
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server_version": "1", "supported_api_versions": []int{1}, "minimum_relay_version": nil,
			"server_time": time.Now().UTC().Format(time.RFC3339), "relay": map[string]any{"id": 1, "name": "Phone Tent"},
			"event":       map[string]any{"name": "Field Day", "starts_at": "2026-06-27T18:00:00Z", "ends_at": "2026-06-28T21:00:00Z"},
			"permissions": map[string]any{"create_stations": true}, "stations": []any{},
		})
	case "/api/relay/v1/contacts":
		var body struct {
			Contacts []struct {
				Callsign string `json:"callsign"`
			} `json:"contacts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		results := []map[string]any{}
		for i, c := range body.Contacts {
			f.received = append(f.received, c.Callsign)
			results = append(results, map[string]any{"index": i, "status": "accepted", "contact_uuid": "uuid-" + c.Callsign})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeServer) contacts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func (f *fakeServer) seenAuth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authSeen...)
}

type harness struct {
	app   *App
	store *memoryStore
	dir   string
}

func start(t *testing.T, dir string, store *memoryStore) *harness {
	t.Helper()
	a, err := New(Config{DataDir: dir, Version: "26.11.1", Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(stop)

	eventually(t, "the app to start", func() bool {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.source != nil
	})
	return &harness{app: a, store: store, dir: dir}
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func TestAnUnenrolledRelayStillRuns(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	status, err := h.app.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Enrolled || status.ServerURL != "" || status.Version != "26.11.1" || status.LoggerAddress != "127.0.0.1:1000" {
		t.Errorf("status = %+v", status)
	}
}

func TestEnrollingStoresTheTokenInTheKeychainOnly(t *testing.T) {
	server := &fakeServer{}
	api := httptest.NewServer(server)
	defer api.Close()

	h := start(t, t.TempDir(), &memoryStore{})
	if err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL + "/", Code: "ABCD-EFGH-JKMN"}); err != nil {
		t.Fatal(err)
	}

	if h.store.value != "fdca_secret_token" {
		t.Errorf("keychain has %q", h.store.value)
	}
	eventually(t, "the handshake", func() bool { return len(server.seenAuth()) > 0 })
	if got := server.seenAuth()[0]; got != "Bearer fdca_secret_token" {
		t.Errorf("auth = %q", got)
	}

	status, err := h.app.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enrolled || status.RelayName != "Phone Tent" || status.ServerURL != api.URL {
		t.Errorf("status = %+v", status)
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(h.dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "fdca_secret") {
			t.Errorf("the token is in %s", entry.Name())
		}
	}
}

func TestEnrollmentSurvivesARestart(t *testing.T) {
	server := &fakeServer{}
	api := httptest.NewServer(server)
	defer api.Close()

	dir := t.TempDir()
	store := &memoryStore{}
	if err := settings.Save(dir, settings.Settings{ServerURL: api.URL, RelayName: "Phone Tent"}); err != nil {
		t.Fatal(err)
	}
	store.value = "fdca_saved"

	h := start(t, dir, store)
	eventually(t, "the handshake", func() bool { return len(server.seenAuth()) > 0 })
	if status, _ := h.app.Status(context.Background()); !status.Enrolled {
		t.Errorf("status = %+v", status)
	}
}

func TestEnrollFailuresAreNamed(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   EnrollFailure
	}{
		{"used code", 422, `{"code":"invalid_code","message":"no"}`, FailBadCode},
		{"too many tries", 429, ``, FailRateLimited},
		{"plain http", 403, `{"code":"https_required","message":"no"}`, FailHTTPSRequired},
		{"server trouble", 500, ``, FailRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &fakeServer{enroll: func(w http.ResponseWriter) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}}
			api := httptest.NewServer(server)
			defer api.Close()

			h := start(t, t.TempDir(), &memoryStore{})
			err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL, Code: "X"})

			var enrollErr *EnrollError
			if !errors.As(err, &enrollErr) || enrollErr.Failure != tt.want {
				t.Errorf("err = %v", err)
			}
			if h.store.value != "" {
				t.Error("a failed enrollment left a token")
			}
		})
	}
}

func TestABadAddressAndAnUnreachableServerAreNamed(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	tests := map[string]EnrollFailure{
		"http://logs.example.org": FailBadAddress,
		"":                        FailBadAddress,
		"https://127.0.0.1:1":     FailUnreachable,
	}
	for address, want := range tests {
		err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: address, Code: "X"})
		var enrollErr *EnrollError
		if !errors.As(err, &enrollErr) || enrollErr.Failure != want {
			t.Errorf("%q: err = %v", address, err)
		}
	}
}

// A code works once, so a broken keychain has to be found before it's spent.
func TestABrokenKeychainDoesNotSpendTheCode(t *testing.T) {
	server := &fakeServer{}
	api := httptest.NewServer(server)
	defer api.Close()

	h := start(t, t.TempDir(), &memoryStore{setErr: secret.ErrUnavailable})
	err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL, Code: "X"})

	var enrollErr *EnrollError
	if !errors.As(err, &enrollErr) || enrollErr.Failure != FailKeychain {
		t.Fatalf("err = %v", err)
	}
	if server.enrollments != 0 {
		t.Error("the code was sent")
	}
}

func TestASelfSignedServerNeedsAPersonToAcceptItsKey(t *testing.T) {
	server := &fakeServer{}
	api := httptest.NewTLSServer(server)
	defer api.Close()

	h := start(t, t.TempDir(), &memoryStore{})
	err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL, Code: "X"})

	var enrollErr *EnrollError
	if !errors.As(err, &enrollErr) || enrollErr.Failure != FailUntrusted || len(enrollErr.Fingerprint) != 64 {
		t.Fatalf("err = %v", err)
	}
	if server.enrollments != 0 {
		t.Fatal("the code went to a server nobody has accepted")
	}

	if err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL, Code: "X", PinSHA256: enrollErr.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	status, err := h.app.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enrolled || !status.Pinned {
		t.Errorf("status = %+v", status)
	}
	eventually(t, "the handshake over the pinned connection", func() bool { return len(server.seenAuth()) > 0 })

	wrong := strings.Repeat("ab", 32)
	err = h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL, Code: "X", PinSHA256: wrong})
	if !errors.As(err, &enrollErr) || enrollErr.Failure != FailPinMismatch {
		t.Errorf("err = %v", err)
	}
}

func TestForgetRemovesTheTokenAndServer(t *testing.T) {
	server := &fakeServer{}
	api := httptest.NewServer(server)
	defer api.Close()

	h := start(t, t.TempDir(), &memoryStore{})
	if err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: api.URL, Code: "X"}); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Forget(context.Background()); err != nil {
		t.Fatal(err)
	}

	if h.store.value != "" {
		t.Error("the token is still there")
	}
	status, err := h.app.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Enrolled || status.ServerURL != "" {
		t.Errorf("status = %+v", status)
	}
	saved, err := settings.Load(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ServerURL != "" {
		t.Errorf("saved = %+v", saved)
	}
}

func TestLoggerAddress(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})

	good := map[string]string{
		"192.168.1.5":       "192.168.1.5:1000",
		" 192.168.1.5:1100": "192.168.1.5:1100",
		"LOG-PC":            "LOG-PC:1000",
		"[fe80::1]:1000":    "[fe80::1]:1000",
		"":                  "",
	}
	for address, want := range good {
		got, err := normalizeLoggerAddress(address)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", address, got, err)
		}
	}
	for _, address := range []string{"host:99999", "host:abc", "a b", "http://host/", ":1000"} {
		if err := h.app.SetLogger(RoleClient, address, false); !errors.Is(err, ErrBadLoggerAddress) {
			t.Errorf("%q: err = %v", address, err)
		}
	}

	if err := h.app.SetLogger(RoleClient, "192.168.1.5", false); err != nil {
		t.Fatal(err)
	}
	status, err := h.app.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.LoggerAddress != "192.168.1.5:1000" {
		t.Errorf("status = %+v", status)
	}
	saved, _ := settings.Load(h.dir)
	if saved.LoggerAddress != "192.168.1.5:1000" {
		t.Errorf("saved = %+v", saved)
	}
}

func TestTheRoleCanChangeAndIsRemembered(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	if err := h.app.SetLogger("nonsense", "", false); !errors.Is(err, ErrBadRole) {
		t.Errorf("err = %v", err)
	}
	if err := h.app.SetLogger(RoleServer, "", true); err != nil {
		t.Fatal(err)
	}
	status, err := h.app.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ServerRole || !status.ListenOnNetwork {
		t.Errorf("status = %+v", status)
	}
	saved, _ := settings.Load(h.dir)
	if saved.LoggerRole != RoleServer || !saved.ListenOnNetwork {
		t.Errorf("saved = %+v", saved)
	}

	if err := h.app.SetLogger(RoleClient, "", true); err != nil {
		t.Fatal(err)
	}
	status, _ = h.app.Status(context.Background())
	if status.ServerRole || status.ListenOnNetwork {
		t.Errorf("a client has nothing to listen for: %+v", status)
	}
}

func TestChangingRoleStartsTheFeedOverButOnlyWhenItChanges(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	ctx := context.Background()
	set := func() {
		t.Helper()
		if err := h.app.queue.SetMeta(ctx, "feed_cursor", "x"); err != nil {
			t.Fatal(err)
		}
	}
	cursor := func() string {
		t.Helper()
		value, err := h.app.queue.Meta(ctx, "feed_cursor")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	set()
	if err := h.app.SetLogger(RoleServer, "", false); err != nil {
		t.Fatal(err)
	}
	if cursor() != "" {
		t.Error("becoming the server has to fill its log from the start")
	}

	set()
	if err := h.app.SetLogger(RoleServer, "", true); err != nil {
		t.Fatal(err)
	}
	if cursor() != "x" {
		t.Error("the same role with another setting shouldn't start over")
	}
}

// Saving the form unchanged mustn't drop every connected N3FJP PC.
func TestSavingWhatIsAlreadySetRestartsNothing(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	if err := h.app.SetLogger(RoleClient, "192.168.1.5", false); err != nil {
		t.Fatal(err)
	}
	h.app.mu.RLock()
	before := h.app.source
	h.app.mu.RUnlock()

	if err := h.app.SetLogger(RoleClient, " 192.168.1.5:1000 ", false); err != nil {
		t.Fatal(err)
	}
	h.app.mu.RLock()
	after := h.app.source
	h.app.mu.RUnlock()
	if before != after {
		t.Error("an unchanged save restarted the connection")
	}
}

func TestServerAddressesAreTidied(t *testing.T) {
	tests := map[string]string{
		" logs.example.org/ ":      "https://logs.example.org",
		"https://logs.example.org": "https://logs.example.org",
		"http://localhost:8000/":   "http://localhost:8000",
		"":                         "",
	}
	for raw, want := range tests {
		if got := normalizeServerURL(raw); got != want {
			t.Errorf("%q: got %q", raw, got)
		}
	}
}

const logFileText = "<ADIF_VER:5>3.1.4<EOH>\n" +
	"<CALL:5>K1ABC<QSO_DATE:8>20260627<TIME_ON:4>1900<BAND:3>20m<MODE:3>FT8<EOR>\n" +
	"<CALL:5>W2OLD<QSO_DATE:8>20190101<TIME_ON:4>1900<BAND:3>20m<MODE:3>FT8<EOR>\n"

func TestALogFileReachesTheServerWithN3FJPOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wsjtx_log.adi")
	if err := os.WriteFile(path, []byte(logFileText), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &fakeServer{}
	backend := httptest.NewServer(server)
	defer backend.Close()

	dir := t.TempDir()
	h := start(t, dir, &memoryStore{})
	if err := h.app.SetLogger(RoleOff, "", false); err != nil {
		t.Fatal(err)
	}
	h.choose(t, path)

	// Before connecting, the relay doesn't know which contacts belong to the event.
	eventually(t, "the watcher to wait", func() bool {
		status, _ := h.app.Status(context.Background())
		return len(status.LogFiles) == 1 && status.LogFiles[0].State == logfile.WaitingForEvent
	})
	status, _ := h.app.Status(context.Background())
	if status.Counts.Pending != 0 || !status.N3FJPOff {
		t.Errorf("status = %+v", status)
	}

	if err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: backend.URL, Code: "ABCD"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the contact to arrive", func() bool { return len(server.contacts()) > 0 })
	if got := server.contacts(); len(got) != 1 || got[0] != "K1ABC" {
		t.Errorf("server received %v", got)
	}
	eventually(t, "the count of old contacts", func() bool {
		status, _ := h.app.Status(context.Background())
		return status.LogFiles[0].Outside == 1 && status.LogFiles[0].Contacts == 1
	})
	h.app.mu.RLock()
	defer h.app.mu.RUnlock()
	if h.app.source != nil || h.app.server != nil || h.app.feed != nil {
		t.Error("something is still looking for N3FJP")
	}
}

func TestDisconnectingForgetsTheEventsDatesButChangingRoleKeepsThem(t *testing.T) {
	server := &fakeServer{}
	backend := httptest.NewServer(server)
	defer backend.Close()
	h := start(t, t.TempDir(), &memoryStore{})
	if err := h.app.Enroll(context.Background(), EnrollRequest{ServerURL: backend.URL, Code: "ABCD"}); err != nil {
		t.Fatal(err)
	}
	known := func() bool {
		_, _, found, err := h.app.queue.EventPeriod(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	eventually(t, "the event's dates", known)

	// Stay offline: no handshake can bring the dates back during this check.
	backend.Close()
	if err := h.app.SetLogger(RoleOff, "", false); err != nil {
		t.Fatal(err)
	}
	if !known() {
		t.Error("changing role forgot the dates")
	}
	if err := h.app.Forget(context.Background()); err != nil {
		t.Fatal(err)
	}
	if known() {
		t.Error("disconnecting kept the dates")
	}
}

func TestChangingLogFilesLeavesN3FJPAlone(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	h.app.mu.RLock()
	before := h.app.source
	h.app.mu.RUnlock()

	first := filepath.Join(t.TempDir(), "a.adi")
	h.choose(t, first)
	h.app.mu.RLock()
	after := h.app.source
	h.app.mu.RUnlock()
	if before != after {
		t.Error("N3FJP was restarted")
	}

	saved, _ := settings.Load(h.dir)
	if len(saved.LogFiles) != 1 || saved.LogFiles[0] != first {
		t.Errorf("saved = %+v", saved)
	}
	status, _ := h.app.Status(context.Background())
	if len(status.LogFiles) != 1 || status.LogFiles[0].Path != first {
		t.Errorf("status = %+v", status.LogFiles)
	}

	if err := h.app.RemoveLogFile(`not on the list`); err != nil {
		t.Fatal(err)
	}
	if status, _ = h.app.Status(context.Background()); len(status.LogFiles) != 1 {
		t.Errorf("removed the wrong file: %+v", status.LogFiles)
	}
	if err := h.app.RemoveLogFile(first); err != nil {
		t.Fatal(err)
	}
	if status, _ = h.app.Status(context.Background()); len(status.LogFiles) != 0 {
		t.Errorf("still watching %+v", status.LogFiles)
	}
	if saved, _ = settings.Load(h.dir); len(saved.LogFiles) != 0 {
		t.Errorf("saved = %+v", saved)
	}
}

// choose goes through the file window as if the person chose path.
func (h *harness) choose(t *testing.T, path string) {
	t.Helper()
	h.app.mu.Lock()
	h.app.cfg.PickLogFile = func() (string, error) { return path, nil }
	h.app.mu.Unlock()
	h.app.ChooseLogFile()
	eventually(t, "the choice to finish", func() bool {
		status, _ := h.app.Status(context.Background())
		return !status.ChoosingLogFile
	})
}

func TestChoosingTheSameFileTwiceAddsItOnce(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	path := filepath.Join(t.TempDir(), "a.adi")
	h.choose(t, path)
	h.choose(t, path)
	if status, _ := h.app.Status(context.Background()); len(status.LogFiles) != 1 {
		t.Errorf("status = %+v", status.LogFiles)
	}
}

func TestChangingYourMindChangesNothing(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	h.choose(t, "")
	status, _ := h.app.Status(context.Background())
	if len(status.LogFiles) != 0 || status.LogFileChooseFailed {
		t.Errorf("status = %+v", status)
	}
}

func TestOnlyOneFileWindowShowsAtATime(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	opened := make(chan struct{}, 8)
	answer := make(chan string)
	h.app.mu.Lock()
	h.app.cfg.PickLogFile = func() (string, error) {
		opened <- struct{}{}
		return <-answer, nil
	}
	h.app.mu.Unlock()

	h.app.ChooseLogFile()
	h.app.ChooseLogFile()
	<-opened
	status, _ := h.app.Status(context.Background())
	if !status.ChoosingLogFile {
		t.Error("the page doesn't know the window is showing")
	}
	answer <- ""
	eventually(t, "the window to close", func() bool {
		status, _ := h.app.Status(context.Background())
		return !status.ChoosingLogFile
	})
	if len(opened) != 0 {
		t.Error("a second window opened")
	}
}

func TestAFailedFileWindowIsReportedAndCanBeRetried(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	h.app.mu.Lock()
	h.app.cfg.PickLogFile = func() (string, error) { return "", errors.New("no window") }
	h.app.mu.Unlock()
	h.app.ChooseLogFile()
	eventually(t, "the failure", func() bool {
		status, _ := h.app.Status(context.Background())
		return status.LogFileChooseFailed
	})

	path := filepath.Join(t.TempDir(), "a.adi")
	h.choose(t, path)
	status, _ := h.app.Status(context.Background())
	if status.LogFileChooseFailed || len(status.LogFiles) != 1 {
		t.Errorf("status = %+v", status)
	}
}

func TestTheFileListHasALimit(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	dir := t.TempDir()
	for i := range maxLogFiles {
		h.choose(t, filepath.Join(dir, strconv.Itoa(i)))
	}
	status, _ := h.app.Status(context.Background())
	if !status.LogFilesFull || len(status.LogFiles) != maxLogFiles {
		t.Errorf("%d files, full %v", len(status.LogFiles), status.LogFilesFull)
	}
	h.choose(t, filepath.Join(dir, "one too many"))
	if status, _ = h.app.Status(context.Background()); len(status.LogFiles) != maxLogFiles {
		t.Errorf("%d files", len(status.LogFiles))
	}
}

func TestChoosingWithNoFileWindowIsReported(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	h.app.ChooseLogFile()
	eventually(t, "the failure", func() bool {
		status, _ := h.app.Status(context.Background())
		return status.LogFileChooseFailed
	})
}

func TestN1MMStartsOnlyWhenAskedAndSurvivesOtherChanges(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	status, _ := h.app.Status(context.Background())
	if status.N1MMEnabled || status.N1MM.Listening {
		t.Errorf("listening without being asked: %+v", status.N1MM)
	}

	// The network setting means nothing while N1MM is off.
	if err := h.app.SetN1MM(false, true); err != nil {
		t.Fatal(err)
	}
	if saved, _ := settings.Load(h.dir); saved.N1MMOnNetwork {
		t.Errorf("saved = %+v", saved)
	}

	if err := h.app.SetN1MM(true, false); err != nil {
		t.Fatal(err)
	}
	if saved, _ := settings.Load(h.dir); !saved.N1MMEnabled || saved.N1MMOnNetwork {
		t.Errorf("saved = %+v", saved)
	}
	h.app.mu.RLock()
	listener := h.app.n1mm
	h.app.mu.RUnlock()
	if listener == nil {
		t.Fatal("not started")
	}

	// A role change restarts N3FJP but must not close N1MM's port.
	if err := h.app.SetLogger(RoleOff, "", false); err != nil {
		t.Fatal(err)
	}
	h.app.mu.RLock()
	after := h.app.n1mm
	h.app.mu.RUnlock()
	if after != listener {
		t.Error("N1MM was restarted")
	}

	if err := h.app.SetN1MM(true, false); err != nil {
		t.Fatal(err)
	}
	h.app.mu.RLock()
	after = h.app.n1mm
	h.app.mu.RUnlock()
	if after != listener {
		t.Error("saving the same settings restarted N1MM")
	}

	if err := h.app.SetN1MM(false, true); err != nil {
		t.Fatal(err)
	}
	status, _ = h.app.Status(context.Background())
	if status.N1MMEnabled || status.N1MMOnNetwork || status.N1MM.Listening {
		t.Errorf("status = %+v", status)
	}
}

func TestUDPLoggersStartOnlyWhenAskedAndOnlyEachOther(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	listener := func(kind udplog.Kind) *udplog.Listener {
		h.app.mu.RLock()
		defer h.app.mu.RUnlock()
		return h.app.udp[kind].listener
	}
	if listener(udplog.WSJTX) != nil || listener(udplog.ADIF) != nil {
		t.Fatal("listening without being asked")
	}

	// The network setting means nothing while the listener is off.
	if err := h.app.SetUDPLogger(udplog.WSJTX, false, true, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.app.SetUDPLogger(udplog.WSJTX, true, false, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.app.SetUDPLogger(udplog.ADIF, true, true, 0); err != nil {
		t.Fatal(err)
	}
	saved, _ := settings.Load(h.dir)
	if !saved.WSJTXEnabled || saved.WSJTXOnNetwork || !saved.UDPADIFEnabled || !saved.UDPADIFOnNetwork {
		t.Errorf("saved = %+v", saved)
	}
	wsjtx := listener(udplog.WSJTX)
	if wsjtx == nil || listener(udplog.ADIF) == nil {
		t.Fatal("not started")
	}

	// Other changes, and the other listener's, must not close this one's port.
	if err := h.app.SetLogger(RoleOff, "", false); err != nil {
		t.Fatal(err)
	}
	if err := h.app.SetUDPLogger(udplog.ADIF, true, false, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.app.SetUDPLogger(udplog.WSJTX, true, false, 0); err != nil {
		t.Fatal(err)
	}
	if listener(udplog.WSJTX) != wsjtx {
		t.Error("WSJT-X was restarted")
	}

	if err := h.app.SetUDPLogger(udplog.WSJTX, false, false, 0); err != nil {
		t.Fatal(err)
	}
	status, _ := h.app.Status(context.Background())
	if status.WSJTX.Enabled || status.WSJTX.Listening || !status.UDPADIF.Enabled || status.UDPADIF.OnNetwork {
		t.Errorf("status = %+v %+v", status.WSJTX, status.UDPADIF)
	}
}

func TestUDPLoggerPorts(t *testing.T) {
	h := start(t, t.TempDir(), &memoryStore{})
	port := func(kind udplog.Kind) int {
		status, _ := h.app.Status(context.Background())
		if kind == udplog.WSJTX {
			return status.WSJTX.Port
		}
		return status.UDPADIF.Port
	}
	if port(udplog.WSJTX) != 2237 || port(udplog.ADIF) != 2238 {
		t.Fatalf("defaults are %d and %d", port(udplog.WSJTX), port(udplog.ADIF))
	}

	for _, bad := range []int{-1, 65536} {
		if err := h.app.SetUDPLogger(udplog.WSJTX, true, false, bad); !errors.Is(err, ErrBadPort) {
			t.Errorf("port %d: %v", bad, err)
		}
	}
	for _, taken := range []int{2238, 12060} {
		if err := h.app.SetUDPLogger(udplog.WSJTX, true, false, taken); !errors.Is(err, ErrPortTaken) {
			t.Errorf("port %d: %v", taken, err)
		}
	}

	if err := h.app.SetUDPLogger(udplog.WSJTX, true, false, 2333); err != nil {
		t.Fatal(err)
	}
	h.app.mu.RLock()
	listener := h.app.udp[udplog.WSJTX].listener
	h.app.mu.RUnlock()
	// Turning it off and on keeps the port it was given.
	if err := h.app.SetUDPLogger(udplog.WSJTX, false, false, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.app.SetUDPLogger(udplog.WSJTX, true, false, 0); err != nil {
		t.Fatal(err)
	}
	if saved, _ := settings.Load(h.dir); saved.WSJTXPort != 2333 || port(udplog.WSJTX) != 2333 || listener == nil {
		t.Errorf("saved port %d, status port %d", saved.WSJTXPort, port(udplog.WSJTX))
	}
	// The other listener can't take it now, but can have the default WSJT-X gave up.
	if err := h.app.SetUDPLogger(udplog.ADIF, false, false, 2333); !errors.Is(err, ErrPortTaken) {
		t.Errorf("shared port: %v", err)
	}
	if err := h.app.SetUDPLogger(udplog.ADIF, false, false, 2237); err != nil {
		t.Errorf("free port: %v", err)
	}
	// Picking the default again saves it as the default.
	if err := h.app.SetUDPLogger(udplog.ADIF, false, false, 2238); err != nil {
		t.Fatal(err)
	}
	if saved, _ := settings.Load(h.dir); saved.UDPADIFPort != 0 {
		t.Errorf("the default was saved as %d", saved.UDPADIFPort)
	}
}

// Changing a setting while nothing is running saves it and starts nothing, so a
// page still open as the relay closes can't leave a listener running.
func TestNothingStartsBeforeRunOrAfterItEnds(t *testing.T) {
	a, err := New(Config{DataDir: t.TempDir(), Version: "26.11.1", Store: &memoryStore{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	listening := func() bool {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.n1mmCancel != nil
	}

	if err := a.SetN1MM(true, false); err != nil {
		t.Fatal(err)
	}
	if listening() {
		t.Error("N1MM started before Run")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	eventually(t, "N1MM to start", listening)

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := a.SetN1MM(false, false); err != nil {
		t.Fatal(err)
	}
	if err := a.SetN1MM(true, false); err != nil {
		t.Fatal(err)
	}
	if listening() {
		t.Error("N1MM started after Run ended")
	}
}
