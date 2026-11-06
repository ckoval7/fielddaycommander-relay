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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/secret"
	"github.com/ckoval7/fielddaycommander-relay/internal/settings"
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
	default:
		w.WriteHeader(http.StatusNotFound)
	}
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
		if err := h.app.SetLoggerAddress(address); !errors.Is(err, ErrBadLoggerAddress) {
			t.Errorf("%q: err = %v", address, err)
		}
	}

	if err := h.app.SetLoggerAddress("192.168.1.5"); err != nil {
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
