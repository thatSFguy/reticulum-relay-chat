// Package service wires the transport-agnostic RRC hub to a live
// Reticulum stack: it owns the RNS identity, attaches TCP interfaces,
// registers the rrc.hub destination, announces it, and routes inbound
// link DATA to per-link hub sessions.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/thatSFguy/reticulum-go/lxmf"
	"github.com/thatSFguy/reticulum-go/rns"
	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/hub"
	"github.com/thatSFguy/reticulum-relay-chat/internal/lxmfaddr"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// hubAspect is the RNS destination aspect an RRC hub announces under.
// name_hash = SHA-256("rrc.hub")[:10] = ac9fd3a81e4036f86e1d.
const hubAspect = "rrc.hub"

// LINKIDENTIFY (SPEC §6.7.6) is no longer parsed here.
//
// reticulum-go >= v0.4.0 handles context 0xFB inside the Transport —
// it decrypts the frame, verifies the Ed25519 signature over
// link_id || public_key, records the key on the Link, and CONSUMES the
// packet, so the frame never reaches SetDefaultInboundDataHandler. The
// hub therefore learns about identification through
// SetRemoteIdentifiedHandler (see bindPeer) instead of sniffing link
// DATA for a 128-byte body.
//
// One behaviour is lost in the move. RRC used to also accept a
// non-standard 144-byte form, link_id(16) || public_key(64) ||
// signature(64), that older reticulum-mobile-app builds sent. The
// library is spec-pure and rejects it, and because it consumes the
// packet there is no hook through which RRC can be lenient. Current
// mobile builds send the spec form — Link.kt buildIdentifyPayload
// documents the 144-byte layout as the bug it was — so this costs only
// clients that have not been rebuilt since.

// resourceSendTimeout bounds one outbound RNS Resource transfer to a
// client. Generous enough for a slow mesh link to complete the
// ADV/REQ/PART/PRF round-trips; on expiry rnsLink.SendResource returns
// an error and the hub falls back to chunked NOTICEs.
const resourceSendTimeout = 30 * time.Second

// maxLinkFrame caps an inbound link-DATA frame before it reaches the
// CBOR decoder. Every RRC envelope rides a single Reticulum link DATA
// packet, bounded by the link MDU (< 500 bytes); a frame far above that
// is malformed and must not be decoded (audit A4). Large payloads use
// the RNS Resource path, which is handled separately.
const maxLinkFrame = 8 * 1024

// Service is the running RRC hub daemon.
type Service struct {
	cfg       *config.Config
	log       *log.Logger
	identity  *rns.Identity
	transport *rns.Transport
	hub       *hub.Hub
	destHash  []byte

	// mu guards sessions and identities.
	//
	// LOCK ORDERING (audit A16): Service.mu is a LEAF relative to the
	// hub's mutex — Session.identity() reaches back through rnsLink into
	// Service.peerIdentity, which takes Service.mu, and that happens in
	// several places while hub.mu is held. Therefore code holding
	// Service.mu must NEVER call into the hub in a way that takes
	// hub.mu. Acquire order is always hub.mu → Service.mu, never the
	// reverse. Violating this reintroduces the sessionFor deadlock.
	// lxmfDest is the hub's own lxmf.delivery destination hash, set only
	// when mention notification over LXMF is enabled. nil means there is
	// nothing to announce.
	lxmfDest []byte

	// notifier is the installed LXMF notifier, kept so the inbound
	// auto-reply can send over the same destination.
	notifier *lxmfNotifier

	// lastInboundReply throttles the auto-reply to somebody who
	// messaged the notification address, per sender.
	lastInboundReply map[string]time.Time

	mu         sync.Mutex
	sessions   map[string]*hub.Session // linkID hex -> session
	identities map[string]peerBinding  // linkID hex -> what LINKIDENTIFY proved
}

// peerBinding is what a verified §6.7.6 LINKIDENTIFY establishes about the
// remote end of a link.
//
// The public key is retained, not just the hash it reduces to: the hash
// identifies a peer but cannot address one, while the key yields the
// peer's LXMF delivery destination (see internal/lxmfaddr) — the only
// way this hub can reach someone whose link has since gone away.
type peerBinding struct {
	hash   []byte // SHA-256(public_key)[:16] — the envelope K_SRC value
	pubKey []byte // 64 bytes: X25519 public || Ed25519 public
}

