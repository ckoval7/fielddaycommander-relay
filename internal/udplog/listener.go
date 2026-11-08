// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package udplog receives contacts that loggers send as UDP datagrams: plain ADIF
// text, which fldigi, Log4OM, WSJT-X's secondary server and others can send, and
// WSJT-X's own protocol, which JTDX shares. It only listens and never answers, so
// nothing can reach a logger from here.
//
// UDP has no second chance. Anything that arrives is held in memory only until
// it's saved, and shutting down saves what's waiting. Neither format has edits or
// deletes: a contact sent again is the same contact, and one sent with a corrected
// call, time, band or mode is a new one.
package udplog

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/adif"
	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

// Kind is which of the two formats a listener takes.
type Kind int

// The formats.
const (
	// ADIF is plain ADIF text, one or more records to a datagram.
	ADIF Kind = iota
	// WSJTX is WSJT-X's binary protocol.
	WSJTX
)

// The ports the server's listeners use by default. Neither format has a password,
// so other PCs can't reach the relay until someone turns that on.
const (
	adifPort  = 2238
	wsjtxPort = 2237
)

const (
	readBufferBytes = 1 << 16
	socketBuffer    = 1 << 20
	// maxPacketBytes holds dozens of ADIF records, and keeps what's waiting small
	// when anything on the network can send.
	maxPacketBytes = 16 << 10
	// waitingPackets covers a burst while each contact waits to be saved to disk.
	waitingPackets = 1024
	// batchPackets is the most saved in one commit. Each commit waits for the disk,
	// which on a slow PC can take longer than a burst takes to arrive.
	batchPackets = 256
	retryBind    = 5 * time.Second
	retryStore   = time.Second
)

// DefaultPort is the port a kind listens on unless someone picks another.
func (k Kind) DefaultPort() int {
	if k == WSJTX {
		return wsjtxPort
	}
	return adifPort
}

