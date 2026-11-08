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
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/autostart"
	"github.com/ckoval7/fielddaycommander-relay/internal/feed"
	"github.com/ckoval7/fielddaycommander-relay/internal/logfile"
	"github.com/ckoval7/fielddaycommander-relay/internal/n1mm"
	"github.com/ckoval7/fielddaycommander-relay/internal/n3fjp"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
	"github.com/ckoval7/fielddaycommander-relay/internal/secret"
	"github.com/ckoval7/fielddaycommander-relay/internal/sender"
	"github.com/ckoval7/fielddaycommander-relay/internal/settings"
	"github.com/ckoval7/fielddaycommander-relay/internal/udplog"
)

const (
	queueFile      = "relay.db"
	attentionLimit = 50
	defaultN3FJP   = "1000"
	maxLogFiles    = 16
	// The server's stations table is this wide.
	maxStationName = 50
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
	// RoleOff is for a computer with no N3FJP, whose contacts come from log files.
	// Nothing writes back to a log file, so the feed doesn't run either.
	RoleOff = "off"
)

// ErrBadRole means a role isn't one of the two.
var ErrBadRole = errors.New("unknown logger role")

// ErrBadLoggerAddress means a logger address isn't a host and port.
var ErrBadLoggerAddress = errors.New("logger address must be a host and port")

// ErrBadStation means a station name can't be used as one.
var ErrBadStation = errors.New("station name isn't usable")

// ErrStationNotAllowed means a station isn't on the server, and this relay isn't
// allowed to create it.
var ErrStationNotAllowed = errors.New("station isn't on the server")

// ErrBadPort means a port isn't a number from 1 to 65535.
var ErrBadPort = errors.New("port must be from 1 to 65535")

// ErrPortTaken means another of the relay's UDP listeners has that port.
var ErrPortTaken = errors.New("another listener uses that port")

var errNoFileWindow = errors.New("this build has no file window")

var errNoAutostart = errors.New("this build can't start at sign-in")

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
	ServerRole bool
	// N3FJPOff is whether the relay has been told there's no N3FJP to reach.
	N3FJPOff        bool
	ListenOnNetwork bool
	N3FJPServer     n3fjp.ServerStatus
	Feed            feed.Status
	Counts          queue.Counts
	Attention       []queue.Attention
	LogFiles        []logfile.Status
	// N1MMEnabled is whether the relay listens for N1MM Logger+.
	N1MMEnabled   bool
	N1MMOnNetwork bool
	N1MM          n1mm.Status
	// WSJTX and UDPADIF are the loggers that send contacts over UDP.
	WSJTX   UDPLogger
	UDPADIF UDPLogger
	// ChoosingLogFile is whether the file window is showing.
	ChoosingLogFile bool
	// LogFileChooseFailed is whether the last try at choosing a file went wrong.
	LogFileChooseFailed bool
	// LogFilesFull is whether the relay is watching as many files as it can.
	LogFilesFull bool
	// Station is the station picked for this relay, if any.
	Station string
	// StationOverride is whether Station also replaces what contacts name themselves.
	StationOverride bool
}

// UDPLogger is the settings and state of one UDP listener.
type UDPLogger struct {
	Enabled   bool
	OnNetwork bool
	// Port is the port it listens on, or would if it were on.
	Port int
	udplog.Status
}

// Config is what New needs.
type Config struct {
	DataDir string
	Version string
	Store   secret.Store
	Log     *slog.Logger
	// PickLogFile asks the person for a file and returns its location, or "" if they
	// change their mind.
	PickLogFile func() (string, error)
	// Autostart starts the relay at sign-in. Nil means this build can't.
	Autostart autostart.Starter
}

// App is the running relay.
type App struct {
	cfg   Config
	queue *queue.Queue

	mu       sync.RWMutex
	current  settings.Settings
	enrolled bool
	// alive is whether Run is keeping the parts going. Run cancels them all itself
	// when it ends, so their contexts don't need to come from its own.
	alive   bool
	cancel  context.CancelFunc
	running sync.WaitGroup
	sender  *sender.Sender
	source  *n3fjp.Source
	server  *n3fjp.Server
	feed    *feed.Syncer

	// Log files stop and start on their own, so changing them doesn't drop N3FJP PCs.
	files        []*logfile.Watcher
	filesCancel  context.CancelFunc
	filesRunning sync.WaitGroup
	choosing     bool

	// N1MM keeps listening through every other change, since UDP that arrives while
	// the port is closed is gone. It wakes whichever sender is current.
	n1mm         *n1mm.Listener
	n1mmCancel   context.CancelFunc
	n1mmRunning  sync.WaitGroup
	wakeTarget   atomic.Pointer[sender.Sender]
	chooseFailed bool

	// udp holds the UDP loggers by kind. Like N1MM, they keep listening through
	// every other change.
	udp [2]udpListener
}