// New builds the service: loads (or creates) the hub identity, wires the
// transport and the hub, and registers the rrc.hub destination.
func New(cfg *config.Config, logger *log.Logger) (*Service, error) {
	id, err := loadOrCreateIdentity(cfg.Hub.IdentityPath, logger)
	if err != nil {
		return nil, err
	}
	svc := &Service{
		cfg:        cfg,
		log:        logger,
		identity:   id,
		transport:  rns.NewTransport(logger),
		destHash:   id.DestinationHashFor(hubAspect),
		sessions:   make(map[string]*hub.Session),
		identities: make(map[string]peerBinding),
	}
	svc.hub = hub.New(id.Hash(), cfg.Hub, logger)
	// The hub signs as its identity but clients dial its destination,
	// and only the destination is any use to somebody trying to get
	// back after an offline mention notification.
	svc.hub.SetDestHash(svc.destHash)

	if err := svc.transport.RegisterLocal(&rns.LocalDestination{
		DestHash:      svc.destHash,
		Identity:      id,
		BuildAnnounce: svc.buildAnnounce,
		// RRC carries no opportunistic DATA (spec §17.5) — all traffic
		// rides Links. A non-link DATA packet to the hub destination is
		// unexpected; log and drop it. OnPacket is required by
		// RegisterLocal even when, as here, it is effectively a no-op.
		OnPacket: svc.onPacket,
		// OnLinkPlaintext is left nil so inbound link DATA falls through
		// to the LinkManager's default handler, which also hands us the
		// link_id — we need it to route DATA to the right session.
	}); err != nil {
		return nil, fmt.Errorf("register rrc.hub destination: %w", err)
	}
	// Mention notifications over LXMF, when the operator asked for them.
	// A hub that cannot build the notifier still runs: it falls back to
	// holding mentions until the peer's next RRC session, which is what
	// it does with the feature off.
	if cfg.Hub.MentionNotify && cfg.Hub.MentionLXMF {
		notifier, err := newLXMFNotifier(svc)
		if err != nil {
			logger.Printf("lxmf: mention notifications disabled — %v", err)
		} else {
			svc.hub.SetOfflineNotifier(notifier)
			svc.notifier = notifier
			svc.lxmfDest = id.DestinationHashFor(lxmf.FullName())
		}
	}

	svc.transport.LinkManager().SetDefaultInboundDataHandler(svc.onLinkData)
	svc.transport.LinkManager().SetResourceAssembledHandler(svc.onResourceAssembled)
	// §6.7.6 identification is consumed by the Transport (see the note
	// above bindPeer); this is how the hub hears about it.
	svc.transport.LinkManager().SetRemoteIdentifiedHandler(svc.bindPeer)
	svc.transport.LinkManager().SetLinkClosedHandler(svc.onLinkClosed)

	logger.Printf("RRC hub %q — dest_name=%s dest_hash=%s identity=%s",
		cfg.Hub.Name, hubAspect, hex.EncodeToString(svc.destHash), id.HexHash())
	return svc, nil
}

// DestHashHex is the hub's destination hash — the value clients add.
func (s *Service) DestHashHex() string { return hex.EncodeToString(s.destHash) }

// Run attaches the configured interfaces and runs the hub until ctx is
// cancelled.
func (s *Service) Run(ctx context.Context) error {
	for _, iface := range s.cfg.Interfaces {
		// Reconnecting client, not a bare DialTCP: the hub is a
		// long-lived unattended daemon, so every transport drop it can
		// recover from, it must. A bare client turns a peer restart, a
		// NAT idle eviction, or a single oversized inbound HDLC frame
		// (rns returns ErrFrameTooLarge and drops the connection rather
		// than resynchronizing) into a permanently dead uplink that
		// only a process restart clears.
		tc, err := rns.DialReconnectingTCP(iface.Address, 15*time.Second, s.log)
		if err != nil {
			return fmt.Errorf("dial %s: %w", iface.Address, err)
		}
		s.transport.AddInterface(tc)
		s.log.Printf("attached tcp_client %s", iface.Address)
	}

	transportDone := make(chan struct{})
	go func() {
		s.transport.Run(ctx)
		close(transportDone)
	}()
	go s.transport.RunLinkSweeper(ctx)
	go s.announceLoop(ctx)
	go s.janitor(ctx)

	// The hub owns its own background loops (keepalive PING, room-registry
	// prune, resource-expectation reaper). Start them once the transport
	// is up so a PING never races an un-attached interface.
	s.hub.Start(ctx)

	// Announce immediately only when configured to — otherwise the first
	// announce waits a full announce_interval. The periodic announceLoop
	// runs regardless.
	if s.cfg.Hub.AnnounceOnStart {
		s.announceOnce()
		// The LXMF delivery destination is NOT announced here. It rides
		// its own half-interval-offset schedule (deliveryAnnounceLoop),
		// and announcing it at startup too would recreate the very
		// same-millisecond pair the offset exists to break up — on a hub
		// that gets restarted often, that is most of its announces.
	}
	s.log.Printf("RRC hub running — add this hub in a client by hash: %s", s.DestHashHex())

	<-ctx.Done()
	s.log.Printf("shutdown: closing %d session(s)", s.hub.SessionCount())
	// Wait for the transport dispatch goroutine to finish any in-flight
	// inbound handler before persisting hub state, so a late handler
	// cannot race hub.Stop()'s registry write (audit A9).
	<-transportDone
	// Persist the room registry and klines before exit.
	s.hub.Stop()
	return nil
}