// Address is where a kind listens on port: this computer only, or every network.
// Port 0 means the default.
func (k Kind) Address(onNetwork bool, port int) string {
	if port == 0 {
		port = k.DefaultPort()
	}
	host := "127.0.0.1"
	if onNetwork {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Source identifies a kind in the relay's records, with the same names the
// server's listeners use.
func (k Kind) Source() string {
	if k == WSJTX {
		return "wsjtx"
	}
	return "udp-adif"
}

// Store keeps what the listener receives. Put must have made the contact durable
// before it returns.
type Store interface {
	Put(ctx context.Context, c contact.Contact) (queued bool, err error)
	PutInvalid(ctx context.Context, source, reason, raw string) error
	// EventPeriod returns the event's dates, or false until the relay has learned
	// them from the server.
	EventPeriod(ctx context.Context) (start, end time.Time, known bool, err error)
	// Batch commits everything work saves through its context together, or nothing.
	Batch(ctx context.Context, work func(ctx context.Context) error) error
}

// Status is a snapshot for the status page.
type Status struct {
	Listening bool
	// Address is where it's listening.
	Address string
	// Contacts is how many new contacts or changes arrived since the relay started.
	Contacts int
	// LastContact is when the last of those arrived.
	LastContact time.Time
	// Outside is how many arrived from outside the event's dates, which aren't sent.
	Outside int
	// Dropped is how many datagrams arrived too fast to keep.
	Dropped int
}

// Listener receives one kind of datagram.
type Listener struct {
	kind    Kind
	address string
	store   Store
	notify  func()
	log     *slog.Logger

	// Shortened by tests.
	bindWait  time.Duration
	storeWait time.Duration

	statusMu sync.Mutex
	status   Status
}

type datagram struct {
	data []byte
	from netip.Addr
}

// New returns a Listener for address. notify, when set, is called after a contact
// is queued.
func New(kind Kind, address string, store Store, notify func(), log *slog.Logger) *Listener {
	return &Listener{
		kind: kind, address: address, store: store, notify: notify, log: log.With("source", kind.Source()),
		bindWait: retryBind, storeWait: retryStore,
	}
}

// Status returns the current status.
func (l *Listener) Status() Status {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	return l.status
}

// Run listens until ctx is canceled, and saves what's waiting before it returns.
func (l *Listener) Run(ctx context.Context) {
	waiting := make(chan datagram, waitingPackets)
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		for first := range waiting {
			l.save(ctx, waitingAfter(first, waiting))
		}
	}()

	for ctx.Err() == nil {
		if err := l.listen(ctx, waiting); err != nil && ctx.Err() == nil {
			l.log.Warn("can't listen for contacts", "address", l.address, "error", err)
		}
		l.setStatus(func(status *Status) { status.Listening = false })
		select {
		case <-ctx.Done():
		case <-time.After(l.bindWait):
		}
	}

	close(waiting)
	<-saved
}

func (l *Listener) listen(ctx context.Context, waiting chan<- datagram) error {
	var config net.ListenConfig
	packetConn, err := config.ListenPacket(ctx, "udp4", l.address)
	if err != nil {
		return err
	}
	defer packetConn.Close()
	conn, ok := packetConn.(*net.UDPConn)
	if !ok {
		return errors.New("not a UDP socket")
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := conn.SetReadBuffer(socketBuffer); err != nil {
		l.log.Warn("couldn't enlarge the receive buffer", "error", err)
	}
	l.setStatus(func(status *Status) {
		status.Listening = true
		status.Address = conn.LocalAddr().String()
	})

	buffer := make([]byte, readBufferBytes)
	for {
		size, from, err := conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Windows can report a failure from an earlier packet on a UDP socket. The
			// socket itself is still fine.
			l.log.Warn("reading a datagram failed", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		l.receive(buffer[:size], from.Addr(), waiting)
	}
}

// receive keeps datagrams that may hold a contact and hands them over without
// waiting, because waiting here would lose whatever arrives meanwhile. WSJT-X
// sends many other messages, such as a heartbeat every 15 seconds.
func (l *Listener) receive(data []byte, from netip.Addr, waiting chan<- datagram) {
	if len(data) > maxPacketBytes {
		l.setStatus(func(status *Status) { status.Dropped++ })
		l.log.Error("a datagram was too large to keep", "bytes", len(data))
		return
	}
	if l.kind == WSJTX {
		if _, _, ok := wsjtxLogged(data); !ok {
			return
		}
	} else if !looksLikeADIF(data) {
		return
	}
	select {
	case waiting <- datagram{data: append([]byte(nil), data...), from: from}:
	default:
		l.setStatus(func(status *Status) { status.Dropped++ })
		l.log.Error("contacts arrived too fast to keep", "bytes", len(data))
	}
}

// waitingAfter returns first and whatever else is already waiting, up to a batch.
func waitingAfter(first datagram, waiting <-chan datagram) []datagram {
	batch := []datagram{first}
	for len(batch) < batchPackets {
		select {
		case received, ok := <-waiting:
			if !ok {
				return batch
			}
			batch = append(batch, received)
		default:
			return batch
		}
	}
	return batch
}

// save keeps trying, because this is the only copy of the datagrams. Saving a
// contact again changes nothing, so a retry can start from the top. Shutting down
// is the one thing that ends the tries early.
func (l *Listener) save(ctx context.Context, batch []datagram) {
	storeCtx := context.WithoutCancel(ctx)
	for {
		err := l.handle(storeCtx, batch...)
		if err == nil {
			return
		}
		l.log.Error("saving a contact failed", "error", err)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(l.storeWait):
		}
	}
}

// handle saves datagrams in one commit, then counts what they held. A failed commit
// saves none of them, and a retry starts over, so nothing is counted twice.
func (l *Listener) handle(ctx context.Context, batch ...datagram) error {
	queued, outside := 0, 0
	err := l.store.Batch(ctx, func(ctx context.Context) error {
		queued, outside = 0, 0
		for _, received := range batch {
			q, o, err := l.handleDatagram(ctx, received)
			if err != nil {
				return err
			}
			queued, outside = queued+q, outside+o
		}
		return nil
	})
	if err != nil {
		return err
	}
	l.setStatus(func(status *Status) { status.Outside += outside })
	l.queued(queued)
	return nil
}

// handleDatagram saves what a datagram holds, and returns how many contacts it
// queued and how many were outside the event.
func (l *Listener) handleDatagram(ctx context.Context, received datagram) (queued, outside int, err error) {
	text, station := received.data, func(fields map[string]string) string { return adifStation(fields, received.from) }
	if l.kind == WSJTX {
		id, logged, _ := wsjtxLogged(received.data)
		text = logged
		name := wsjtxStation(id, received.from)
		station = func(map[string]string) string { return name }
	}
	records, _, _ := adif.Scan(text)
	if len(records) == 0 {
		return 0, 0, nil
	}

	start, end, known, err := l.store.EventPeriod(ctx)
	if err != nil {
		return 0, 0, err
	}
	inEvent := func(when time.Time) bool { return !known || (!when.Before(start) && !when.After(end)) }

	for _, record := range records {
		result, err := l.saveRecord(ctx, record, station(record.Fields), inEvent)
		if err != nil {
			return 0, 0, err
		}
		switch result {
		case recordQueued:
			queued++
		case recordOutside:
			outside++
		}
	}
	return queued, outside, nil
}

type recordResult int

const (
	recordKept recordResult = iota
	recordQueued
	recordOutside
)

// saveRecord saves one record as a contact, or as one for someone to check if it
// can't be used. A contact outside the event isn't saved.
func (l *Listener) saveRecord(ctx context.Context, record adif.Record, station string, inEvent func(time.Time) bool) (recordResult, error) {
	candidate, when, err := adif.ToContact(record.Fields)
	if err == nil && !inEvent(when) {
		return recordOutside, nil
	}
	var normalized contact.Contact
	if err == nil {
		candidate.Station = station
		normalized, _, err = contact.Normalize(candidate)
	}
	if err != nil {
		if !errors.Is(err, contact.ErrInvalid) {
			return recordKept, err
		}
		return recordKept, l.store.PutInvalid(ctx, l.kind.Source(), err.Error(), record.Raw)
	}
	isNew, err := l.store.Put(ctx, normalized)
	if err != nil || !isNew {
		return recordKept, err
	}
	return recordQueued, nil
}

func (l *Listener) queued(count int) {
	if count == 0 {
		return
	}
	l.setStatus(func(status *Status) {
		status.Contacts += count
		status.LastContact = time.Now()
	})
	if l.notify != nil {
		l.notify()
	}
}

func (l *Listener) setStatus(change func(*Status)) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	change(&l.status)
}
