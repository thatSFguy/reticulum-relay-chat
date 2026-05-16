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

// identifyFrameLen is the plaintext length of a §6.6 LINKIDENTIFY frame:
// identity_hash(16) || ed25519_signature(64).
const identifyFrameLen = 16 + ed25519.SignatureSize

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
	svc.hub = hub.New(id.Hash(), hub.Config{
		Name:     cfg.Hub.Name,
		Version:  cfg.Hub.Version,
		Greeting: cfg.Hub.Greeting,
		Limits: rrc.Limits{
			MaxNickBytes:        cfg.Hub.Limits.MaxNickBytes,
			MaxRoomNameBytes:    cfg.Hub.Limits.MaxRoomNameBytes,
			MaxMsgBodyBytes:     cfg.Hub.Limits.MaxMsgBodyBytes,
			MaxRoomsPerSession:  cfg.Hub.Limits.MaxRoomsPerSession,
			RateLimitMsgsPerMin: cfg.Hub.Limits.RateLimitMsgsPerMin,
		},
	}, logger)

	if err := svc.transport.RegisterLocal(&rns.LocalDestination{
		DestHash:      svc.destHash,
		Identity:      id,
		BuildAnnounce: svc.buildAnnounce,
		// OnLinkPlaintext is left nil so inbound link DATA falls through
		// to the LinkManager's default handler, which also hands us the
		// link_id — we need it to route DATA to the right session.
	}); err != nil {
		return nil, fmt.Errorf("register rrc.hub destination: %w", err)
	}
	svc.transport.LinkManager().SetDefaultInboundDataHandler(svc.onLinkData)

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

	// Announce immediately so clients can path to us without waiting a
	// full interval.
	s.announceOnce()
	s.log.Printf("RRC hub running — add this hub in a client by hash: %s", s.DestHashHex())

	<-ctx.Done()
	s.log.Printf("shutdown: closing %d session(s)", s.hub.SessionCount())
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

// --- inbound link DATA routing ---------------------------------------

// onLinkData is the LinkManager default handler: it receives every
// decrypted inbound link-DATA payload tagged with its link_id. An RRC
// CBOR envelope is routed to the link's hub session; an 80-byte frame is
// treated as a §6.6 LINKIDENTIFY and used to bind the peer identity.
func (s *Service) onLinkData(linkID, plaintext []byte) {
	if _, err := rrc.Decode(plaintext); err == nil {
		s.sessionFor(linkID).OnInbound(plaintext)
		return
	}
	if len(plaintext) == identifyFrameLen {
		s.handleIdentify(linkID, plaintext)
		return
	}
	s.log.Printf("rrc: dropped %d-byte non-RRC link frame on %x", len(plaintext), linkID[:4])
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

// handleIdentify parses a LINKIDENTIFY frame, verifies the Ed25519
// signature against a cached announce when one is available, and binds
// the (verified) identity hash to the link.
func (s *Service) handleIdentify(linkID, plaintext []byte) {
	idHash := plaintext[:16]
	sig := plaintext[16:]
	verified := s.verifyIdentify(linkID, idHash, sig)

	key := hex.EncodeToString(linkID)
	s.mu.Lock()
	s.identities[key] = append([]byte(nil), idHash...)
	s.mu.Unlock()

	state := "verified"
	if !verified {
		state = "UNVERIFIED (no cached announce for this identity)"
	}
	s.log.Printf("link %x identified as %s — %s", linkID[:4], hex.EncodeToString(idHash), state)
}

// verifyIdentify checks the LINKIDENTIFY signature — sig covers
// link_id(16) || identity.public_key(64) — against a public key
// recovered from the announce table. Returns false when no announce for
// the claimed identity has been seen (the claim is still recorded, but
// flagged).
func (s *Service) verifyIdentify(linkID, idHash, sig []byte) bool {
	for _, k := range s.transport.KnownSnapshot() {
		if len(k.PublicKey) != 64 {
			continue
		}
		h := sha256.Sum256(k.PublicKey)
		if !equalBytes(h[:16], idHash) {
			continue
		}
		signed := make([]byte, 0, len(linkID)+64)
		signed = append(signed, linkID...)
		signed = append(signed, k.PublicKey...)
		return ed25519.Verify(ed25519.PublicKey(k.PublicKey[32:]), signed, sig)
	}
	return false
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