// --- announce ---------------------------------------------------------

func (s *Service) buildAnnounce(context byte) (*rns.Packet, error) {
	return rns.BuildAnnounceWithContext(s.identity, hubAspect, []byte(s.cfg.Hub.Name), nil, context)
}

func (s *Service) announceOnce() {
	pkt, err := rns.BuildAnnounce(s.identity, hubAspect, []byte(s.cfg.Hub.Name), nil)
	if err != nil {
		s.log.Printf("announce build failed: %v", err)
		return
	}
	if err := s.transport.Broadcast(pkt); err != nil {
		s.log.Printf("announce broadcast failed: %v", err)
		return
	}
	s.log.Printf("announced rrc.hub (%s)", s.DestHashHex())
}

// announceLoop re-announces rrc.hub every announce_interval, and starts
// the delivery destination's own loop half an interval behind it.
func (s *Service) announceLoop(ctx context.Context) {
	iv := s.cfg.Hub.AnnounceInterval.Duration
	go s.deliveryAnnounceLoop(ctx, iv)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.announceOnce()
		}
	}
}

// deliveryAnnounceLoop announces lxmf.delivery on the same period as
// rrc.hub, phase-shifted by iv/2.
//
// The two used to fire from ONE tick, microseconds apart. That is two
// announce packets on the same interface in the same millisecond, which
// a receiving stack sees as a burst rather than as two independent
// destinations — and announce rate control is applied per interface, so
// a burst is the shape most likely to be queued or dropped. A dropped
// announce is invisible from the sending side (SPEC §4.5 has no
// acknowledgement), which is why this was never visible in a log that
// said "announced" twice.
//
// Spacing them also halves the worst case for a client that is only
// listening part of the time: with both on one tick, a client that
// misses the tick misses BOTH destinations for a full interval.
func (s *Service) deliveryAnnounceLoop(ctx context.Context, iv time.Duration) {
	// Nothing to announce when mention notification is off — lxmfDest is
	// set only when the notifier was built (see New).
	if s.lxmfDest == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(iv / 2):
	}
	s.announceDelivery()
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.announceDelivery()
		}
	}
}

// onPacket handles inbound non-link DATA addressed to the hub. RRC uses
// no opportunistic packets, so anything arriving here is unexpected.
func (s *Service) onPacket(p *rns.Packet) {
	s.log.Printf("rrc: ignoring unexpected non-link DATA packet (context=0x%02x)", p.Context)
}

// --- inbound link DATA routing ---------------------------------------

// onLinkData is the LinkManager default handler: it receives every
// decrypted inbound link-DATA payload tagged with its link_id. An RRC
// CBOR envelope is routed to the link's hub session; a LINKIDENTIFY
// frame is used to bind the peer's verified identity.
func (s *Service) onLinkData(linkID, plaintext []byte) {
	if len(plaintext) > maxLinkFrame {
		s.log.Printf("rrc: dropped oversized %d-byte link frame on %x", len(plaintext), linkID[:4])
		return
	}
	if _, err := rrc.Decode(plaintext); err == nil {
		s.sessionFor(linkID).OnInbound(plaintext)
		return
	}
	s.log.Printf("rrc: dropped %d-byte non-RRC link frame on %x", len(plaintext), linkID[:4])
}

// onResourceAssembled receives the reassembled body of an inbound RNS
// Resource a client advertised, routed by link_id. It hands the payload
// to the link's hub session, which matches it to a pending
// RESOURCE_ENVELOPE expectation. The rns ResourceReceiver has already
// verified the transfer's integrity before this fires.
func (s *Service) onResourceAssembled(linkID, body []byte) {
	s.log.Printf("rrc: inbound resource assembled on %x (%d bytes)", linkID[:4], len(body))
	s.sessionFor(linkID).OnResourceConcluded(body)
}

