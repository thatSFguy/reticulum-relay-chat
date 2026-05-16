package hub

import (
	"sync"

	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// Session is one connected RRC client — the protocol state machine that
// rides a single established, identified Reticulum Link. Inbound link
// DATA frames are fed to OnInbound; the session never blocks the caller
// on network I/O while holding the hub lock.
type Session struct {
	hub  *Hub
	link Link

	mu       sync.Mutex
	welcomed bool
	closed   bool
	nick     string
	joined   map[string]*Room
	msgTimes []int64 // recent MSG wall-clock ms, for rate limiting
}

// NewSession registers a fresh session for an established link. The
// session is not usable until the client sends HELLO.
func (h *Hub) NewSession(link Link) *Session {
	s := &Session{hub: h, link: link, joined: make(map[string]*Room)}
	h.mu.Lock()
	h.sessions[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (s *Session) identity() []byte { return s.link.PeerIdentityHash() }

// OnInbound feeds one decrypted inbound link-DATA frame (a CBOR
// envelope). Decode failures and unhandled types are logged and
// dropped — a misbehaving client must never crash the hub.
func (s *Session) OnInbound(frame []byte) {
	env, err := rrc.Decode(frame)
	if err != nil {
		s.hub.log.Printf("rrc: dropped malformed inbound frame: %v", err)
		return
	}
	switch env.Type {
	case rrc.THello:
		s.handleHello(env)
	case rrc.TJoin:
		s.handleJoin(env)
	case rrc.TPart:
		s.handlePart(env)
	case rrc.TMsg:
		s.handleMsg(env)
	case rrc.TPing:
		s.handlePing(env)
	case rrc.TPong:
		// Client answered a hub keepalive — liveness only, nothing to do.
	default:
		s.hub.log.Printf("rrc: unhandled inbound message type %d", env.Type)
	}
}

// --- outbound helpers -------------------------------------------------

// send encodes and delivers one envelope to this session's client.
func (s *Session) send(env *rrc.Envelope) {
	frame, err := env.Encode()
	if err != nil {
		s.hub.log.Printf("rrc: encode failed (type %d): %v", env.Type, err)
		return
	}
	s.sendFrame(frame)
}

// sendFrame delivers an already-encoded frame.
func (s *Session) sendFrame(frame []byte) {
	if err := s.link.Send(frame); err != nil {
		s.hub.log.Printf("rrc: link send failed: %v", err)
	}
}

func (s *Session) sendError(room *string, text string) {
	s.send(rrc.Error(s.hub.identityHash, s.hub.now(), room, text))
}

// --- HELLO ------------------------------------------------------------

func (s *Session) handleHello(env *rrc.Envelope) {
	h := s.hub
	nick := clampNick(rrc.NickName(env), h.cfg.Limits.MaxNickBytes)

	s.mu.Lock()
	s.welcomed = true
	s.nick = nick
	s.mu.Unlock()

	h.log.Printf("session welcomed: %s (nick=%q)", shortHash(s.identity()), nick)
	s.send(rrc.Welcome(h.identityHash, h.now(), h.cfg.Name, h.cfg.Version,
		h.cfg.Limits, true))

	// Greeting (message-of-the-day) is delivered after WELCOME as one or
	// more hub-wide NOTICE messages, chunked to stay within the link MTU.
	for _, chunk := range chunkGreeting(h.cfg.Greeting) {
		s.send(rrc.Notice(h.identityHash, h.now(), nil, chunk))
	}
}

// --- JOIN -------------------------------------------------------------

func (s *Session) handleJoin(env *rrc.Envelope) {
	h := s.hub
	if !s.isWelcomed() {
		s.sendError(nil, "send HELLO before joining a room")
		return
	}
	id := s.identity()
	if id == nil {
		s.sendError(nil, "link is not identified — RRC requires LINKIDENTIFY")
		return
	}
	room := rrc.RoomName(env)
	if room == "" {
		s.sendError(nil, "JOIN requires a room name")
		return
	}
	if len(room) > h.cfg.Limits.MaxRoomNameBytes {
		s.sendError(&room, "room name exceeds the hub limit")
		return
	}
	key := rrc.BodyText(env)

	s.mu.Lock()
	if _, already := s.joined[room]; already {
		s.mu.Unlock()
		return // idempotent — silently ignore a re-JOIN
	}
	roomCount := len(s.joined)
	s.mu.Unlock()
	if roomCount >= h.cfg.Limits.MaxRoomsPerSession {
		s.sendError(&room, "joined-room limit reached for this session")
		return
	}

	h.mu.Lock()
	r := h.roomLocked(room)
	if r.Key != "" && r.Key != key {
		h.mu.Unlock()
		s.sendError(&room, "incorrect or missing room key")
		return
	}
	r.members[s] = struct{}{}
	r.lastActivity = h.now()
	members := r.memberHashesLocked()
	recipients := roomLinksLocked(r)
	h.mu.Unlock()

	s.mu.Lock()
	s.joined[room] = r
	s.mu.Unlock()

	h.log.Printf("%s joined #%s (%d members)", shortHash(id), room, len(members))
	h.fanout(recipients, rrc.Joined(h.identityHash, h.now(), room, members))
}

// --- PART -------------------------------------------------------------

func (s *Session) handlePart(env *rrc.Envelope) {
	h := s.hub
	room := rrc.RoomName(env)
	if room == "" {
		s.sendError(nil, "PART requires a room name")
		return
	}

	s.mu.Lock()
	r := s.joined[room]
	delete(s.joined, room)
	s.mu.Unlock()
	if r == nil {
		return // not in that room — nothing to do
	}

	h.mu.Lock()
	delete(r.members, s)
	r.lastActivity = h.now()
	members := r.memberHashesLocked()
	recipients := roomLinksLocked(r)
	h.dropRoomIfEmptyLocked(r)
	h.mu.Unlock()

	h.log.Printf("%s parted #%s", shortHash(s.identity()), room)
	parted := rrc.Parted(h.identityHash, h.now(), room, members)
	h.fanout(recipients, parted)
	s.send(parted) // confirm to the parter, who is no longer in recipients
}

// --- MSG --------------------------------------------------------------

func (s *Session) handleMsg(env *rrc.Envelope) {
	h := s.hub
	if !s.isWelcomed() {
		s.sendError(nil, "send HELLO before messaging")
		return
	}
	id := s.identity()
	if id == nil {
		s.sendError(nil, "link is not identified — RRC requires LINKIDENTIFY")
		return
	}
	room := rrc.RoomName(env)
	text := rrc.BodyText(env)
	if len(text) > h.cfg.Limits.MaxMsgBodyBytes {
		s.sendError(&room, "message exceeds the hub body-size limit")
		return
	}

	s.mu.Lock()
	r := s.joined[room]
	nick := s.nick
	overLimit := s.rateLimitedLocked()
	s.mu.Unlock()
	if r == nil {
		s.sendError(&room, "join the room before messaging it")
		return
	}
	if overLimit {
		s.sendError(&room, "rate limit exceeded — slow down")
		return
	}

	// Rewrite K_SRC to the link-verified identity and stamp K_NICK; the
	// client-supplied src/nick are advisory. K_ID / K_TS / K_ROOM /
	// K_BODY pass through so clients can dedup the hub echo by msg id.
	env.Src = id
	if nick != "" {
		env.Nick = &nick
	} else {
		env.Nick = nil
	}
	frame, err := env.Encode()
	if err != nil {
		h.log.Printf("rrc: re-encode of relayed MSG failed: %v", err)
		return
	}

	h.mu.Lock()
	r.lastActivity = h.now()
	recipients := roomLinksLocked(r)
	h.mu.Unlock()

	for _, l := range recipients {
		if e := l.Send(frame); e != nil {
			h.log.Printf("rrc: fan-out send failed: %v", e)
		}
	}
}

// --- PING -------------------------------------------------------------

func (s *Session) handlePing(env *rrc.Envelope) {
	s.send(rrc.Pong(s.hub.identityHash, s.hub.now(), rrc.BodyBytes(env)))
}

// --- lifecycle --------------------------------------------------------

// Close tears the session down: it leaves every joined room (announcing
// a PARTED to the remaining members), de-registers from the hub, and
// closes the underlying link. Idempotent.
func (s *Session) Close() {
	h := s.hub
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	rooms := make([]*Room, 0, len(s.joined))
	for _, r := range s.joined {
		rooms = append(rooms, r)
	}
	s.joined = make(map[string]*Room)
	s.mu.Unlock()

	type partedRoom struct {
		recipients []Link
		env        *rrc.Envelope
	}
	var announce []partedRoom

	h.mu.Lock()
	delete(h.sessions, s)
	for _, r := range rooms {
		delete(r.members, s)
		r.lastActivity = h.now()
		announce = append(announce, partedRoom{
			recipients: roomLinksLocked(r),
			env:        rrc.Parted(h.identityHash, h.now(), r.Name, r.memberHashesLocked()),
		})
		h.dropRoomIfEmptyLocked(r)
	}
	h.mu.Unlock()

	for _, pr := range announce {
		h.fanout(pr.recipients, pr.env)
	}
	h.log.Printf("session closed: %s", shortHash(s.identity()))
	s.link.Close()
}

func (s *Session) isWelcomed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.welcomed
}

// rateLimitedLocked prunes the recent-send window and reports whether
// another MSG would exceed the hub's per-minute rate limit. On a false
// return it records the send. Caller must hold s.mu.
func (s *Session) rateLimitedLocked() bool {
	limit := s.hub.cfg.Limits.RateLimitMsgsPerMin
	if limit <= 0 {
		return false
	}
	now := s.hub.now()
	cutoff := now - 60_000
	kept := s.msgTimes[:0]
	for _, t := range s.msgTimes {
		if t >= cutoff {
			kept = append(kept, t)
		}
	}
	s.msgTimes = kept
	if len(s.msgTimes) >= limit {
		return true
	}
	s.msgTimes = append(s.msgTimes, now)
	return false
}
