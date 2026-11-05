// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package e2e runs the relay's parts together against stand-ins for an N3FJP
// network server and a Field Day Commander server.
package e2e

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/n3fjp"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
	"github.com/ckoval7/fielddaycommander-relay/internal/sender"
)

// fakeFDC follows the API's rules for keys: a known key updates its contact,
// replaces_key renames one, and a deleted contact stays deleted.
type fakeFDC struct {
	mu       sync.Mutex
	contacts map[string]map[string]any
	deleted  map[string]bool
	down     atomic.Bool
	seen     int
}

func newFakeFDC() *fakeFDC {
	return &fakeFDC{contacts: map[string]map[string]any{}, deleted: map[string]bool{}}
}

func (f *fakeFDC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	switch r.URL.Path {
	case "/api/relay/v1/handshake":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server_version": "test", "supported_api_versions": []int{1}, "minimum_relay_version": nil,
			"server_time": time.Now().UTC().Format(time.RFC3339), "relay": map[string]any{"id": 1, "name": "Tent"},
			"event": nil, "permissions": map[string]any{"create_stations": true}, "stations": []any{},
		})
	case "/api/relay/v1/contacts":
		var body struct {
			Contacts []map[string]any `json:"contacts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		results := make([]map[string]any, len(body.Contacts))
		f.mu.Lock()
		for i, c := range body.Contacts {
			f.store(c)
			results[i] = map[string]any{"index": i, "status": "accepted"}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeFDC) store(c map[string]any) {
	f.seen++
	key, _ := c["idempotency_key"].(string)
	if c["deleted"] == true {
		delete(f.contacts, key)
		f.deleted[key] = true
		return
	}
	if f.deleted[key] {
		return
	}
	if old, _ := c["replaces_key"].(string); old != "" {
		delete(f.contacts, old)
	}
	f.contacts[key] = c
}

func (f *fakeFDC) calls() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := map[string]int{}
	for _, c := range f.contacts {
		call, _ := c["callsign"].(string)
		calls[call]++
	}
	return calls
}

func (f *fakeFDC) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

// fakeN3FJP serves a log the way an N3FJP network server does: the whole log in
// answer to OPEN, then changes as they happen.
type fakeN3FJP struct {
	listener net.Listener
	mu       sync.Mutex
	conn     net.Conn
	log      map[int]string
}

func newFakeN3FJP(t *testing.T) *fakeN3FJP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	server := &fakeN3FJP{listener: listener, log: map[int]string{}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.mu.Lock()
			server.conn = conn
			server.mu.Unlock()
			go server.serve(conn)
		}
	}()
	return server
}

// serve answers every read as an OPEN, the only message that needs an answer here.
func (f *fakeN3FJP) serve(conn net.Conn) {
	buffer := make([]byte, 1024)
	for {
		if _, err := conn.Read(buffer); err != nil {
			return
		}
		f.mu.Lock()
		f.write(tx("CLEAR", ""))
		for number := 1; number <= len(f.log)+64; number++ {
			if record, ok := f.log[number]; ok {
				f.write(tx("GET", record))
			}
		}
		f.write(tx("LIST", ""))
		f.mu.Unlock()
	}
}

func (f *fakeN3FJP) write(message string) {
	if f.conn != nil {
		_, _ = f.conn.Write(wire(message))
	}
}

func (f *fakeN3FJP) broadcast(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.write(message)
}

func (f *fakeN3FJP) add(number int, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log[number] = record(call, number)
	f.write(tx("ADD", f.log[number]))
}

func (f *fakeN3FJP) dropClient() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn != nil {
		_ = f.conn.Close()
		f.conn = nil
	}
}

func (f *fakeN3FJP) logOffline(number int, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log[number] = record(call, number)
}

func record(call string, number int) string {
	return fmt.Sprintf("<FLDBAND>20</FLDBAND><FLDCALL>%s</FLDCALL><FLDCLASS>2A</FLDCLASS><FLDDATESTR>2026/06/27</FLDDATESTR><FLDMODE>PH</FLDMODE><FLDPRIMARYKEY>%d</FLDPRIMARYKEY><FLDRSTS>5NN</FLDRSTS><FLDSECTION>CT</FLDSECTION><FLDSTATION>Tent</FLDSTATION><FLDTIMEONSTR>18:42:07</FLDTIMEONSTR>", call, number)
}

func tx(kind, data string) string {
	return "<BOR><NTWK><FROM>PC-1</FROM><TRANSACTION>" + kind + "</TRANSACTION><XMLDATA>" + data + "</XMLDATA><EOR>"
}

func wire(message string) []byte {
	units := utf16.Encode([]rune(message + "\x03\x04\x07"))
	out := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(out[i*2:], unit)
	}
	return out
}

type relay struct {
	queue  *queue.Queue
	cancel context.CancelFunc
	done   chan struct{}
}

// startRelay wires the parts together the way the program will.
func startRelay(t *testing.T, path, serverURL, loggerAddress string) *relay {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	q, err := queue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.New(api.Config{ServerURL: serverURL, Token: "token", Version: "26.11.1"})
	if err != nil {
		t.Fatal(err)
	}
	s := sender.New(q, client, "26.11.1", log)
	source := n3fjp.NewSource(loggerAddress, q, s.Wake, log)

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { s.Run(ctx) })
	running.Go(func() { source.Run(ctx) })

	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()

	r := &relay{queue: q, cancel: cancel, done: done}
	t.Cleanup(r.stop)
	return r
}

func (r *relay) stop() {
	r.cancel()
	<-r.done
	_ = r.queue.Close()
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func TestAContactLoggedDuringAnOutageReachesTheServer(t *testing.T) {
	logger := newFakeN3FJP(t)
	fdc := newFakeFDC()
	server := httptest.NewServer(fdc)
	defer server.Close()

	fdc.down.Store(true)
	r := startRelay(t, filepath.Join(t.TempDir(), "relay.db"), server.URL, logger.listener.Addr().String())

	eventually(t, "the logger connection", func() bool {
		logger.mu.Lock()
		defer logger.mu.Unlock()
		return logger.conn != nil
	})
	logger.add(1, "W1AW")
	eventually(t, "the contact in the queue", func() bool {
		counts, err := r.queue.Counts(context.Background())
		return err == nil && counts.Pending == 1
	})
	if len(fdc.calls()) != 0 {
		t.Fatal("the server is down")
	}

	fdc.down.Store(false)
	eventually(t, "the contact on the server", func() bool { return fdc.calls()["W1AW"] == 1 })
}

func TestARestartSendsOnlyWhatIsNew(t *testing.T) {
	logger := newFakeN3FJP(t)
	fdc := newFakeFDC()
	server := httptest.NewServer(fdc)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "relay.db")
	address := logger.listener.Addr().String()

	first := startRelay(t, path, server.URL, address)
	eventually(t, "the logger connection", func() bool {
		logger.mu.Lock()
		defer logger.mu.Unlock()
		return logger.conn != nil
	})
	logger.add(1, "W1AW")
	logger.add(2, "K1ABC")
	// Until the answers are recorded, a restart rightly sends these again.
	eventually(t, "two answered contacts", func() bool {
		counts, err := first.queue.Counts(context.Background())
		return err == nil && counts.Accepted == 2
	})
	sent := fdc.requests()

	first.stop()
	logger.dropClient()
	logger.logOffline(3, "N1MM")

	startRelay(t, path, server.URL, address)
	eventually(t, "the contact logged while the relay was stopped", func() bool { return fdc.calls()["N1MM"] == 1 })

	if got := fdc.requests(); got != sent+1 {
		t.Errorf("the server received %d contacts after the restart, want 1", got-sent)
	}
}

func TestLoggerEditsAndDeletesReachTheServerAsOneContact(t *testing.T) {
	logger := newFakeN3FJP(t)
	fdc := newFakeFDC()
	server := httptest.NewServer(fdc)
	defer server.Close()

	startRelay(t, filepath.Join(t.TempDir(), "relay.db"), server.URL, logger.listener.Addr().String())
	eventually(t, "the logger connection", func() bool {
		logger.mu.Lock()
		defer logger.mu.Unlock()
		return logger.conn != nil
	})

	logger.add(1, "W1WA")
	logger.add(2, "K1ABC")
	eventually(t, "two contacts", func() bool { return len(fdc.calls()) == 2 })

	logger.broadcast(tx("EDIT", record("W1AW", 1)))
	eventually(t, "the correction", func() bool {
		calls := fdc.calls()
		return calls["W1AW"] == 1 && calls["W1WA"] == 0 && len(calls) == 2
	})

	logger.broadcast(tx("DELETE", "2"))
	eventually(t, "the delete", func() bool {
		calls := fdc.calls()
		return calls["K1ABC"] == 0 && len(calls) == 1
	})
}
