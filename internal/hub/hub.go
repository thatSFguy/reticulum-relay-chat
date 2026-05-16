// Package hub implements the RRC hub: the server side of the Reticulum
// Relay Chat protocol. It is deliberately transport-agnostic — it speaks
// to each client through a [Link] and is driven by inbound frames fed to
// [Session.OnInbound]. The service layer wires those to real RNS links.
//
// Mirrors the room / session / router logic of the reference hub rrcd
// (github.com/kc1awv/rrcd).
package hub

import (
	"encoding/hex"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// Link is the transport an RRC [Session] sends over. One Link is one
// established, identified Reticulum Link to a connected client.
type Link interface {
	// Send delivers one encoded RRC envelope to the client as
	// encrypted CTX_NONE link DATA.
	Send(frame []byte) error
	// Close tears the underlying RNS Link down.
	Close()
	// PeerIdentityHash returns the client's 16-byte verified RNS
	// identity hash (from the §6.6 LINKIDENTIFY), or nil if the client
	// has not identified yet. RRC requires identification — the hub
	// rewrites every envelope K_SRC from this value.
	PeerIdentityHash() []byte
}

// Config is the hub's static configuration.
type Config struct {
	Name     string     // hub display name, sent in WELCOME
	Version  string     // hub software version string, sent in WELCOME
	Greeting string     // optional message-of-the-day, sent as NOTICEs after WELCOME
	Limits   rrc.Limits // advertised client limits
}

// Hub holds the live room registry and connected sessions. All exported
// methods are safe for concurrent use.
type Hub struct {
	identityHash []byte // hub's own 16-byte RNS identity hash
	cfg          Config
	now          func() int64 // wall-clock milliseconds
	log          *log.Logger

	mu       sync.Mutex
	rooms    map[string]*Room
	sessions map[*Session]struct{}
}

// New builds a Hub. identityHash is the hub's 16-byte RNS identity hash —
// the K_SRC value the hub stamps on its own messages.
func New(identityHash []byte, cfg Config, logger *log.Logger) *Hub {
	if cfg.Limits == (rrc.Limits{}) {
		cfg.Limits = rrc.DefaultLimits()
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Hub{
		identityHash: identityHash,
		cfg:          cfg,
		now:          func() int64 { return time.Now().UnixMilli() },
		log:          logger,
		rooms:        make(map[string]*Room),
		sessions:     make(map[*Session]struct{}),
	}
}

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

// roomDirectory renders the hub's current rooms as a human-readable
// line — "Active rooms: #lobby, #checkers" — or "" when the hub has no
// rooms. RRC has no room-list protocol message, so the hub advertises
// its rooms to each new client in a NOTICE after WELCOME; that is the
// only way a client can discover a room name without being told it
// out-of-band.
func (h *Hub) roomDirectory() string {
	h.mu.Lock()
	names := make([]string, 0, len(h.rooms))
	for name := range h.rooms {
		names = append(names, name)
	}
	h.mu.Unlock()
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	for i, n := range names {
		names[i] = "#" + n
	}
	return "Active rooms: " + strings.Join(names, ", ")
}

// roomLocked returns the room, creating it on first reference. Caller
// must hold h.mu.
func (h *Hub) roomLocked(name string) *Room {
	r := h.rooms[name]
	if r == nil {
		r = &Room{
			Name:      name,
			members:   make(map[*Session]struct{}),
			createdAt: h.now(),
		}
		h.rooms[name] = r
		h.log.Printf("room created: #%s", name)
	}
	return r
}

// dropRoomIfEmptyLocked removes a room once its last member parts.
// Caller must hold h.mu.
func (h *Hub) dropRoomIfEmptyLocked(r *Room) {
	if len(r.members) == 0 {
		delete(h.rooms, r.Name)
		h.log.Printf("room dropped (empty): #%s", r.Name)
	}
}

// Room is one chat room. Membership and activity are guarded by the
// owning Hub's mutex.
type Room struct {
	Name         string
	Key          string // "" = open; non-empty = keyed (+k) room
	members      map[*Session]struct{}
	createdAt    int64
	lastActivity int64
}

// memberHashesLocked returns the room's member identity hashes, sorted
// for a deterministic JOINED/PARTED member list. Caller must hold h.mu.
func (r *Room) memberHashesLocked() [][]byte {
	out := make([][]byte, 0, len(r.members))
	for s := range r.members {
		if id := s.identity(); id != nil {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return hex.EncodeToString(out[i]) < hex.EncodeToString(out[j])
	})
	return out
}
