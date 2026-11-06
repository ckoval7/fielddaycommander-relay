// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package app ties the relay's parts together: it keeps the logger source
// running from the moment the program starts, adds the sender once the relay is
// enrolled, and handles the changes a person makes on the status page.
package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/feed"
	"github.com/ckoval7/fielddaycommander-relay/internal/n3fjp"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
	"github.com/ckoval7/fielddaycommander-relay/internal/secret"
	"github.com/ckoval7/fielddaycommander-relay/internal/sender"
	"github.com/ckoval7/fielddaycommander-relay/internal/settings"
)

const (
	queueFile      = "relay.db"
	attentionLimit = 50
	defaultN3FJP   = "1000"
)

// EnrollFailure says why enrollment didn't work. The status page turns it into words.
type EnrollFailure string

// The ways enrollment can fail.
const (
	FailBadAddress    EnrollFailure = "bad_address"
	FailBadCode       EnrollFailure = "bad_code"
	FailUnreachable   EnrollFailure = "unreachable"
	FailRateLimited   EnrollFailure = "rate_limited"
	FailHTTPSRequired EnrollFailure = "https_required"
	FailRefused       EnrollFailure = "refused"
	FailKeychain      EnrollFailure = "keychain"
	FailPinMismatch   EnrollFailure = "pin_mismatch"
	FailUntrusted     EnrollFailure = "untrusted"
	FailSave          EnrollFailure = "save"
)

// EnrollError is a failed enrollment.
type EnrollError struct {
	Failure EnrollFailure
	// Fingerprint is the key a server with an untrusted certificate presented,
	// for a person to compare and accept.
	Fingerprint string
	Err         error
}

func (e *EnrollError) Error() string {
	return fmt.Sprintf("enroll: %s: %v", e.Failure, e.Err)
}

func (e *EnrollError) Unwrap() error { return e.Err }

// The relay's roles on an N3FJP network.
const (
	RoleClient = "client"
	RoleServer = "server"
)

// ErrBadRole means a role isn't one of the two.
var ErrBadRole = errors.New("unknown logger role")

// ErrBadLoggerAddress means a logger address isn't a host and port.
var ErrBadLoggerAddress = errors.New("logger address must be a host and port")

// EnrollRequest is what a person enters to enroll.
type EnrollRequest struct {
	ServerURL string
	Code      string
	// PinSHA256 is set once a person has accepted a self-signed server's key.
	PinSHA256 string
}

// Status is everything the status page shows.
type Status struct {
	Version       string
	Enrolled      bool
	ServerURL     string
	RelayName     string
	Pinned        bool
	LoggerAddress string
	Sender        sender.Status
	Source        n3fjp.Status
	// ServerRole is whether N3FJP PCs connect to the relay.
	ServerRole      bool
	ListenOnNetwork bool
	N3FJPServer     n3fjp.ServerStatus
	Feed            feed.Status
	Counts          queue.Counts
	Attention       []queue.Attention
}

// Config is what New needs.
type Config struct {
	DataDir string
	Version string
	Store   secret.Store
	Log     *slog.Logger
}

// App is the running relay.
type App struct {
	cfg   Config
	queue *queue.Queue

	mu       sync.RWMutex
	current  settings.Settings
	enrolled bool
	base     context.Context
	cancel   context.CancelFunc
	running  sync.WaitGroup
	sender   *sender.Sender
	source   *n3fjp.Source
	server   *n3fjp.Server
	feed     *feed.Syncer
}

// New opens the relay's data. Call Run to start it.
func New(cfg Config) (*App, error) {
	current, err := settings.Load(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	q, err := queue.Open(filepath.Join(cfg.DataDir, queueFile))
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, queue: q, current: current}, nil
}

// Run keeps the relay working until ctx is canceled, then closes its data.
func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()
	a.base = ctx
	a.restartLocked()
	a.mu.Unlock()

	<-ctx.Done()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
	return a.queue.Close()
}

func (a *App) stopLocked() {
	if a.cancel != nil {
		a.cancel()
		a.running.Wait()
		a.cancel = nil
	}
}

