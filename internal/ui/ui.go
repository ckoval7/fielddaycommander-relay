// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package ui is the relay's settings and status page, served on loopback only.
//
// Any web page the operator visits can try to reach a local port, so every
// request's Host must name this listener (which stops DNS rebinding), and every
// change needs this run's random form token and a same-origin request (which
// stops another site posting to it).
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/app"
	"github.com/ckoval7/fielddaycommander-relay/internal/autostart"
)

const (
	pageTemplate = "index.html"
	sameOrigin   = "same-origin"
)

//go:embed web
var webFiles embed.FS

// HealthMarker identifies a running relay on the health endpoint, so a second
// copy can open the first one's page and not start.
const HealthMarker = "field-day-commander-relay"

const (
	enrollTimeout = 30 * time.Second
	maxFormBytes  = 8 << 10
)

// Backend is what the page can see and change.
type Backend interface {
	Status(ctx context.Context) (app.Status, error)
	Enroll(ctx context.Context, request app.EnrollRequest) error
	Forget(ctx context.Context) error
	SetLogger(role, address string, listenOnNetwork bool) error
	SetN1MM(enabled, onNetwork bool) error
	ChooseLogFile()
	RemoveLogFile(path string) error
	SetStation(name string, override bool) error
	AskServerAgain()
	StartAtLogin() autostart.Status
	SetStartAtLogin(on bool) error
}

// Server serves the page.
type Server struct {
	backend   Backend
	log       *slog.Logger
	formToken string
	pages     *template.Template
	port      int
}

// New returns a Server. Call Serve, or set the port with SetPort for a test.
func New(backend Backend, log *slog.Logger) (*Server, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("make form token: %w", err)
	}
	pages, err := template.New("").Funcs(template.FuncMap{
		"senderState":     senderStateText,
		"senderLabel":     senderLabelText,
		"senderBadge":     senderBadgeClass,
		"enrollError":     enrollErrorText,
		"attention":       attentionText,
		"text":            func() pageText { return pageWords },
		"shortTime":       shortTime,
		"qsoTime":         qsoTime,
		"fingerprint":     groupFingerprint,
		"fileState":       fileStateText,
		"fileLabel":       fileLabelText,
		"fileBadge":       fileBadgeClass,
		"fileName":        fileName,
		"fileFolder":      fileFolder,
		"help":            newHelp,
		"stationList":     stationList,
		"stationMissing":  stationMissing,
		"stationUnlisted": stationUnlisted,
		"minutes":         func(d time.Duration) int { return int(d.Abs().Minutes()) },
	}).ParseFS(webFiles, "web/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse pages: %w", err)
	}
	return &Server{backend: backend, log: log, formToken: hex.EncodeToString(token), pages: pages}, nil
}

// SetPort tells the server which port it's reachable on.
func (s *Server) SetPort(port int) { s.port = port }

// Handler returns the page's routes with its safety checks in front.
func (s *Server) Handler() http.Handler {
	assets, err := fs.Sub(webFiles, "web/assets")
	if err != nil {
		panic(err) // The folder is embedded at build time.
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /status", s.statusFragment)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(HealthMarker))
	})
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("POST /enroll", s.protected(s.enroll))
	mux.HandleFunc("POST /forget", s.protected(s.forget))
	mux.HandleFunc("POST /n3fjp", s.protected(s.logger))
	mux.HandleFunc("POST /n1mm", s.protected(s.n1mm))
	mux.HandleFunc("POST /station", s.protected(s.station))
	mux.HandleFunc("POST /logfiles/choose", s.protected(s.chooseLogFile))
	mux.HandleFunc("POST /logfiles/remove", s.protected(s.removeLogFile))
	mux.HandleFunc("POST /startup", s.protected(s.startup))
	return s.guard(mux)
}

// Serve answers on listener until ctx is canceled.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() {
		return errors.New("the status page only listens on loopback")
	}
	s.port = address.Port

	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      enrollTimeout + 10*time.Second,
	}
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	})
	defer stop()

	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		header := w.Header()
		header.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		header.Set("X-Frame-Options", "DENY")
		header.Set("X-Content-Type-Options", "nosniff")
		// Not no-referrer: with that, browsers send "Origin: null" on this page's own
		// form posts, and the same-origin check can't tell them from another site's.
		header.Set("Referrer-Policy", sameOrigin)
		header.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(host string) bool {
	port := strconv.Itoa(s.port)
	return host == net.JoinHostPort("127.0.0.1", port) || host == net.JoinHostPort("localhost", port)
}

