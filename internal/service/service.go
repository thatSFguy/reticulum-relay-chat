// Package service wires the transport-agnostic RRC hub to a live
// Reticulum stack: it owns the RNS identity, attaches TCP interfaces,
// registers the rrc.hub destination, announces it, and routes inbound
// link DATA to per-link hub sessions.
package service

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/hub"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rns"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// hubAspect is the RNS destination aspect an RRC hub announces under.
// name_hash = SHA-256("rrc.hub")[:10] = ac9fd3a81e4036f86e1d.
const hubAspect = "rrc.hub"

// Decrypted lengths of a §6.6 LINKIDENTIFY frame. The signature covers
// link_id || public_key in both forms.
//
//   - identifyLenUpstream (128): public_key(64) || signature(64) — the
//     form upstream RNS link.identify() sends; every spec-compliant
//     client (e.g. the Python RRC desktop client) uses this.
//   - identifyLenLegacy (144): link_id(16) || public_key(64) ||
//     signature(64) — a non-standard form older reticulum-mobile-app
//     builds still send. Accepted until those clients ship the fix.
const (
	identifyLenUpstream = 64 + ed25519.SignatureSize
	identifyLenLegacy   = 16 + 64 + ed25519.SignatureSize
)

// isIdentifyLen reports whether a decrypted link frame is a LINKIDENTIFY
// by its length (either accepted form).
func isIdentifyLen(n int) bool {
	return n == identifyLenUpstream || n == identifyLenLegacy
}

// resourceSendTimeout bounds one outbound RNS Resource transfer to a
// client. Generous enough for a slow mesh link to complete the
// ADV/REQ/PART/PRF round-trips; on expiry rnsLink.SendResource returns
// an error and the hub falls back to chunked NOTICEs.
const resourceSendTimeout = 30 * time.Second

// parseIdentifyFrame extracts the public key and signature from a
// decrypted LINKIDENTIFY payload, accepting both the 128-byte upstream
// form and the 144-byte legacy form. ok is false for any other length,
// or for a legacy frame whose embedded link_id does not match linkID.
func parseIdentifyFrame(linkID, plaintext []byte) (pubKey, sig []byte, ok bool) {
	switch len(plaintext) {
	case identifyLenUpstream:
		return plaintext[0:64], plaintext[64:128], true
	case identifyLenLegacy:
		if !equalBytes(plaintext[0:16], linkID) {
			return nil, nil, false
		}
		return plaintext[16:80], plaintext[80:144], true
	default:
		return nil, nil, false
	}
}

// Service is the running RRC hub daemon.
type Service struct {
	cfg       *config.Config
	log       *log.Logger
	identity  *rns.Identity
	transport *rns.Transport
	hub       *hub.Hub
	destHash  []byte

	mu         sync.Mutex
	sessions   map[string]*hub.Session // linkID hex -> session
	identities map[string][]byte       // linkID hex -> verified peer identity hash
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
		identities: make(map[string][]byte),
	}
	svc.hub = hub.New(id.Hash(), cfg.Hub, logger)

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
	svc.transport.LinkManager().SetDefaultInboundDataHandler(svc.onLinkData)
	svc.transport.LinkManager().SetResourceAssembledHandler(svc.onResourceAssembled)

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
		tc, err := rns.DialTCP(iface.Address, 15*time.Second)
		if err != nil {
			return fmt.Errorf("dial %s: %w", iface.Address, err)
		}
		s.transport.AddInterface(tc)
		s.log.Printf("attached tcp_client %s", iface.Address)
	}

	go s.transport.Run(ctx)
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
	}
	s.log.Printf("RRC hub running — add this hub in a client by hash: %s", s.DestHashHex())

	<-ctx.Done()
	s.log.Printf("shutdown: closing %d session(s)", s.hub.SessionCount())
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

func (s *Service) announceLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.Hub.AnnounceInterval.Duration)
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
	if _, err := rrc.Decode(plaintext); err == nil {
		s.sessionFor(linkID).OnInbound(plaintext)
		return
	}
	if isIdentifyLen(len(plaintext)) {
		s.handleIdentify(linkID, plaintext)
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
func (s *Service) sessionFor(linkID []byte) *hub.Session {
	key := hex.EncodeToString(linkID)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[key]
	if sess == nil {
		sess = s.hub.NewSession(&rnsLink{svc: s, linkID: append([]byte(nil), linkID...)})
		s.sessions[key] = sess
	}
	return sess
}

// handleIdentify parses a §6.6 LINKIDENTIFY frame (either accepted
// form — see parseIdentifyFrame), verifies the Ed25519 signature over
// link_id || public_key, and binds the verified identity hash
// (SHA-256(public_key)[:16]) to the link. The public key is carried in
// the frame, so verification never depends on a prior announce.
func (s *Service) handleIdentify(linkID, plaintext []byte) {
	pubKey, sig, ok := parseIdentifyFrame(linkID, plaintext)
	if !ok {
		s.log.Printf("link %x: malformed LINKIDENTIFY (%d bytes) — dropped",
			linkID[:4], len(plaintext))
		return
	}
	signed := make([]byte, 0, len(linkID)+len(pubKey))
	signed = append(signed, linkID...)
	signed = append(signed, pubKey...)
	if !ed25519.Verify(ed25519.PublicKey(pubKey[32:]), signed, sig) {
		s.log.Printf("link %x: LINKIDENTIFY signature invalid — dropped", linkID[:4])
		return
	}

	h := sha256.Sum256(pubKey)
	idHash := append([]byte(nil), h[:16]...)
	s.mu.Lock()
	s.identities[hex.EncodeToString(linkID)] = idHash
	s.mu.Unlock()
	s.log.Printf("link %x identified as %s (verified)", linkID[:4], hex.EncodeToString(idHash))
}

func (s *Service) peerIdentity(linkID []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identities[hex.EncodeToString(linkID)]
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