// restartLocked starts the parts again with the current settings. The source
// always runs: contacts are collected even before the relay enrolls, and they go
// out once it does.
func (a *App) restartLocked() {
	a.stopLocked()
	if a.base == nil || a.base.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(a.base)
	a.cancel = cancel

	var client *api.Client
	a.sender, client = a.buildSender()
	a.enrolled = a.sender != nil

	var notify func()
	if a.sender != nil {
		notify = a.sender.Wake
		running := a.sender
		a.running.Go(func() { running.Run(ctx) })
	}

	var writer feed.Writer
	a.source, a.server = nil, nil
	if a.current.LoggerRole == RoleServer {
		a.server = n3fjp.NewServer(a.listenAddress(), a.queue, a.queue, notify, a.cfg.Log)
		writer = a.server
		running := a.server
		a.running.Go(func() { running.Run(ctx) })
	} else {
		address := a.current.LoggerAddress
		if address == "" {
			address = n3fjp.DefaultAddress
		}
		a.source = n3fjp.NewSource(address, a.queue, notify, a.cfg.Log)
		writer = a.source
		running := a.source
		a.running.Go(func() { running.Run(ctx) })
	}

	a.feed = nil
	if client != nil {
		a.feed = feed.New(a.queue, client, writer, a.cfg.Log)
		running := a.feed
		a.running.Go(func() { running.Run(ctx) })
	}
}

func (a *App) listenAddress() string {
	if a.current.ListenOnNetwork {
		return n3fjp.NetworkListenAddress
	}
	return n3fjp.LocalListenAddress
}

// buildSender returns a sender and its client, or nil when the relay isn't
// enrolled or can't read its token. The token stays out of the app's own state.
func (a *App) buildSender() (*sender.Sender, *api.Client) {
	if a.current.ServerURL == "" {
		return nil, nil
	}
	token, err := a.cfg.Store.Get()
	if err != nil {
		if !errors.Is(err, secret.ErrNotFound) {
			a.cfg.Log.Error("reading the token failed", "error", err)
		}
		return nil, nil
	}
	pin, err := hex.DecodeString(a.current.PinSHA256)
	if err != nil {
		a.cfg.Log.Error("saved server pin is damaged", "error", err)
		return nil, nil
	}
	client, err := api.New(api.Config{ServerURL: a.current.ServerURL, Token: api.Token(token), Version: a.cfg.Version, PinSHA256: pin})
	if err != nil {
		a.cfg.Log.Error("saved server address is unusable", "error", err)
		return nil, nil
	}
	return sender.New(a.queue, client, a.cfg.Version, a.cfg.Log), client
}

// Enroll trades a one-time code for a token, stores it and starts sending.
func (a *App) Enroll(ctx context.Context, request EnrollRequest) error {
	serverURL := normalizeServerURL(request.ServerURL)

	pin, err := hex.DecodeString(strings.TrimSpace(request.PinSHA256))
	if err != nil {
		return &EnrollError{Failure: FailBadAddress, Err: err}
	}
	client, err := api.New(api.Config{ServerURL: serverURL, Version: a.cfg.Version, PinSHA256: pin})
	if err != nil {
		return &EnrollError{Failure: FailBadAddress, Err: err}
	}

	// A code works once, so find out now whether there's anywhere to put the token.
	if err := a.checkStore(); err != nil {
		return &EnrollError{Failure: FailKeychain, Err: err}
	}

	enrolled, err := client.Enroll(ctx, request.Code)
	if err != nil {
		return a.enrollFailure(ctx, serverURL, err)
	}

	if err := a.cfg.Store.Set(string(enrolled.Token)); err != nil {
		return &EnrollError{Failure: FailKeychain, Err: err}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	next := a.current
	next.ServerURL = serverURL
	next.PinSHA256 = hex.EncodeToString(pin)
	next.RelayName = enrolled.Relay.Name
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return &EnrollError{Failure: FailSave, Err: err}
	}
	// A new enrollment can mean another server or event, so the feed starts over.
	// Contacts the logger already has are recognized by their keys.
	if err := a.queue.ResetSync(ctx); err != nil {
		return &EnrollError{Failure: FailSave, Err: err}
	}
	a.current = next
	a.restartLocked()
	return nil
}

func (a *App) checkStore() error {
	_, err := a.cfg.Store.Get()
	if err == nil {
		return nil
	}
	if !errors.Is(err, secret.ErrNotFound) {
		return err
	}
	if err := a.cfg.Store.Set("relay-keychain-check"); err != nil {
		return err
	}
	return a.cfg.Store.Delete()
}

