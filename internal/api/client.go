// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package api is the client for a Field Day Commander server's relay API,
// described in api/relay-v1.openapi.yaml.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

const (
	basePath        = "/api/relay/v1"
	requestTimeout  = 2 * time.Minute
	maxResponseSize = 4 << 20
)

// ContactStatus values from the API. Anything else is treated as a retry.
const (
	StatusAccepted = "accepted"
	StatusRejected = "rejected"
)

// Token is a relay's bearer token. It prints as a placeholder so a stray log
// line can't leak it.
type Token string

// String hides the token.
func (Token) String() string { return "[redacted]" }

// GoString hides the token.
func (Token) GoString() string { return "[redacted]" }

// Config describes the server to talk to.
type Config struct {
	// ServerURL is the server's address, such as https://logs.example.org.
	ServerURL string
	Token     Token
	// Version is this relay's version, reported to the server.
	Version string
	// PinSHA256 is the SHA-256 of a self-signed server's public key (its SPKI).
	// When set, the server must present exactly that key. When empty, the
	// certificate is checked against the system's trusted roots.
	PinSHA256 []byte
}

// Client makes requests to one server.
type Client struct {
	base    string
	token   Token
	version string
	http    *http.Client
}

// New checks cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	base, err := baseURL(cfg.ServerURL)
	if err != nil {
		return nil, err
	}
	if len(cfg.PinSHA256) != 0 && len(cfg.PinSHA256) != sha256.Size {
		return nil, fmt.Errorf("server pin must be %d bytes", sha256.Size)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig(cfg.PinSHA256)

	return &Client{
		base:    base,
		token:   cfg.Token,
		version: cfg.Version,
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			// A redirect is a misconfigured address, and following one could
			// carry contacts somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// baseURL requires HTTPS, except for plain HTTP to this machine, which is the
// only exception the server makes too.
func baseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("server address: %w", err)
	}
	if parsed.Host == "" {
		return "", errors.New("server address has no host")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopback(parsed.Hostname()) {
			return "", errors.New("server address must use https")
		}
	default:
		return "", errors.New("server address must use https")
	}
	return parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.Path, "/") + basePath, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tlsConfig always verifies the server. Without a pin it uses Go's normal chain
// and hostname checks. With a pin the only thing trusted is that exact key, so
// the chain check is replaced by a stricter one, not removed.
func tlsConfig(pin []byte) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(pin) == 0 {
		return cfg
	}

	cfg.InsecureSkipVerify = true //nolint:gosec // VerifyConnection below checks the pin on every connection.
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("server sent no certificate")
		}
		sum := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
			return ErrPinMismatch
		}
		return nil
	}
	return cfg
}

// ErrPinMismatch means a pinned server presented a different key.
var ErrPinMismatch = errors.New("server's key doesn't match the pinned key")

// StatusError is a response other than the one expected.
type StatusError struct {
	StatusCode int
	Code       string
	Message    string
	// RetryAfter is how long the server asked the relay to wait, if it did.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("server answered %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// Relay names this relay.
type Relay struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Enrollment is the answer to Enroll.
type Enrollment struct {
	Token Token `json:"token"`
	Relay Relay `json:"relay"`
}

// Event is the event a relay enrolled for.
type Event struct {
	Name     string    `json:"name"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// Station is a station in the relay's event.
type Station struct {
	Name     string  `json:"name"`
	Hostname *string `json:"hostname"`
}

// Handshake is what the server reports about itself and this relay.
type Handshake struct {
	ServerVersion        string    `json:"server_version"`
	SupportedAPIVersions []int     `json:"supported_api_versions"`
	MinimumRelayVersion  *string   `json:"minimum_relay_version"`
	ServerTime           time.Time `json:"server_time"`
	Relay                Relay     `json:"relay"`
	Event                *Event    `json:"event"`
	Permissions          struct {
		CreateStations bool `json:"create_stations"`
	} `json:"permissions"`
	Stations []Station `json:"stations"`
}

// Result is the server's answer for one contact.
type Result struct {
	Index       int    `json:"index"`
	Status      string `json:"status"`
	ContactUUID string `json:"contact_uuid"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

// Enroll trades a one-time code for this relay's token.
func (c *Client) Enroll(ctx context.Context, code string) (Enrollment, error) {
	var out Enrollment
	err := c.do(ctx, http.MethodPost, "/enroll", map[string]string{"code": code, "relay_version": c.version}, http.StatusCreated, &out)
	return out, err
}

// Handshake reports this relay's version and fetches the server's capabilities.
func (c *Client) Handshake(ctx context.Context) (Handshake, error) {
	var out Handshake
	err := c.do(ctx, http.MethodGet, "/handshake", nil, http.StatusOK, &out)
	return out, err
}

// SendContacts sends one batch and returns the results by position in contacts.
// A contact the server didn't report on has no entry in the map, and must be
// treated as not stored.
func (c *Client) SendContacts(ctx context.Context, contacts []contact.Contact) (map[int]Result, error) {
	var out struct {
		Results []Result `json:"results"`
	}
	body := struct {
		Contacts []contact.Contact `json:"contacts"`
	}{contacts}
	if err := c.do(ctx, http.MethodPost, "/contacts", body, http.StatusOK, &out); err != nil {
		return nil, err
	}

	results := make(map[int]Result, len(out.Results))
	for _, result := range out.Results {
		if result.Index >= 0 && result.Index < len(contacts) {
			results[result.Index] = result
		}
	}
	return results, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, want int, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+string(c.token))
	}
	if c.version != "" {
		req.Header.Set("X-Relay-Version", c.version)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != want {
		return statusError(resp, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func statusError(resp *http.Response, data []byte) *StatusError {
	statusErr := &StatusError{StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil {
		statusErr.Code = body.Code
		statusErr.Message = body.Message
	}
	return statusErr
}

func retryAfter(header string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}
