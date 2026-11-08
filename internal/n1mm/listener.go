// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package n1mm receives contacts from N1MM Logger+'s UDP broadcasts. It only
// listens and never sends anything, so nothing can reach N1MM's network from here.
//
// UDP has no second chance. Anything that arrives is held in memory only until
// it's saved, and shutting down saves what's waiting. N1MM doesn't send anything
// that happens while the relay is closed. Its Rebroadcast all Contest QSO's to other Apps command
// sends the whole log again, which fills in new contacts and edits without any
// repeats, but a delete that was missed stays missed.
//
// A delete names no mode, and an edit doesn't say what the band and mode were
// before, so neither can rebuild the key the contact went out under. N1MM's id for
// the contact stays the same through edits, so the relay remembers the key by id.
package n1mm

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
	"github.com/ckoval7/fielddaycommander-relay/internal/queue"
)

// SourceName identifies this source in the relay's records.
const SourceName = "n1mm"

// N1MM's own default port. It has no password, so other PCs can't reach the relay
// until someone turns that on.
const (
	LocalAddress   = "127.0.0.1:12060"
	NetworkAddress = "0.0.0.0:12060"
)

const (
	readBufferBytes = 1 << 16
	socketBuffer    = 4 << 20
	// maxPacketBytes is far more than any contact needs. N1MM's largest packets are
	// the ones the relay doesn't want.
	maxPacketBytes = 8 << 10
	// waitingPackets covers Rebroadcast all Contest QSO's to other Apps, which sends quickly, while each
	// contact waits to be saved to disk.
	waitingPackets = 2048
	// batchPackets is the most saved in one commit. Each commit waits for the disk,
	// which on a slow PC takes longer than a rebroadcast takes to send a contact.
	batchPackets = 256
	retryBind    = 5 * time.Second
	retryStore   = time.Second
)

// Store keeps what the listener receives. Put must have made the contact durable
// before it returns.
type Store interface {
	Put(ctx context.Context, c contact.Contact) (queued bool, err error)
	PutInvalid(ctx context.Context, source, reason, raw string) error
	N1MMRecord(ctx context.Context, id string) (queue.N1MMRecord, bool, error)
	SaveN1MMRecord(ctx context.Context, record queue.N1MMRecord) error
	DeleteN1MMRecord(ctx context.Context, id string) error
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
	// Dropped is how many packets arrived too fast to keep.
	Dropped int
}

// Listener receives N1MM's broadcasts.
type Listener struct {
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

// New returns a Listener for address. notify, when set, is called after a contact
// is queued.
func New(address string, store Store, notify func(), log *slog.Logger) *Listener {
	return &Listener{address: address, store: store, notify: notify, log: log, bindWait: retryBind, storeWait: retryStore}
}

// Status returns the current status.
func (l *Listener) Status() Status {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	return l.status
}

// Run listens until ctx is canceled, and saves what's waiting before it returns.
func (l *Listener) Run(ctx context.Context) {
	packets := make(chan []byte, waitingPackets)
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		for first := range packets {
			l.save(ctx, waitingAfter(first, packets))
		}
	}()

	for ctx.Err() == nil {
		if err := l.listen(ctx, packets); err != nil && ctx.Err() == nil {
			l.log.Warn("can't listen for N1MM", "address", l.address, "error", err)
		}
		l.setStatus(func(status *Status) { status.Listening = false })
		select {
		case <-ctx.Done():
		case <-time.After(l.bindWait):
		}
	}

	close(packets)
	<-saved
}

