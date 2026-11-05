// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

func TestServerAddressMustBeSafe(t *testing.T) {
	tests := map[string]bool{
		"https://logs.example.org":      true,
		"https://logs.example.org/fdc/": true,
		"http://localhost:8080":         true,
		"http://127.0.0.1":              true,
		"http://[::1]:8080":             true,
		"http://logs.example.org":       false,
		"http://192.168.1.10":           false,
		"http://localhost.evil.example": false,
		"ftp://logs.example.org":        false,
		"logs.example.org":              false,
		"":                              false,
	}
	for address, ok := range tests {
		if _, err := New(Config{ServerURL: address}); (err == nil) != ok {
			t.Errorf("%q: err = %v", address, err)
		}
	}
}

func TestTokenNeverPrints(t *testing.T) {
	token := Token("fdca_secret")
	for _, printed := range []string{
		fmt.Sprint(token), fmt.Sprintf("%v %s %+v %#v", token, token, token, token),
		fmt.Sprintf("%+v", Enrollment{Token: token}), fmt.Sprintf("%#v", Enrollment{Token: token}),
	} {
		if strings.Contains(printed, "secret") {
			t.Errorf("token leaked: %s", printed)
		}
	}
}

func TestSendContactsSendsTokenAndVersionAndReadsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/relay/v1/contacts" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("X-Relay-Version") != "26.11.1" {
			t.Errorf("headers = %v", r.Header)
		}
		var body struct {
			Contacts []map[string]any `json:"contacts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Contacts) != 2 || body.Contacts[0]["callsign"] != "W1AW" {
			t.Errorf("body = %+v", body)
		}
		if _, sent := body.Contacts[0]["band"]; sent {
			t.Error("an empty field should be left out")
		}
		_, _ = w.Write([]byte(`{"results":[{"index":0,"status":"accepted","contact_uuid":"u"},{"index":9,"status":"accepted"},{"index":-1,"status":"accepted"}],"added_later":true}`))
	}))
	defer server.Close()

	client, err := New(Config{ServerURL: server.URL, Token: "token", Version: "26.11.1"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := client.SendContacts(context.Background(), []contact.Contact{{Callsign: "W1AW"}, {Callsign: "K1ABC"}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != StatusAccepted || results[0].ContactUUID != "u" {
		t.Errorf("results = %+v", results)
	}
	if _, reported := results[1]; reported || len(results) != 1 {
		t.Errorf("only the contact the server reported on should have a result: %+v", results)
	}
}

func TestStatusErrorsCarryCodeAndRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"slow_down","message":"wait"}`))
	}))
	defer server.Close()

	client, _ := New(Config{ServerURL: server.URL})
	_, err := client.Handshake(context.Background())

	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v", err)
	}
	if statusErr.StatusCode != 429 || statusErr.Code != "slow_down" || statusErr.RetryAfter != 7*time.Second {
		t.Errorf("err = %+v", statusErr)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	client, _ := New(Config{ServerURL: server.URL, Token: "token"})
	_, err := client.SendContacts(context.Background(), []contact.Contact{{Callsign: "W1AW"}})

	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusFound {
		t.Errorf("err = %v", err)
	}
	if followed {
		t.Error("the redirect was followed")
	}
}

func handshakeServer() *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"server_version":"1","supported_api_versions":[1],"minimum_relay_version":null,"server_time":"2026-06-27T18:05:00Z","relay":{"id":1,"name":"Tent"},"event":null,"permissions":{"create_stations":true},"stations":[]}`))
	}))
}

func TestUntrustedCertificateIsRefusedWithoutAPin(t *testing.T) {
	server := handshakeServer()
	defer server.Close()

	client, _ := New(Config{ServerURL: server.URL})
	if _, err := client.Handshake(context.Background()); err == nil {
		t.Error("a self-signed certificate was accepted without a pin")
	}
}

func TestPinnedKeyIsAcceptedAndAnyOtherIsRefused(t *testing.T) {
	server := handshakeServer()
	defer server.Close()
	pin := sha256.Sum256(server.Certificate().RawSubjectPublicKeyInfo)

	client, err := New(Config{ServerURL: server.URL, PinSHA256: pin[:]})
	if err != nil {
		t.Fatal(err)
	}
	handshake, err := client.Handshake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if handshake.Relay.Name != "Tent" || handshake.MinimumRelayVersion != nil || handshake.Event != nil {
		t.Errorf("handshake = %+v", handshake)
	}

	wrong := sha256.Sum256([]byte("another key"))
	client, _ = New(Config{ServerURL: server.URL, PinSHA256: wrong[:]})
	if _, err := client.Handshake(context.Background()); !errors.Is(err, ErrPinMismatch) {
		t.Errorf("err = %v", err)
	}

	if _, err := New(Config{ServerURL: server.URL, PinSHA256: []byte("short")}); err == nil {
		t.Error("a malformed pin was accepted")
	}
}

func TestEnrollReturnsTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/relay/v1/enroll" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"fdca_abc","relay":{"id":3,"name":"Tent"}}`))
	}))
	defer server.Close()

	client, _ := New(Config{ServerURL: server.URL, Version: "26.11.1"})
	got, err := client.Enroll(context.Background(), "ABCD-EFGH-JKMN")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Token) != "fdca_abc" || got.Relay.ID != 3 {
		t.Errorf("got %+v", got.Relay)
	}
}