type udpListener struct {
	listener *udplog.Listener
	cancel   context.CancelFunc
	running  sync.WaitGroup
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
	a.alive = ctx.Err() == nil
	a.restartLocked()
	a.startN1MMLocked()
	a.startUDPLocked(udplog.WSJTX)
	a.startUDPLocked(udplog.ADIF)
	a.mu.Unlock()

	<-ctx.Done()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.alive = false
	a.stopLocked()
	a.stopN1MMLocked()
	a.stopUDPLocked(udplog.WSJTX)
	a.stopUDPLocked(udplog.ADIF)
	return a.queue.Close()
}

func (a *App) stopLocked() {
	a.stopFilesLocked()
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
	if !a.alive {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	var client *api.Client
	a.sender, client = a.buildSender()
	a.enrolled = a.sender != nil
	a.wakeTarget.Store(a.sender)

	var notify func()
	if a.sender != nil {
		notify = a.sender.Wake
		running := a.sender
		a.running.Go(func() { running.Run(ctx) })
	}

	a.startFilesLocked(notify)

	var writer feed.Writer
	a.source, a.server = nil, nil
	switch a.current.LoggerRole {
	case RoleOff:
	case RoleServer:
		a.server = n3fjp.NewServer(a.listenAddress(), a.queue, a.queue, notify, a.cfg.Log)
		writer = a.server
		running := a.server
		a.running.Go(func() { running.Run(ctx) })
	default:
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
	if client != nil && writer != nil {
		a.feed = feed.New(a.queue, client, writer, a.cfg.Log)
		running := a.feed
		a.running.Go(func() { running.Run(ctx) })
	}
}

func (a *App) startN1MMLocked() {
	a.n1mm = nil
	if !a.current.N1MMEnabled {
		return
	}
	address := n1mm.LocalAddress
	if a.current.N1MMOnNetwork {
		address = n1mm.NetworkAddress
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.n1mmCancel = cancel
	listener := n1mm.New(address, a.queue, a.wakeSender, a.cfg.Log)
	a.n1mm = listener
	a.n1mmRunning.Go(func() { listener.Run(ctx) })
}

func (a *App) stopN1MMLocked() {
	if a.n1mmCancel != nil {
		a.n1mmCancel()
		a.n1mmRunning.Wait()
		a.n1mmCancel = nil
	}
}

func (a *App) wakeSender() {
	if current := a.wakeTarget.Load(); current != nil {
		current.Wake()
	}
}

// SetN1MM saves whether to listen for N1MM Logger+, and whether other PCs can reach
// the listener. Saving what's already set changes nothing.
func (a *App) SetN1MM(enabled, onNetwork bool) error {
	onNetwork = onNetwork && enabled

	a.mu.Lock()
	defer a.mu.Unlock()
	if enabled == a.current.N1MMEnabled && onNetwork == a.current.N1MMOnNetwork {
		return nil
	}
	next := a.current
	next.N1MMEnabled, next.N1MMOnNetwork = enabled, onNetwork
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return err
	}
	a.current = next

	a.stopN1MMLocked()
	if a.alive {
		a.startN1MMLocked()
	}
	return nil
}

// udpSettings is where the settings for a kind of UDP logger are kept.
type udpSettings struct {
	enabled, onNetwork *bool
	// port is zero for the kind's default.
	port *int
}

func udpSettingsOf(s *settings.Settings, kind udplog.Kind) udpSettings {
	if kind == udplog.WSJTX {
		return udpSettings{&s.WSJTXEnabled, &s.WSJTXOnNetwork, &s.WSJTXPort}
	}
	return udpSettings{&s.UDPADIFEnabled, &s.UDPADIFOnNetwork, &s.UDPADIFPort}
}

// udpPortLocked is the port a kind of UDP logger uses with the current settings.
func (a *App) udpPortLocked(kind udplog.Kind) int {
	if port := *udpSettingsOf(&a.current, kind).port; port != 0 {
		return port
	}
	return kind.DefaultPort()
}

// udpPortTakenLocked reports whether port belongs to another of the relay's UDP
// listeners. Two can't share one, and one that's off may be turned on later.
func (a *App) udpPortTakenLocked(kind udplog.Kind, port int) bool {
	for _, other := range []udplog.Kind{udplog.WSJTX, udplog.ADIF} {
		if other != kind && a.udpPortLocked(other) == port {
			return true
		}
	}
	_, n1mmPort, _ := net.SplitHostPort(n1mm.LocalAddress)
	return strconv.Itoa(port) == n1mmPort
}

func (a *App) startUDPLocked(kind udplog.Kind) {
	slot := &a.udp[kind]
	slot.listener = nil
	saved := udpSettingsOf(&a.current, kind)
	if !*saved.enabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	slot.cancel = cancel
	listener := udplog.New(kind, kind.Address(*saved.onNetwork, *saved.port), a.queue, a.wakeSender, a.cfg.Log)
	slot.listener = listener
	slot.running.Go(func() { listener.Run(ctx) })
}

func (a *App) stopUDPLocked(kind udplog.Kind) {
	slot := &a.udp[kind]
	if slot.cancel != nil {
		slot.cancel()
		slot.running.Wait()
		slot.cancel = nil
	}
}

// SetUDPLogger saves whether to listen for a kind of UDP logger, whether other PCs
// can reach the listener, and its port, where 0 keeps the one it has. Saving what's
// already set changes nothing.
func (a *App) SetUDPLogger(kind udplog.Kind, enabled, onNetwork bool, port int) error {
	onNetwork = onNetwork && enabled
	if port < 0 || port > 65535 {
		return ErrBadPort
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if port == 0 {
		port = a.udpPortLocked(kind)
	}
	if a.udpPortTakenLocked(kind, port) {
		return ErrPortTaken
	}
	// The default is kept as zero, so it stays whatever the server's default is.
	if port == kind.DefaultPort() {
		port = 0
	}
	was := udpSettingsOf(&a.current, kind)
	if enabled == *was.enabled && onNetwork == *was.onNetwork && port == *was.port {
		return nil
	}
	next := a.current
	changed := udpSettingsOf(&next, kind)
	*changed.enabled, *changed.onNetwork, *changed.port = enabled, onNetwork, port
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return err
	}
	a.current = next

	a.stopUDPLocked(kind)
	if a.alive {
		a.startUDPLocked(kind)
	}
	return nil
}

func (a *App) udpStatusLocked(kind udplog.Kind) UDPLogger {
	saved := udpSettingsOf(&a.current, kind)
	status := UDPLogger{Enabled: *saved.enabled, OnNetwork: *saved.onNetwork, Port: a.udpPortLocked(kind)}
	if listener := a.udp[kind].listener; listener != nil {
		status.Status = listener.Status()
	}
	return status
}

func (a *App) startFilesLocked(notify func()) {
	ctx, cancel := context.WithCancel(context.Background())
	a.filesCancel = cancel
	a.files = nil
	for _, path := range a.current.LogFiles {
		watcher := logfile.New(path, a.queue, notify, a.cfg.Log)
		a.files = append(a.files, watcher)
		a.filesRunning.Go(func() { watcher.Run(ctx) })
	}
}

func (a *App) stopFilesLocked() {
	if a.filesCancel != nil {
		a.filesCancel()
		a.filesRunning.Wait()
		a.filesCancel = nil
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
	created := sender.New(a.queue, client, a.cfg.Version, a.cfg.Log)
	created.SetStation(a.stationChoiceLocked())
	return created, client
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
	// Stations belong to an event, and a new enrollment can mean another one.
	next.Station, next.StationOverride = "", false
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return &EnrollError{Failure: FailSave, Err: err}
	}
	// A new enrollment can mean another server or event, so the feed starts over.
	// Contacts the logger already has are recognized by their keys.
	if err := a.queue.ResetSync(ctx); err != nil {
		return &EnrollError{Failure: FailSave, Err: err}
	}
	if err := a.queue.ForgetEventPeriod(ctx); err != nil {
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
	next.Station, next.StationOverride = "", false
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return err
	}
	if err := a.cfg.Store.Delete(); err != nil {
		return err
	}
	if err := a.queue.ResetSync(ctx); err != nil {
		return err
	}
	if err := a.queue.ForgetEventPeriod(ctx); err != nil {
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
	if role != RoleServer && role != RoleClient && role != RoleOff {
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
	switch role {
	case RoleServer:
		next.LoggerRole = RoleServer
		next.ListenOnNetwork = listenOnNetwork
	case RoleOff:
		next.LoggerRole = RoleOff
	}
	if reflect.DeepEqual(next, a.current) {
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

// SetStation saves the station this relay is set up for, where empty means the
// server uses the relay's name, and whether it replaces what contacts name
// themselves. A name the server doesn't list needs permission to create it, and
// the server creates it with the first contact sent for it. Saving what's already
// set changes nothing.
func (a *App) SetStation(name string, override bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	name, err := a.usableStationLocked(strings.TrimSpace(name))
	if err != nil {
		return err
	}
	override = override && name != ""
	if name == a.current.Station && override == a.current.StationOverride {
		return nil
	}

	next := a.current
	next.Station, next.StationOverride = name, override
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return err
	}
	a.current = next

	if a.sender != nil {
		a.sender.SetStation(a.stationChoiceLocked())
		// The contacts would go out anyway once their delay ends.
		if err := a.queue.RetryWaitingForStation(context.Background()); err != nil {
			a.cfg.Log.Error("retrying contacts waiting for a station failed", "error", err)
		}
		a.sender.Wake()
	}
	return nil
}

// usableStationLocked checks a name against what the server last said, and returns
// it as the server spells it. The server matches names and hostnames in any case.
func (a *App) usableStationLocked(name string) (string, error) {
	if name == "" || name == a.current.Station {
		return name, nil
	}
	if utf8.RuneCountInString(name) > maxStationName || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrBadStation
	}
	if a.sender == nil {
		return "", ErrStationNotAllowed
	}
	known := a.sender.Status()
	for _, station := range known.Stations {
		if strings.EqualFold(station.Name, name) || (station.Hostname != nil && strings.EqualFold(*station.Hostname, name)) {
			return station.Name, nil
		}
	}
	if !known.CanCreateStations {
		return "", ErrStationNotAllowed
	}
	return name, nil
}

func (a *App) stationChoiceLocked() sender.StationChoice {
	return sender.StationChoice{Name: a.current.Station, Override: a.current.StationOverride}
}

// AskServerAgain gets the server's stations again, for someone opening the page.
func (a *App) AskServerAgain() {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.sender != nil {
		a.sender.AskServerAgain()
	}
}

// ChooseLogFile shows the file window, unless it's already showing. The person may
// take their time, so this doesn't wait: Status reports how it went.
func (a *App) ChooseLogFile() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.choosing || len(a.current.LogFiles) >= maxLogFiles {
		return
	}
	a.choosing, a.chooseFailed = true, false
	go a.chooseLogFile()
}

func (a *App) chooseLogFile() {
	err := errNoFileWindow
	var path string
	if a.cfg.PickLogFile != nil {
		path, err = a.cfg.PickLogFile()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.choosing = false
	if err == nil && path != "" && !slices.Contains(a.current.LogFiles, path) {
		err = a.setLogFilesLocked(append(slices.Clone(a.current.LogFiles), path))
	}
	if err != nil {
		a.cfg.Log.Error("choosing a log file failed", "error", err)
		a.chooseFailed = true
	}
}

// RemoveLogFile stops watching a file. Contacts already read from it stay.
func (a *App) RemoveLogFile(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Contains(a.current.LogFiles, path) {
		return nil
	}
	return a.setLogFilesLocked(slices.DeleteFunc(slices.Clone(a.current.LogFiles), func(other string) bool { return other == path }))
}

// setLogFilesLocked changes the watched files. N3FJP keeps running meanwhile.
func (a *App) setLogFilesLocked(paths []string) error {
	next := a.current
	next.LogFiles = paths
	if err := settings.Save(a.cfg.DataDir, next); err != nil {
		return err
	}
	a.current = next

	a.stopFilesLocked()
	if a.alive {
		var notify func()
		if a.sender != nil {
			notify = a.sender.Wake
		}
		a.startFilesLocked(notify)
	}
	return nil
}

// StartAtLogin reports whether the relay will start at the next sign-in. If that
// can't be read, the page doesn't offer a choice it can't show correctly.
func (a *App) StartAtLogin() autostart.Status {
	if a.cfg.Autostart == nil {
		return autostart.Status{}
	}
	status, err := a.cfg.Autostart.Status()
	if err != nil {
		a.cfg.Log.Warn("couldn't read the startup entry", "error", err)
		return autostart.Status{}
	}
	return status
}

// SetStartAtLogin changes whether the relay starts at sign-in.
func (a *App) SetStartAtLogin(on bool) error {
	if a.cfg.Autostart == nil {
		return errNoAutostart
	}
	return a.cfg.Autostart.Set(on)
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
	status.N3FJPOff = a.current.LoggerRole == RoleOff
	status.N1MMEnabled = a.current.N1MMEnabled
	status.N1MMOnNetwork = a.current.N1MMOnNetwork
	if a.n1mm != nil {
		status.N1MM = a.n1mm.Status()
	}
	status.WSJTX = a.udpStatusLocked(udplog.WSJTX)
	status.UDPADIF = a.udpStatusLocked(udplog.ADIF)
	status.ChoosingLogFile = a.choosing
	status.LogFileChooseFailed = a.chooseFailed
	status.LogFilesFull = len(a.current.LogFiles) >= maxLogFiles
	status.Station = a.current.Station
	status.StationOverride = a.current.StationOverride
	choice := a.stationChoiceLocked()
	for _, watcher := range a.files {
		status.LogFiles = append(status.LogFiles, watcher.Status())
	}
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
	// A contact still waiting goes out with the current station, so show that name:
	// it's the one someone would have to create. The server's default is the relay's.
	for i := range status.Attention {
		if item := &status.Attention[i]; !item.Rejected {
			if item.Station = choice.For(item.Station); item.Station == "" {
				item.Station = status.RelayName
			}
		}
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