// sessionFor returns the hub session for a link, creating it on first
// reference.
//
// hub.NewSession must be called WITHOUT s.mu held: it calls back into the
// link (PeerIdentityHash for the banned-peer check, and Close for a
// banned peer), and those re-enter s.mu via peerIdentity/dropSession.
// Holding s.mu across NewSession self-deadlocks the transport dispatch
// goroutine — which silently wedges all inbound packet processing.
func (s *Service) sessionFor(linkID []byte) *hub.Session {
	key := hex.EncodeToString(linkID)
	s.mu.Lock()
	sess := s.sessions[key]
	s.mu.Unlock()
	if sess != nil {
		return sess
	}

	created := s.hub.NewSession(&rnsLink{svc: s, linkID: append([]byte(nil), linkID...)})

	s.mu.Lock()
	defer s.mu.Unlock()
	// Inbound DATA for one link is dispatched by a single goroutine, so
	// a creation race is not expected; the re-check is defensive — on a
	// lost race the winner is authoritative (closing the loser would
	// tear down the shared link).
	if existing := s.sessions[key]; existing != nil {
		return existing
	}
	s.sessions[key] = created
	return created
}

// bindPeer records what a verified §6.7.6 LINKIDENTIFY proved about a
// link: the peer's 64-byte announced public key, and the identity hash
// it reduces to.
//
// It is the LinkManager's remote-identified callback, so the signature
// over link_id || public_key has already been checked (and the key
// pinned assign-once) inside reticulum-go before this runs. What is
// left is RRC's own bookkeeping — and specifically retaining the KEY,
// not just its hash: the hash names the peer, but only the key derives
// the peer's lxmf.delivery address, which is the sole way this hub can
// reach somebody after their link is gone.
func (s *Service) bindPeer(linkID, pubKey []byte) {
	if len(pubKey) != rns.PublicKeyLen {
		s.log.Printf("link %x: LINKIDENTIFY carried a %d-byte key — ignored", linkID[:4], len(pubKey))
		return
	}
	h := sha256.Sum256(pubKey)
	idHash := append([]byte(nil), h[:rns.IdentityHashLen]...)
	s.mu.Lock()
	s.identities[hex.EncodeToString(linkID)] = peerBinding{
		hash:   idHash,
		pubKey: append([]byte(nil), pubKey...),
	}
	s.mu.Unlock()
	// reticulum-go has already logged the verification. What is worth
	// adding is the part that is ours: the peer is now addressable when
	// their link is gone, because the KEY was retained and not just the
	// hash it reduces to.
	if dest, err := lxmfaddr.DeliveryDest(pubKey); err == nil {
		s.log.Printf("link %x bound to %s (reachable offline at lxmf.delivery %x)",
			linkID[:4], hex.EncodeToString(idHash), dest)
	} else {
		s.log.Printf("link %x bound to %s", linkID[:4], hex.EncodeToString(idHash))
	}
}

// onLinkClosed reaps the session behind a link the moment it closes,
// rather than up to 30 seconds later when the janitor next looks.
//
// That delay was not cosmetic. Room membership is what the hub answers
// "is this person here?" with, and for the whole window the answer was
// wrong in the one direction that loses messages: a mention aimed at
// somebody who had just disconnected was treated as delivered by the
// room fan-out, so it was neither shown to them nor queued for their
// return. Observed live at 56 seconds before reticulum-go v0.7.0 gave
// us anything to hook.
//
// The janitor stays. This fires only for a link whose closure was
// OBSERVED — a §6.7.3 LINKCLOSE, or the library's own watchdog — and a
// client that vanishes without either (dead battery, lost signal) is
// still found by polling. Presence remains an estimate; this narrows
// the window rather than closing it, which is why
// hub.mentionLivenessProven is still load-bearing.
func (s *Service) onLinkClosed(linkID []byte, reason byte) {
	// A local close is our own doing, and the session teardown that
	// decided on it is already unwinding — rnsLink.Close calls
	// CloseLink, which is what got us here. Re-entering would be a
	// second pass over state the first has not finished with.
	if reason == rns.TeardownLocalClosed {
		return
	}
	key := hex.EncodeToString(linkID)
	s.mu.Lock()
	sess := s.sessions[key]
	s.mu.Unlock()
	if sess == nil {
		return
	}
	s.log.Printf("link %x closed (reason 0x%02x) — closing session now", linkID[:4], reason)
	// Outside the lock: Close re-enters the service through
	// rnsLink.Close -> dropSession, and the hub's own teardown calls
	// back in for the peer identity.
	sess.Close()
}

func (s *Service) peerIdentity(linkID []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identities[hex.EncodeToString(linkID)].hash
}

// peerPublicKey returns the 64-byte public key the link's peer proved
// over LINKIDENTIFY, or nil if it has not identified.
func (s *Service) peerPublicKey(linkID []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identities[hex.EncodeToString(linkID)].pubKey
}

