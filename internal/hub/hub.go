// Package hub implements the RRC hub: the server side of the Reticulum
// Relay Chat protocol. It is deliberately transport-agnostic — it speaks
// to each client through a [Link] and is driven by inbound frames fed to
// [Session.OnInbound]. The service layer wires those to real RNS links.
//
// Mirrors the room / session / router logic of the reference hub rrcd
// (github.com/kc1awv/rrcd).
package hub

import (
	"context"
	"encoding/hex"
	"log"
	"sync"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/config"
	"github.com/thatSFguy/reticulum-relay-chat/internal/roomreg"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// Link is the transport for one connected client. One Link is one
// established, identified Reticulum Link.
type Link interface {
	// Send delivers one encoded RRC envelope to the client as link DATA.
	Send(frame []byte) error
	// Close tears the underlying RNS Link down.
	Close()
	// PeerIdentityHash returns the client's 16-byte verified RNS identity
	// hash, or nil if the client has not identified yet.
	PeerIdentityHash() []byte
	// SendResource delivers a large payload to the client as an RNS
	// Resource (the hub has already sent the matching RESOURCE_ENVELOPE).
	// Returns an error when resource transfer is unavailable — the hub
	// then falls back to chunked NOTICEs.
	SendResource(payload []byte) error
}

// resourceExpectFloor is the resource-expectation reaper interval.
const resourceExpectFloor = 30 * time.Second

// Hub holds the live room registry and connected sessions. All exported
// methods are safe for concurrent use.
type Hub struct {
	identityHash []byte // hub's own 16-byte RNS identity hash
	cfg          config.HubConfig
	limits       rrc.Limits
	now          func() int64 // wall-clock milliseconds
	log          *log.Logger
	startedAt    int64

	mu       sync.Mutex
	rooms    map[string]*Room
	sessions map[*Session]struct{}

	trusted map[string]struct{} // server-op identity hashes (hex)
	banned  map[string]struct{} // config-banned ∪ kline hashes (hex)
	klines  map[string]struct{} // kline-only hashes (hex), for persistence

	started bool
	stats   stats
}

// New builds a Hub. identityHash is the hub's 16-byte RNS identity hash —
// the K_SRC value the hub stamps on its own messages.
func New(identityHash []byte, cfg config.HubConfig, logger *log.Logger) *Hub {
	if logger == nil {
		logger = log.Default()
	}
	lim := rrc.Limits{
		MaxNickBytes:        cfg.Limits.MaxNickBytes,
		MaxRoomNameBytes:    cfg.Limits.MaxRoomNameBytes,
		MaxMsgBodyBytes:     cfg.Limits.MaxMsgBodyBytes,
		MaxRoomsPerSession:  cfg.Limits.MaxRoomsPerSession,
		RateLimitMsgsPerMin: cfg.Limits.RateLimitMsgsPerMin,
	}
	if lim == (rrc.Limits{}) {
		lim = rrc.DefaultLimits()
	}
	h := &Hub{
		identityHash: identityHash,
		cfg:          cfg,
		limits:       lim,
		now:          func() int64 { return time.Now().UnixMilli() },
		log:          logger,
		startedAt:    time.Now().UnixMilli(),
		rooms:        make(map[string]*Room),
		sessions:     make(map[*Session]struct{}),
		trusted:      make(map[string]struct{}),
		banned:       make(map[string]struct{}),
		klines:       make(map[string]struct{}),
	}
	h.reloadTrust()
	h.loadKlines()
	h.loadRegistry()
	return h
}

// reloadTrust rebuilds the trusted/banned sets from config. The kline set
// is layered on top of the config-banned set. Caller need not hold h.mu;
// only called from New and /reload (which holds it). Safe under lock.
func (h *Hub) reloadTrust() {
	h.trusted = make(map[string]struct{})
	for _, t := range h.cfg.TrustedIdentities {
		if n := normHex(t); n != "" {
			h.trusted[n] = struct{}{}
		}
	}
	h.banned = make(map[string]struct{})
	for _, b := range h.cfg.BannedIdentities {
		if n := normHex(b); n != "" {
			h.banned[n] = struct{}{}
		}
	}
	for k := range h.klines {
		h.banned[k] = struct{}{}
	}
}

// loadKlines reads the kline file (if configured) into the kline + banned
// sets.
func (h *Hub) loadKlines() {
	if h.cfg.KlinePath == "" {
		return
	}
	hashes, err := roomreg.LoadKlines(h.cfg.KlinePath)
	if err != nil {
		h.log.Printf("hub: load klines: %v", err)
		return
	}
	for _, k := range hashes {
		if n := normHex(k); n != "" {
			h.klines[n] = struct{}{}
			h.banned[n] = struct{}{}
		}
	}
}

// loadRegistry materializes registered rooms from rooms.toml.
func (h *Hub) loadRegistry() {
	if h.cfg.RoomRegistryPath == "" {
		return
	}
	recs, err := roomreg.LoadRegistry(h.cfg.RoomRegistryPath, h.nowUnix())
	if err != nil {
		h.log.Printf("hub: load registry: %v", err)
		return
	}
	for name, rec := range recs {
		h.rooms[name] = roomFromRecord(name, rec)
	}
}

// nowUnix returns the current wall clock in unix seconds.
func (h *Hub) nowUnix() float64 { return float64(h.now()) / 1000.0 }

// SessionCount returns the number of connected sessions.
func (h *Hub) SessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// RoomCount returns the number of live rooms.
func (h *Hub) RoomCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}