func (l *Listener) listen(ctx context.Context, packets chan<- []byte) error {
	var config net.ListenConfig
	conn, err := config.ListenPacket(ctx, "udp4", l.address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if udp, ok := conn.(*net.UDPConn); ok {
		if err := udp.SetReadBuffer(socketBuffer); err != nil {
			l.log.Warn("couldn't enlarge the N1MM receive buffer", "error", err)
		}
	}
	l.setStatus(func(status *Status) {
		status.Listening = true
		status.Address = conn.LocalAddr().String()
	})

	buffer := make([]byte, readBufferBytes)
	for {
		size, _, err := conn.ReadFrom(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Windows can report a failure from an earlier packet on a UDP socket. The
			// socket itself is still fine.
			l.log.Warn("reading from N1MM failed", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		l.receive(buffer[:size], packets)
	}
}

// receive picks out contacts and hands them over without waiting, because waiting
// here would lose whatever arrives meanwhile.
func (l *Listener) receive(data []byte, packets chan<- []byte) {
	if len(data) > maxPacketBytes {
		return
	}
	switch rootOf(data) {
	case rootContact, rootReplace, rootDelete:
	default:
		return
	}
	select {
	case packets <- append([]byte(nil), data...):
	default:
		l.setStatus(func(status *Status) { status.Dropped++ })
		l.log.Error("N1MM packets arrived too fast to keep", "bytes", len(data))
	}
}

// waitingAfter returns first and whatever else is already waiting, up to a batch.
func waitingAfter(first []byte, packets <-chan []byte) [][]byte {
	batch := [][]byte{first}
	for len(batch) < batchPackets {
		select {
		case data, ok := <-packets:
			if !ok {
				return batch
			}
			batch = append(batch, data)
		default:
			return batch
		}
	}
	return batch
}

// save keeps trying, because this is the only copy of the packets. Shutting down is
// the one thing that ends the tries early.
func (l *Listener) save(ctx context.Context, batch [][]byte) {
	storeCtx := context.WithoutCancel(ctx)
	for {
		err := l.saveBatch(storeCtx, batch)
		if err == nil {
			return
		}
		l.log.Error("saving N1MM contacts failed", "error", err)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(l.storeWait):
		}
	}
}

// saveBatch saves packets in one commit, then counts what was new. A failed commit
// saves none of them, and a retry starts over, so nothing is counted twice.
func (l *Listener) saveBatch(ctx context.Context, batch [][]byte) error {
	queued := 0
	err := l.store.Batch(ctx, func(ctx context.Context) error {
		queued = 0
		for _, data := range batch {
			count, err := l.handlePacket(ctx, parsePacket(data), string(data))
			if err != nil {
				return err
			}
			queued += count
		}
		return nil
	})
	if err != nil {
		return err
	}
	l.queued(queued)
	return nil
}

// handle saves one packet.
func (l *Listener) handle(ctx context.Context, parsed packet, raw string) error {
	queued, err := l.handlePacket(ctx, parsed, raw)
	if err == nil {
		l.queued(queued)
	}
	return err
}

// handlePacket saves a packet and returns how many contacts it queued.
func (l *Listener) handlePacket(ctx context.Context, parsed packet, raw string) (int, error) {
	if parsed.root == rootDelete {
		return l.handleDelete(ctx, parsed)
	}
	return l.handleContact(ctx, parsed, raw)
}

func (l *Listener) handleContact(ctx context.Context, parsed packet, raw string) (int, error) {
	candidate, err := parsed.toContact()
	var normalized contact.Contact
	if err == nil {
		normalized, _, err = contact.Normalize(candidate)
	}
	if err != nil {
		if !errors.Is(err, contact.ErrInvalid) {
			return 0, err
		}
		return 0, l.store.PutInvalid(ctx, SourceName, err.Error(), raw)
	}

	id := parsed.id()
	if id != "" {
		previous, found, err := l.store.N1MMRecord(ctx, id)
		if err != nil {
			return 0, err
		}
		if found && previous.Key != normalized.IdempotencyKey {
			normalized.ReplacesKey = previous.Key
		}
	}

	queued, err := l.put(ctx, normalized)
	if err != nil || id == "" {
		return queued, err
	}
	return queued, l.store.SaveN1MMRecord(ctx, queue.N1MMRecord{
		ID: id, Key: normalized.IdempotencyKey, Callsign: normalized.Callsign, QSOTime: normalized.QSOTime, Station: normalized.Station,
	})
}

func (l *Listener) handleDelete(ctx context.Context, parsed packet) (int, error) {
	id := parsed.id()
	if id == "" {
		return 0, nil
	}
	record, found, err := l.store.N1MMRecord(ctx, id)
	if err != nil {
		return 0, err
	}
	if !found {
		// Logged while the relay wasn't listening, so it was never sent from here.
		l.log.Info("N1MM deleted a contact the relay never saw", "call", cut(parsed.fields["call"]))
		return 0, nil
	}
	queued, err := l.put(ctx, contact.Contact{
		IdempotencyKey: record.Key, Deleted: true, Callsign: record.Callsign, QSOTime: record.QSOTime, Station: record.Station,
	})
	if err != nil {
		return 0, err
	}
	return queued, l.store.DeleteN1MMRecord(ctx, id)
}

// put saves a contact and returns 1 if it was new or changed.
func (l *Listener) put(ctx context.Context, c contact.Contact) (int, error) {
	queued, err := l.store.Put(ctx, c)
	if err != nil || !queued {
		return 0, err
	}
	return 1, nil
}

// queued counts contacts once they're saved, and wakes the sender for them.
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