// sendOnLink encrypts an RRC frame under a link's session keys and
// broadcasts it as CTX_NONE link DATA.
func (s *Service) sendOnLink(linkID, frame []byte) error {
	l := s.transport.LinkManager().Get(linkID)
	if l == nil {
		return errors.New("link no longer active")
	}
	pkt, err := rns.BuildLinkDataPacket(l.ID, l.Signing, l.Encryption, frame)
	if err != nil {
		return fmt.Errorf("build link DATA: %w", err)
	}
	return s.transport.Broadcast(pkt)
}

func (s *Service) dropSession(linkID []byte) {
	key := hex.EncodeToString(linkID)
	s.mu.Lock()
	delete(s.sessions, key)
	delete(s.identities, key)
	s.mu.Unlock()
}

// janitor closes hub sessions whose underlying RNS link has expired, so
// rooms shed members that silently went away.
func (s *Service) janitor(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepDeadLinks()
		}
	}
}

func (s *Service) sweepDeadLinks() {
	lm := s.transport.LinkManager()
	type dead struct {
		sess   *hub.Session
		linkID []byte
	}
	var victims []dead
	s.mu.Lock()
	for key, sess := range s.sessions {
		linkID, err := hex.DecodeString(key)
		if err != nil {
			continue
		}
		if l := lm.Get(linkID); l == nil || !l.IsActive() {
			victims = append(victims, dead{sess, linkID})
		}
	}
	s.mu.Unlock()
	for _, v := range victims {
		s.log.Printf("janitor: link %x expired — closing session", v.linkID[:4])
		v.sess.Close() // -> rnsLink.Close -> dropSession
	}
}

// --- hub.Link adapter -------------------------------------------------

// rnsLink adapts one RNS link to the hub.Link interface.
type rnsLink struct {
	svc    *Service
	linkID []byte
}

func (l *rnsLink) Send(frame []byte) error { return l.svc.sendOnLink(l.linkID, frame) }

func (l *rnsLink) Close() {
	l.svc.transport.LinkManager().CloseLink(l.linkID)
	l.svc.dropSession(l.linkID)
}

func (l *rnsLink) PeerIdentityHash() []byte { return l.svc.peerIdentity(l.linkID) }

func (l *rnsLink) PeerPublicKey() []byte { return l.svc.peerPublicKey(l.linkID) }

// SendResource delivers payload to the client as an RNS Resource over
// this link (SPEC §10). The hub has already sent the matching
// RESOURCE_ENVELOPE; this drives the actual Resource transfer.
//
// The internal/rns package fully implements the responder-side Resource
// sender (Transport.SendResourceOverLink): it builds the encrypted
// parts, advertises the resource, fulfills RESOURCE_REQ part requests,
// and blocks until the client returns a valid RESOURCE_PRF. On any
// failure (link gone, ADV retries exhausted, peer RESOURCE_RCL, proof
// mismatch, timeout) it returns an error and the hub falls back to
// chunked NOTICEs.
func (l *rnsLink) SendResource(payload []byte) error {
	if len(payload) == 0 {
		return errors.New("resource: empty payload")
	}
	link := l.svc.transport.LinkManager().Get(l.linkID)
	if link == nil || !link.IsActive() {
		return errors.New("resource: link no longer active")
	}
	// A responder-side link carries no peerDestHash, so there is no
	// transit relay to route through — pass a nil transport_id. The
	// Resource sender keeps every part at HEADER_1 regardless (see
	// resource_sender.go broadcastAdv), which is correct for a directly
	// reachable client.
	ctx, cancel := context.WithTimeout(context.Background(), resourceSendTimeout)
	defer cancel()
	if err := l.svc.transport.SendResourceOverLink(ctx, link, payload, nil); err != nil {
		return fmt.Errorf("resource send on link %x: %w", l.linkID[:4], err)
	}
	return nil
}

// --- identity ---------------------------------------------------------

func loadOrCreateIdentity(path string, logger *log.Logger) (*rns.Identity, error) {
	if _, err := os.Stat(path); err == nil {
		id, err := rns.IdentityFromFile(path)
		if err != nil {
			return nil, fmt.Errorf("load identity %s: %w", path, err)
		}
		logger.Printf("loaded hub identity from %s", path)
		return id, nil
	}
	id, err := rns.NewIdentity()
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	if err := id.Save(path); err != nil {
		return nil, fmt.Errorf("save identity %s: %w", path, err)
	}
	logger.Printf("generated a new hub identity at %s", path)
	return id, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