// isServerOp reports whether the identity hash is a configured trusted
// (server-operator) identity. Caller must hold h.mu.
func (h *Hub) isServerOp(id []byte) bool {
	if len(id) == 0 {
		return false
	}
	_, ok := h.trusted[hex.EncodeToString(id)]
	return ok
}

// isBanned reports whether the identity hash is in the server ban set.
// Caller must hold h.mu.
func (h *Hub) isBanned(id []byte) bool {
	if len(id) == 0 {
		return false
	}
	_, ok := h.banned[hex.EncodeToString(id)]
	return ok
}

// roomLocked returns an existing room or nil. Caller must hold h.mu.
func (h *Hub) roomLocked(name string) *Room { return h.rooms[name] }

// createRoomLocked auto-creates a room with founder. Caller must hold
// h.mu.
func (h *Hub) createRoomLocked(name string, founder []byte) *Room {
	r := newRoom(name)
	r.founder = hex.EncodeToString(founder)
	if r.founder != "" {
		r.ops[r.founder] = struct{}{}
	}
	r.lastUsedTS = h.nowUnix()
	h.rooms[name] = r
	h.log.Printf("room created: #%s", name)
	return r
}

// dropRoomIfEmptyLocked removes an UNregistered room once its last member
// parts. Registered rooms persist. Caller must hold h.mu.
func (h *Hub) dropRoomIfEmptyLocked(r *Room) {
	if len(r.members) == 0 && !r.registered {
		delete(h.rooms, r.Name)
		h.log.Printf("room dropped (empty): #%s", r.Name)
	}
}

// touchRoom updates a room's lastUsedTS. Caller must hold h.mu.
func (h *Hub) touchRoom(r *Room) { r.lastUsedTS = h.nowUnix() }

// welcomedSessionsLocked returns all welcomed sessions. Caller holds h.mu.
func (h *Hub) welcomedSessionsLocked() []*Session {
	out := make([]*Session, 0, len(h.sessions))
	for s := range h.sessions {
		s.mu.Lock()
		w := s.welcomed
		s.mu.Unlock()
		if w {
			out = append(out, s)
		}
	}
	return out
}

// sessionForHashLocked returns the first connected session whose verified
// identity hash exactly equals id. Caller must hold h.mu.
func (h *Hub) sessionForHashLocked(idHex string) *Session {
	for s := range h.sessions {
		if id := s.identity(); id != nil && hex.EncodeToString(id) == idHex {
			return s
		}
	}
	return nil
}

// persistRegistryLocked saves the registered rooms to rooms.toml. Caller
// must hold h.mu. A no-op when no registry path is configured.
func (h *Hub) persistRegistryLocked() {
	if h.cfg.RoomRegistryPath == "" {
		return
	}
	recs := make(map[string]*roomreg.RoomRecord)
	for name, r := range h.rooms {
		if r.registered {
			recs[name] = r.toRecord()
		}
	}
	if err := roomreg.SaveRegistry(h.cfg.RoomRegistryPath, recs, h.nowUnix()); err != nil {
		h.log.Printf("hub: save registry: %v", err)
	}
}

// persistKlinesLocked saves the kline list. Caller must hold h.mu.
func (h *Hub) persistKlinesLocked() {
	if h.cfg.KlinePath == "" {
		return
	}
	hashes := make([]string, 0, len(h.klines))
	for k := range h.klines {
		hashes = append(hashes, k)
	}
	if err := roomreg.SaveKlines(h.cfg.KlinePath, hashes); err != nil {
		h.log.Printf("hub: save klines: %v", err)
	}
}

