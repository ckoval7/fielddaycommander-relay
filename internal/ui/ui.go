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
	"strconv"
	"strings"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/app"
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
	SetLoggerAddress(address string) error
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
		"senderState": senderStateText,
		"enrollError": enrollErrorText,
		"attention":   attentionText,
		"text":        func() pageText { return pageWords },
		"shortTime":   shortTime,
		"qsoTime":     qsoTime,
		"fingerprint": groupFingerprint,
		"minutes":     func(d time.Duration) int { return int(d.Abs().Minutes()) },
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
	mux.HandleFunc("POST /logger", s.protected(s.logger))
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
		header.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		header.Set("X-Frame-Options", "DENY")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
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
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("form_token")), []byte(s.formToken)) != 1 {
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
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data view) {
	status, err := s.backend.Status(r.Context())
	if err != nil {
		s.log.Error("reading status failed", "error", err)
		data.ActionError = true
	}
	data.Status = status
	data.FormToken = s.formToken

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering the page failed", "error", err)
	}
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "index.html", view{})
}

func (s *Server) statusFragment(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "status", view{})
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
	s.render(w, r, "index.html", data)
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Forget(r.Context()); err != nil {
		s.log.Error("forgetting the server failed", "error", err)
		s.render(w, r, "index.html", view{ActionError: true})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logger(w http.ResponseWriter, r *http.Request) {
	err := s.backend.SetLoggerAddress(r.PostFormValue("address"))
	switch {
	case err == nil:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case errors.Is(err, app.ErrBadLoggerAddress):
		s.render(w, r, "index.html", view{LoggerError: true})
	default:
		s.log.Error("saving the logger address failed", "error", err)
		s.render(w, r, "index.html", view{ActionError: true})
	}
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