func (a *App) enrollFailure(ctx context.Context, serverURL string, err error) error {
	var statusErr *api.StatusError
	switch {
	case errors.Is(err, api.ErrPinMismatch):
		return &EnrollError{Failure: FailPinMismatch, Err: err}
	case api.IsCertificateError(err):
		fingerprint, pinErr := api.FetchPin(ctx, serverURL)
		if pinErr != nil {
			return &EnrollError{Failure: FailUnreachable, Err: pinErr}
		}
		return &EnrollError{Failure: FailUntrusted, Fingerprint: fingerprint, Err: err}
	case !errors.As(err, &statusErr):
		return &EnrollError{Failure: FailUnreachable, Err: err}
	}

	switch {
	case statusErr.StatusCode == http.StatusUnprocessableEntity:
		return &EnrollError{Failure: FailBadCode, Err: err}
	case statusErr.StatusCode == http.StatusTooManyRequests:
		return &EnrollError{Failure: FailRateLimited, Err: err}
	case statusErr.StatusCode == http.StatusForbidden && statusErr.Code == "https_required":
		return &EnrollError{Failure: FailHTTPSRequired, Err: err}
	}
	return &EnrollError{Failure: FailRefused, Err: err}
}

// Forget removes the token and server. Queued contacts stay, and go out if the
// relay enrolls again.
func (a *App) Forget(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	next := a.current
	next.ServerURL = ""
	next.PinSHA256 = ""
	next.RelayName = ""
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return err
	}
	if err := a.cfg.Store.Delete(); err != nil {
		return err
	}
	if err := a.queue.ResetSync(ctx); err != nil {
		return err
	}
	a.current = next
	a.restartLocked()
	return nil
}

// SetLogger saves how the relay joins N3FJP: as a client of the address, where
// empty means this computer and a port is optional, or as the N3FJP server.
// Saving what's already set changes nothing, so it doesn't drop any connections.
func (a *App) SetLogger(role, address string, listenOnNetwork bool) error {
	if role != RoleServer && role != RoleClient {
		return ErrBadRole
	}
	normalized, err := normalizeLoggerAddress(address)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	next := a.current
	next.LoggerAddress = normalized
	next.LoggerRole = ""
	next.ListenOnNetwork = false
	if role == RoleServer {
		next.LoggerRole = RoleServer
		next.ListenOnNetwork = listenOnNetwork
	}
	if next == a.current {
		return nil
	}

	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return err
	}
	// A different role means a different log. Starting the feed over fills it, and
	// contacts it already holds are recognized by their keys.
	if next.LoggerRole != a.current.LoggerRole {
		if err := a.queue.ResetSync(context.Background()); err != nil {
			return err
		}
	}
	a.current = next
	a.restartLocked()
	return nil
}

// Status reports the relay's current state.
func (a *App) Status(ctx context.Context) (Status, error) {
	a.mu.RLock()
	status := Status{
		Version:       a.cfg.Version,
		Enrolled:      a.enrolled,
		ServerURL:     a.current.ServerURL,
		RelayName:     a.current.RelayName,
		Pinned:        a.current.PinSHA256 != "",
		LoggerAddress: a.current.LoggerAddress,
	}
	if status.LoggerAddress == "" {
		status.LoggerAddress = n3fjp.DefaultAddress
	}
	if a.sender != nil {
		status.Sender = a.sender.Status()
	}
	status.ServerRole = a.current.LoggerRole == RoleServer
	status.ListenOnNetwork = a.current.ListenOnNetwork
	if a.source != nil {
		status.Source = a.source.Status()
	}
	if a.server != nil {
		status.N3FJPServer = a.server.Status()
	}
	if a.feed != nil {
		status.Feed = a.feed.Status()
	}
	a.mu.RUnlock()

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var err error
	if status.Counts, err = a.queue.Counts(queryCtx); err != nil {
		return status, err
	}
	if status.Attention, err = a.queue.NeedingAttention(queryCtx, attentionLimit); err != nil {
		return status, err
	}
	return status, nil
}

func normalizeServerURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	return strings.TrimRight(raw, "/")
}

func normalizeLoggerAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		host, port = strings.Trim(raw, "[]"), defaultN3FJP
	}
	if host == "" || strings.ContainsAny(host, " /\\") {
		return "", ErrBadLoggerAddress
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return "", ErrBadLoggerAddress
	}
	return net.JoinHostPort(host, port), nil
}