// Start launches the background loops (hub PING, registry prune, resource
// expectation reaper). Idempotent.
func (h *Hub) Start(ctx context.Context) {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	h.mu.Unlock()

	if h.cfg.PingInterval.Duration > 0 {
		go h.pingLoop(ctx)
	}
	if h.cfg.RoomRegistryPruneInterval.Duration > 0 && h.cfg.RoomRegistryPruneAfter.Duration > 0 {
		go h.pruneLoop(ctx)
	}
	go h.reaperLoop(ctx)
}

// Stop persists the room registry and klines. Call once on shutdown.
func (h *Hub) Stop() {
	h.mu.Lock()
	h.persistRegistryLocked()
	h.persistKlinesLocked()
	h.mu.Unlock()
}

// pingLoop sends hub keepalive PINGs and tears down links with a
// timed-out PONG.
func (h *Hub) pingLoop(ctx context.Context) {
	t := time.NewTicker(h.cfg.PingInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.doPingRound()
		}
	}
}

func (h *Hub) doPingRound() {
	timeout := h.cfg.PingTimeout.Duration
	now := h.now()
	h.mu.Lock()
	sessions := h.welcomedSessionsLocked()
	h.mu.Unlock()

	var toPing []*Session
	var toClose []*Session
	for _, s := range sessions {
		s.mu.Lock()
		awaiting := s.pingAwaitMs
		s.mu.Unlock()
		if awaiting != 0 {
			if timeout > 0 && now-awaiting > timeout.Milliseconds() {
				toClose = append(toClose, s)
			}
			continue
		}
		toPing = append(toPing, s)
	}
	for _, s := range toPing {
		s.mu.Lock()
		s.pingAwaitMs = now
		s.mu.Unlock()
		h.statInc(&h.stats.pingsOut)
		body := []byte(time.UnixMilli(now).UTC().Format(time.RFC3339))
		s.send(rrc.Ping(h.identityHash, h.now(), body))
	}
	for _, s := range toClose {
		h.log.Printf("hub: ping timeout, closing %s", shortHash(s.identity()))
		s.Close()
	}
}

// pruneLoop removes stale registered rooms with no connected members.
func (h *Hub) pruneLoop(ctx context.Context) {
	t := time.NewTicker(h.cfg.RoomRegistryPruneInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.doPrune()
		}
	}
}

func (h *Hub) doPrune() {
	cutoff := h.nowUnix() - h.cfg.RoomRegistryPruneAfter.Duration.Seconds()
	h.mu.Lock()
	var pruned []string
	for name, r := range h.rooms {
		if r.registered && len(r.members) == 0 && r.lastUsedTS < cutoff {
			delete(h.rooms, name)
			pruned = append(pruned, name)
		}
	}
	if len(pruned) > 0 {
		h.persistRegistryLocked()
	}
	h.mu.Unlock()
	for _, name := range pruned {
		h.log.Printf("hub: pruned stale registered room #%s", name)
	}
}

// reaperLoop drops expired pending resource expectations.
func (h *Hub) reaperLoop(ctx context.Context) {
	t := time.NewTicker(resourceExpectFloor)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.reapExpectations()
		}
	}
}

func (h *Hub) reapExpectations() {
	now := h.now()
	h.mu.Lock()
	sessions := make([]*Session, 0, len(h.sessions))
	for s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()
	for _, s := range sessions {
		s.reapExpectations(now)
	}
}

// fanout encodes env once and delivers it to every recipient link.
func (h *Hub) fanout(recipients []Link, env *rrc.Envelope) {
	if len(recipients) == 0 {
		return
	}
	frame, err := env.Encode()
	if err != nil {
		h.log.Printf("rrc: fan-out encode failed (type %d): %v", env.Type, err)
		return
	}
	for _, l := range recipients {
		if e := l.Send(frame); e != nil {
			h.log.Printf("rrc: fan-out send failed: %v", e)
		}
		h.statAdd(&h.stats.bytesOut, int64(len(frame)))
	}
}

// modeBroadcast sends a NOTICE to every member of a room.
func (h *Hub) modeBroadcast(r *Room, text string) {
	h.mu.Lock()
	recipients := roomLinksLocked(r)
	h.mu.Unlock()
	room := r.Name
	h.fanout(recipients, rrc.Notice(h.identityHash, h.now(), &room, text))
}

// inviteTTL returns the configured invite TTL, with a 15m fallback.
func (h *Hub) inviteTTL() time.Duration {
	d := h.cfg.RoomInviteTimeout.Duration
	if d <= 0 {
		return 15 * time.Minute
	}
	return d
}
