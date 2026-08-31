package service

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/thatSFguy/reticulum-go/rns"
	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
)

// captureIface is an rns.Interface that keeps every frame the transport
// broadcasts, with the moment it went out.
//
// Announces are asserted here as WIRE BYTES rather than as log lines,
// because the log is what said "announced" twice while both packets
// left on the same tick from the same identity. What a destination
// announces is a fact about the packet.
type captureIface struct {
	mu     sync.Mutex
	start  time.Time
	frames []capturedFrame
	inbox  chan []byte
	done   chan struct{}
}

type capturedFrame struct {
	at  time.Duration
	raw []byte
}

func newCaptureIface() *captureIface {
	return &captureIface{
		start: time.Now(),
		inbox: make(chan []byte),
		done:  make(chan struct{}),
	}
}

func (c *captureIface) Send(packet []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, capturedFrame{at: time.Since(c.start), raw: bytes.Clone(packet)})
	return nil
}

func (c *captureIface) Inbox() <-chan []byte  { return c.inbox }
func (c *captureIface) Done() <-chan struct{} { return c.done }

// announcesFor returns the times at which destHash was announced.
func (c *captureIface) announcesFor(t *testing.T, destHash []byte) []time.Duration {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, f := range c.frames {
		pkt, err := rns.ParsePacket(f.raw)
		if err != nil {
			t.Fatalf("broadcast a frame that does not parse: %v", err)
		}
		if bytes.Equal(pkt.DestHash, destHash) {
			out = append(out, f.at)
		}
	}
	return out
}

// announcedKeyFor returns the public key inside the announce for
// destHash — SPEC §4.2, the body opens with the 64-byte key.
func (c *captureIface) announcedKeyFor(t *testing.T, destHash []byte) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.frames {
		pkt, err := rns.ParsePacket(f.raw)
		if err != nil {
			t.Fatalf("broadcast a frame that does not parse: %v", err)
		}
		if !bytes.Equal(pkt.DestHash, destHash) {
			continue
		}
		if len(pkt.Data) < 64 {
			t.Fatalf("announce body for %x is %d bytes, too short to carry a key", destHash, len(pkt.Data))
		}
		return pkt.Data[:64]
	}
	t.Fatalf("%x was never announced", destHash)
	return nil
}

// The two destinations must announce DIFFERENT public keys.
//
// This is the assertion the identity split actually rests on, and the
// one the tests around Service.lxmfDest do not make: that field is
// derived in New, while the announce is built somewhere else entirely.
// Point either builder back at s.identity and every other test still
// passes while the hub announces both destinations under one key again
// — which is the whole condition a client could collapse into a single
// entry (see Config.LXMFIdentityPath).
func TestTheTwoAnnouncesCarryDifferentIdentities(t *testing.T) {
	svc := newTestService(t, notifyingConfig(t.TempDir()))
	cap := newCaptureIface()
	svc.transport.AddInterface(cap)

	svc.announceOnce()
	svc.announceDelivery()

	hubKey := cap.announcedKeyFor(t, svc.destHash)
	lxmfKey := cap.announcedKeyFor(t, svc.lxmfDest)

	if !bytes.Equal(hubKey, svc.identity.PublicKey()) {
		t.Error("rrc.hub was not announced under the hub identity")
	}
	if !bytes.Equal(lxmfKey, svc.lxmfIdentity.PublicKey()) {
		t.Error("lxmf.delivery was not announced under the notification identity")
	}
	if bytes.Equal(hubKey, lxmfKey) {
		t.Error("both destinations announced the same public key — the split exists to prevent exactly this")
	}
}

// The two announces must never leave on the same tick.
//
// They used to: announceLoop called announceOnce and announceDelivery
// back to back, putting two packets on one interface in the same
// millisecond. A receiving stack reads that as a burst rather than as
// two independent destinations, and RNS announce rate control is per
// interface — so a burst is the shape most likely to be queued or
// dropped, invisibly, since SPEC §4.5 announces are unacknowledged.
//
// Asserted as a minimum SEPARATION rather than as exact timestamps: the
// contract is that the two never coincide, and a scheduler under -race
// is not a stopwatch.
func TestTheDeliveryAnnounceNeverSharesATickWithTheHub(t *testing.T) {
	const iv = 300 * time.Millisecond

	h := notifyingConfig(t.TempDir())
	h.AnnounceInterval = config.Duration{Duration: iv}
	svc := newTestService(t, h)
	cap := newCaptureIface()
	svc.transport.AddInterface(cap)

	// Three hub ticks (300/600/900ms) and the delivery announces that
	// fall between them (150/450/750ms).
	ctx, cancel := context.WithTimeout(context.Background(), iv*35/10)
	defer cancel()
	go svc.announceLoop(ctx)
	<-ctx.Done()

	hubAt := cap.announcesFor(t, svc.destHash)
	lxmfAt := cap.announcesFor(t, svc.lxmfDest)
	if len(hubAt) < 2 || len(lxmfAt) < 2 {
		t.Fatalf("expected repeated announces of both destinations, got %d rrc.hub and %d lxmf.delivery in %v",
			len(hubAt), len(lxmfAt), iv*35/10)
	}

	// The delivery destination leads: its loop waits iv/2, so its first
	// announce lands before the hub's first full tick. This is what
	// makes the offset an offset rather than a coincidence.
	if lxmfAt[0] >= hubAt[0] {
		t.Errorf("first lxmf.delivery announce at %v did not lead the first rrc.hub announce at %v",
			lxmfAt[0], hubAt[0])
	}

	// And no pair of them coincides.
	const minGap = iv / 4
	for _, l := range lxmfAt {
		for _, hh := range hubAt {
			if gap := l - hh; gap < minGap && gap > -minGap {
				t.Errorf("announces %v apart (rrc.hub at %v, lxmf.delivery at %v); want at least %v",
					gap, hh, l, minGap)
			}
		}
	}
}