// protected rejects changes that didn't come from this page.
func (s *Server) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != sameOrigin && site != "none" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("form_token")), []byte(s.formToken)) != 1 {
			// A tab left open across a relay restart holds the old run's token. It
			// changes nothing, so send the person back to a fresh page.
			if r.Header.Get("Sec-Fetch-Site") == sameOrigin {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// view is what the templates see.
type view struct {
	Status      app.Status
	FormToken   string
	ServerURL   string
	Code        string
	EnrollErr   *app.EnrollError
	LoggerError bool
	ActionError bool
	// StationError is what went wrong choosing a station, in words.
	StationError string
	// NewStation is a new station's name that was refused, kept for correcting.
	NewStation string
	// StartAtLogin is only read for the whole page, not for each refresh.
	StartAtLogin autostart.Status
}

// helpView is the wording behind one ? button.
type helpView struct {
	// Topic says what the button is about, for those who can't see what's beside it.
	Topic      string
	Paragraphs []string
}

func newHelp(topic string, paragraphs ...string) helpView {
	return helpView{Topic: topic, Paragraphs: paragraphs}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data view) {
	status, err := s.backend.Status(r.Context())
	if err != nil {
		s.log.Error("reading status failed", "error", err)
		data.ActionError = true
	}
	data.Status = status
	data.FormToken = s.formToken
	if name == pageTemplate {
		data.StartAtLogin = s.backend.StartAtLogin()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering the page failed", "error", err)
	}
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	// Someone may have just been told a station was created. The refresh shows it.
	s.backend.AskServerAgain()
	s.render(w, r, pageTemplate, view{})
}

func (s *Server) statusFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "refresh", view{})
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), enrollTimeout)
	defer cancel()

	request := app.EnrollRequest{
		ServerURL: r.PostFormValue("server_url"),
		Code:      strings.TrimSpace(r.PostFormValue("code")),
		PinSHA256: r.PostFormValue("pin"),
	}
	err := s.backend.Enroll(ctx, request)
	if err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	s.log.Warn("enrollment failed", "error", err)
	data := view{ServerURL: request.ServerURL, Code: request.Code}
	if !errors.As(err, &data.EnrollErr) {
		data.EnrollErr = &app.EnrollError{Failure: app.FailRefused, Err: err}
	}
	s.render(w, r, pageTemplate, data)
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Forget(r.Context()); err != nil {
		s.log.Error("forgetting the server failed", "error", err)
		s.render(w, r, pageTemplate, view{ActionError: true})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logger(w http.ResponseWriter, r *http.Request) {
	err := s.backend.SetLogger(r.PostFormValue("role"), r.PostFormValue("address"), r.PostFormValue("listen_on_network") != "")
	switch {
	case err == nil:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case errors.Is(err, app.ErrBadLoggerAddress):
		s.render(w, r, pageTemplate, view{LoggerError: true})
	default:
		s.log.Error("saving the N3FJP settings failed", "error", err)
		s.render(w, r, pageTemplate, view{ActionError: true})
	}
}

func (s *Server) n1mm(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.SetN1MM(r.PostFormValue("enabled") != "", r.PostFormValue("on_network") != ""); err != nil {
		s.log.Error("saving the N1MM settings failed", "error", err)
		s.render(w, r, pageTemplate, view{ActionError: true})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) station(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("station")
	created := strings.TrimSpace(r.PostFormValue("new_station"))
	if created != "" {
		name = created
	}
	err := s.backend.SetStation(name, r.PostFormValue("override") != "")
	switch {
	case err == nil:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case errors.Is(err, app.ErrBadStation):
		s.render(w, r, pageTemplate, view{StationError: pageWords.StationBadName, NewStation: created})
	case errors.Is(err, app.ErrStationNotAllowed):
		s.render(w, r, pageTemplate, view{StationError: pageWords.StationNotAllowed, NewStation: created})
	default:
		s.log.Error("saving the station failed", "error", err)
		s.render(w, r, pageTemplate, view{ActionError: true})
	}
}

func (s *Server) startup(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.SetStartAtLogin(r.PostFormValue("start_at_login") != ""); err != nil {
		s.log.Error("saving the startup choice failed", "error", err)
		s.render(w, r, pageTemplate, view{ActionError: true})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) chooseLogFile(w http.ResponseWriter, r *http.Request) {
	s.backend.ChooseLogFile()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) removeLogFile(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.RemoveLogFile(r.PostFormValue("path")); err != nil {
		s.log.Error("removing a log file failed", "error", err)
		s.render(w, r, pageTemplate, view{ActionError: true})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// stationList is what the dropdown offers: the server's stations, and the one picked
// even if the server doesn't have it yet.
func stationList(status app.Status) []string {
	names := make([]string, 0, len(status.Sender.Stations)+1)
	for _, station := range status.Sender.Stations {
		names = append(names, station.Name)
	}
	if status.Station != "" && !slices.Contains(names, status.Station) {
		names = append(names, status.Station)
	}
	return names
}

// stationMissing is whether the picked station isn't on the server and this relay
// can't create it, so contacts would wait until someone does.
func stationMissing(status app.Status) bool {
	return stationUnlisted(status) && !status.Sender.CanCreateStations
}

// stationUnlisted is whether the server has answered and doesn't list the pick.
func stationUnlisted(status app.Status) bool {
	if status.Station == "" || !status.Sender.StationsKnown {
		return false
	}
	return !slices.ContainsFunc(status.Sender.Stations, func(station api.Station) bool {
		return station.Name == status.Station
	})
}

// fileName is the last part of a path. Paths are split by hand so a Windows path
// reads the same on any build.
func fileName(path string) string {
	return path[strings.LastIndexAny(path, `\/`)+1:]
}

// fileFolder is everything before fileName.
func fileFolder(path string) string {
	return path[:strings.LastIndexAny(path, `\/`)+1]
}

func shortTime(when time.Time) string {
	if when.IsZero() {
		return ""
	}
	return when.Local().Format("15:04:05")
}

func qsoTime(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.Format("2006-01-02 15:04") + "Z"
}

// groupFingerprint makes a key easier for a person to compare.
func groupFingerprint(hexKey string) string {
	var groups []string
	for start := 0; start < len(hexKey); start += 4 {
		groups = append(groups, hexKey[start:min(start+4, len(hexKey))])
	}
	return strings.Join(groups, " ")
}
