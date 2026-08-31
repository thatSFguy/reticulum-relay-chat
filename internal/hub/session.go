package hub

import (
	"encoding/hex"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/thatSFguy/reticulum-relay-chat/internal/history"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// Session is one connected RRC client — the protocol state machine that
// rides a single established, identified Reticulum Link.
type Session struct {
	hub  *Hub
	link Link

	createdMs int64 // session creation time (ms), for the idle-unwelcomed sweep

	mu       sync.Mutex
	welcomed bool
	closed   bool
	nick     string
	joined   map[string]*Room // room name -> Room
	bucket   *tokenBucket

	pingAwaitMs int64 // 0 = not awaiting a PONG

	// lastAliveMs is the hub's clock when this peer was last heard
	// from — any inbound frame at all, since every one of them had to
	// cross a live link to get here.
	//
	// Room membership is NOT evidence of presence. A client that closes
	// its app leaves the link to rot, and the hub goes on listing it as
	// a member until a PING goes unanswered for ping_timeout — up to
	// ping_interval + ping_timeout of phantom presence. Anything that
	// treats "in the room" as "received the message" is wrong for that
	// whole window. See mentionLivenessProven.
	lastAliveMs int64

	lastHistoryPullMs int64 // 0 = never; throttles /history (see cmdHistory)

	// away is set by /away. It is a statement the peer makes about
	// themselves, and the hub takes it at face value: a mention is then
	// queued and pushed rather than written into the room the peer is
	// sitting in but not reading.
	//
	// This exists because the hub's own presence test can only measure
	// whether frames are arriving, and "my client is connected in a
	// background tab" looks exactly like "I am here". Nothing else can
	// tell the hub the difference.
	away        bool
	awayReason  string
	awaySinceMs int64

	expectations []*resourceExpectation
}

// NewSession registers a fresh session for an established link. The
// session is not usable until the client sends HELLO.
func (h *Hub) NewSession(link Link) *Session {
	s := &Session{
		hub:       h,
		link:      link,
		joined:    make(map[string]*Room),
		bucket:    newTokenBucket(h.limits.RateLimitMsgsPerMin, h.now()),
		createdMs: h.now(),
	}
	h.mu.Lock()
	// Cap concurrent sessions (audit A3): a peer must not be able to
	// exhaust hub memory by opening unbounded links.
	full := h.cfg.MaxSessions > 0 && len(h.sessions) >= h.cfg.MaxSessions
	if !full {
		h.sessions[s] = struct{}{}
	}
	h.mu.Unlock()
	if full {
		h.log.Printf("hub: session limit (%d) reached — refusing new link", h.cfg.MaxSessions)
		s.sendError(nil, "server full")
		s.Close()
		return s
	}

	// A peer already in the ban set is refused as soon as it is known.
	if id := link.PeerIdentityHash(); id != nil {
		h.mu.Lock()
		banned := h.isBanned(id)
		h.mu.Unlock()
		if banned {
			s.sendError(nil, "banned")
			s.Close()
		}
	}
	return s
}

func (s *Session) identity() []byte { return s.link.PeerIdentityHash() }

// isClosed reports whether this session has been torn down. Used by
// work that finishes after a delay, so it can tell "the answer was
// delivered" from "the answer was written into a dead link".
func (s *Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// isAway reports the peer's own /away state.
func (s *Session) isAway() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.away
}

// PeerPublicKey returns the 64-byte public key this session's client
// proved when it identified, or nil if it has not identified yet.
func (s *Session) PeerPublicKey() []byte { return s.link.PeerPublicKey() }

func (s *Session) identityHex() string {
	if id := s.identity(); id != nil {
		return hex.EncodeToString(id)
	}
	return ""
}

// OnInbound feeds one decoded inbound link-DATA frame.
func (s *Session) OnInbound(frame []byte) {
	h := s.hub
	// A closed session (e.g. refused at the MaxSessions cap, or torn
	// down) must not process further frames even if a stale reference
	// to it still routes inbound DATA here.
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	h.statInc(&h.stats.pktsIn)
	h.statAdd(&h.stats.bytesIn, int64(len(frame)))

	// Heard from. Recorded before the rate limiter and the decoder: even
	// a malformed or throttled frame is proof the peer is still on the
	// other end, which is all this timestamp claims.
	s.mu.Lock()
	s.lastAliveMs = h.now()
	s.mu.Unlock()

	// Rate limit BEFORE decode (audit A19): CBOR decoding is itself
	// attacker-driven work, so a flood of malformed frames must be
	// throttled by the token bucket too — not just well-formed traffic.
	s.mu.Lock()
	allowed := s.bucket.allow(h.now())
	s.mu.Unlock()
	if !allowed {
		h.statInc(&h.stats.rateLimited)
		s.sendError(nil, "rate limited")
		return
	}

	env, err := rrc.Decode(frame)
	if err != nil {
		h.statInc(&h.stats.pktsBad)
		h.log.Printf("rrc: dropped malformed inbound frame: %v", err)
		return
	}

	// A verified RNS identity (bound via §6.7.6 LINKIDENTIFY) is a hard
	// precondition for processing any RRC frame: an un-identified peer is
	// never welcomed and never authorized (A1). Rechecking it on every
	// frame also closes ban evasion (A2) — a banned identity that sent
	// HELLO before LINKIDENTIFY is caught the moment its identity binds.
	id := s.identity()
	if id == nil {
		s.sendError(nil, "identify (LINKIDENTIFY) required before any RRC frame")
		return
	}
	h.mu.Lock()
	hubClosed := h.closed
	banned := h.isBanned(id)
	h.mu.Unlock()
	if hubClosed {
		return // hub is shutting down — drop late frames (audit A9)
	}
	if banned {
		s.sendError(nil, "banned")
		s.Close()
		return
	}

	// PONG answers a hub keepalive and is accepted even before WELCOME.
	if env.Type == rrc.TPong {
		s.handlePong(env)
		return
	}

	// WELCOME gate — every frame except HELLO requires a welcomed session.
	// RESOURCE_ENVELOPE is included (A11): an un-welcomed peer must not be
	// able to pin receive-buffer memory with resource expectations.
	if env.Type != rrc.THello && !s.isWelcomed() {
		s.sendError(nil, "send HELLO first")
		return
	}

	switch env.Type {
	case rrc.THello:
		s.handleHello(env)
	case rrc.TResourceEnvelope:
		s.handleResourceEnvelope(env)
	case rrc.TJoin:
		s.handleJoin(env)
	case rrc.TPart:
		s.handlePart(env)
	case rrc.TMsg:
		s.handleMsg(env, rrc.TMsg)
	case rrc.TNotice:
		s.handleMsg(env, rrc.TNotice)
	case rrc.TAction:
		s.handleMsg(env, rrc.TAction)
	case rrc.TPing:
		s.handlePing(env)
	default:
		h.log.Printf("rrc: unhandled inbound message type %d", env.Type)
	}
}

// --- outbound helpers -------------------------------------------------

func (s *Session) send(env *rrc.Envelope) {
	frame, err := env.Encode()
	if err != nil {
		s.hub.log.Printf("rrc: encode failed (type %d): %v", env.Type, err)
		return
	}
	s.sendFrame(frame)
}

func (s *Session) sendFrame(frame []byte) {
	if err := s.link.Send(frame); err != nil {
		s.hub.log.Printf("rrc: link send failed: %v", err)
		return
	}
	s.hub.statAdd(&s.hub.stats.bytesOut, int64(len(frame)))
}

func (s *Session) sendError(room *string, text string) {
	s.hub.statInc(&s.hub.stats.errorsSent)
	s.send(rrc.Error(s.hub.identityHash, s.hub.now(), room, text))
}

func (s *Session) sendNotice(room *string, text string) {
	s.send(rrc.Notice(s.hub.identityHash, s.hub.now(), room, text))
}

// --- HELLO ------------------------------------------------------------

func (s *Session) handleHello(env *rrc.Envelope) {
	h := s.hub

	// A re-HELLO after welcome resets the session.
	if s.isWelcomed() {
		s.resetForReHello()
	}

	nick := rrc.NickName(env)
	if nick == "" {
		nick = rrc.LegacyHelloNick(env)
	}
	// asked is the claim as typed, minus the validation normalizeNick
	// also applies. Kept because the granted name may differ for TWO
	// reasons now — somebody else owns it, or it had spaces in it — and
	// the peer is owed the right explanation.
	asked, _ := normalizeText(nick, h.limits.MaxNickBytes)
	if n, ok := normalizeNick(nick, h.limits.MaxNickBytes); ok {
		nick = n
	} else {
		nick = ""
	}

	s.mu.Lock()
	s.welcomed = true
	s.mu.Unlock()

	// The hub grants the nick rather than accepting it: see nicks.go.
	// Done before WELCOME so everything downstream — the peer
	// directory, fan-out, mention resolution — sees one name.
	h.mu.Lock()
	granted, renamed, lookalike := s.setNickLocked(nick)
	h.mu.Unlock()

	if renamed || (granted != "" && granted != asked) {
		h.log.Printf("session welcomed: %s (nick=%q, asked for %q)",
			shortHash(s.identity()), granted, asked)
	} else {
		h.log.Printf("session welcomed: %s (nick=%q)", shortHash(s.identity()), granted)
	}

	// WELCOME body: hub name, version, limits. rrcd does not populate caps.
	w := rrc.Welcome(h.identityHash, h.now(), h.cfg.Name, h.cfg.Version, h.limits, false)
	if body, ok := w.Body.(map[int]any); ok {
		delete(body, rrc.BWelcomeCaps)
	}
	s.send(w)

	// Greeting (MOTD) after WELCOME.
	s.sendGreeting()

	// Nothing in RRC can carry "you asked for sam and you are sam1" —
	// WELCOME has no field for it — so it is said in a NOTICE, which
	// every deployed client already renders.
	switch {
	case granted == "" && asked != "":
		// Valid text, but nothing an @mention could address survived.
		s.sendNotice(nil, nickUnusableNotice(asked))
	case granted != "" && asked != nick:
		// The name was rewritten to be mentionable. Said first even
		// when the rewritten form was also taken: a suffix is the
		// ordinary thing that happens here, this is the surprise.
		s.sendNotice(nil, nickUnmentionableNotice(asked, granted))
	case lookalike:
		s.sendNotice(nil, nickLookalikeNotice(nick, granted))
	case renamed:
		s.sendNotice(nil, nickTakenNotice(nick, granted))
	}

	// File this identity — its key is what makes it addressable later —
	// then hand over anything that arrived while it was away.
	s.rememberPeer()
	s.flushMentions()
}

// resetForReHello removes the peer from all rooms and clears its state.
func (s *Session) resetForReHello() {
	h := s.hub
	s.mu.Lock()
	rooms := make([]*Room, 0, len(s.joined))
	for _, r := range s.joined {
		rooms = append(rooms, r)
	}
	s.joined = make(map[string]*Room)
	s.nick = ""
	s.welcomed = false
	s.expectations = nil
	s.away = false
	s.awayReason = ""
	s.awaySinceMs = 0
	s.mu.Unlock()

	type partedRoom struct {
		recipients []Link
		env        *rrc.Envelope
	}
	var announce []partedRoom
	h.mu.Lock()
	for _, r := range rooms {
		delete(r.members, s)
		h.touchRoom(r)
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
}

// sendGreeting delivers the configured MOTD as NOTICE(s) (or a resource
// for large text).
func (s *Session) sendGreeting() {
	h := s.hub
	g := h.cfg.Greeting
	if g == "" {
		return
	}
	if len(g) > 512 {
		if s.tryResourceSend([]byte(g), rrc.ResKindMOTD, nil) {
			return
		}
	}
	for _, chunk := range chunkText(g) {
		s.sendNotice(nil, chunk)
	}
}

// --- JOIN -------------------------------------------------------------

func (s *Session) handleJoin(env *rrc.Envelope) {
	h := s.hub
	id := s.identity()
	room := roomFromEnvelope(env)
	if room == "" {
		s.sendError(nil, "JOIN requires room name")
		return
	}
	// A room name must be valid UTF-8 (audit A7): an invalid-UTF-8 name
	// would later be written to rooms.toml and make the file unloadable.
	if !utf8.ValidString(room) {
		s.sendError(nil, "room name must be valid UTF-8")
		return
	}
	if h.limits.MaxRoomNameBytes > 0 && len(room) > h.limits.MaxRoomNameBytes {
		s.sendError(&room, "room name exceeds the hub limit")
		return
	}
	key := rrc.BodyText(env)

	s.mu.Lock()
	if _, already := s.joined[room]; already {
		s.mu.Unlock()
		return // idempotent re-JOIN
	}
	roomCount := len(s.joined)
	s.mu.Unlock()
	if h.limits.MaxRoomsPerSession > 0 && roomCount >= h.limits.MaxRoomsPerSession {
		s.sendError(&room, "too many rooms")
		return
	}

	idHex := ""
	if id != nil {
		idHex = hex.EncodeToString(id)
	}

	h.mu.Lock()
	r := h.roomLocked(room)
	created := false
	if r == nil {
		// Cap the total room count (audit A3): a JOIN→/register→PART
		// loop must not be able to grow the room table without bound.
		if h.cfg.MaxRooms > 0 && len(h.rooms) >= h.cfg.MaxRooms {
			h.mu.Unlock()
			s.sendError(&room, "room limit reached")
			return
		}
		r = h.createRoomLocked(room, id)
		created = true
	}
	serverOp := h.isServerOp(id)
	r.pruneInvitesLocked(h.nowUnix())

	// Bans.
	if _, banned := r.bans[idHex]; banned && idHex != "" {
		h.mu.Unlock()
		s.sendError(&room, "banned from room")
		return
	}
	invited := r.hasValidInvite(idHex, h.nowUnix())
	isOp := r.isOp(idHex, serverOp)
	// +i invite-only.
	if r.inviteOnly && !isOp && !invited {
		h.mu.Unlock()
		s.sendError(&room, "invite-only (+i)")
		return
	}
	// +k keyed.
	if r.key != "" && !isOp && !invited && r.key != key {
		h.mu.Unlock()
		s.sendError(&room, "bad key (+k)")
		return
	}
	// Consume invite.
	if invited {
		delete(r.invited, idHex)
		if r.registered {
			h.markRegistryDirtyLocked()
		}
	}

	r.members[s] = struct{}{}
	h.touchRoom(r)
	var members [][]byte
	if h.cfg.IncludeJoinedMemberList {
		members = r.memberHashesLocked()
	}
	recipients := roomLinksLocked(r)
	registered := r.registered
	modeStr := r.modeString()
	topic := r.topic
	h.mu.Unlock()

	s.mu.Lock()
	s.joined[room] = r
	s.mu.Unlock()

	h.statInc(&h.stats.joins)
	h.log.Printf("%s joined #%s (created=%v)", shortHash(id), room, created)
	h.fanout(recipients, rrc.Joined(h.identityHash, h.now(), room, members))

	// Room-info NOTICE to the joiner.
	regWord := "unregistered"
	if registered {
		regWord = "registered"
	}
	topicWord := topic
	if topicWord == "" {
		topicWord = "(none)"
	}
	s.sendNotice(&room, "room "+room+": "+regWord+"; mode="+modeStr+"; topic="+topicWord)

	// A freshly created unregistered room starts empty: its name may
	// have been used before by a room that has since died, and RRC's
	// contract is that such a room took its conversation with it.
	if created && !registered {
		h.dropHistory(room)
		return
	}
	// Otherwise catch the joiner up. Deliberately a small taste of the
	// conversation rather than the whole window — the client may be on a
	// link where a week of backlog is minutes of airtime — and /history
	// is there for anyone who wants more.
	if h.cfg.HistoryReplayCount > 0 {
		s.replayTo(room, history.Query{
			Limit:    h.cfg.HistoryReplayCount,
			MaxBytes: h.cfg.HistoryReplayBytes,
		})
	}
}

// --- PART -------------------------------------------------------------

func (s *Session) handlePart(env *rrc.Envelope) {
	h := s.hub
	room := roomFromEnvelope(env)
	if room == "" {
		s.sendError(nil, "PART requires room name")
		return
	}

	s.mu.Lock()
	r := s.joined[room]
	delete(s.joined, room)
	s.mu.Unlock()
	if r == nil {
		return
	}

	h.mu.Lock()
	delete(r.members, s)
	h.touchRoom(r)
	var members [][]byte
	if h.cfg.IncludeJoinedMemberList {
		members = r.memberHashesLocked()
	}
	recipients := roomLinksLocked(r)
	h.dropRoomIfEmptyLocked(r)
	h.mu.Unlock()

	h.statInc(&h.stats.parts)
	h.log.Printf("%s parted #%s", shortHash(s.identity()), room)
	parted := rrc.Parted(h.identityHash, h.now(), room, members)
	h.fanout(recipients, parted)
	s.send(parted) // confirm to the parter
}

// --- MSG / NOTICE / ACTION --------------------------------------------

func (s *Session) handleMsg(env *rrc.Envelope, typ int) {
	h := s.hub
	id := s.identity()
	room := roomFromEnvelope(env)

	// Command dispatch — MSG and NOTICE only, never ACTION.
	if typ != rrc.TAction {
		if body, ok := env.Body.(string); ok {
			if cmd := dispatchBody(body); cmd != "" {
				s.handleCommand(cmd, body, room)
				return
			}
		}
	}

	if typ == rrc.TMsg && room == "" {
		s.sendError(nil, "message requires room name")
		return
	}
	if typ == rrc.TNotice && room == "" {
		return // NOTICE with no room is silently dropped
	}
	if typ == rrc.TAction && room == "" {
		s.sendError(nil, "message requires room name")
		return
	}

	// MSG body-size limit (not NOTICE).
	if typ == rrc.TMsg {
		if text, ok := env.Body.(string); ok && h.limits.MaxMsgBodyBytes > 0 &&
			len(text) > h.limits.MaxMsgBodyBytes {
			s.sendError(&room, "message exceeds the hub body-size limit")
			return
		}
	}

	idHex := ""
	if id != nil {
		idHex = hex.EncodeToString(id)
	}

	h.mu.Lock()
	r := h.roomLocked(room)
	if r == nil {
		h.mu.Unlock()
		s.sendError(&room, "no such room")
		return
	}
	serverOp := h.isServerOp(id)
	joined := r.hasMember(s)
	// Room bans.
	if _, banned := r.bans[idHex]; banned && idHex != "" {
		h.mu.Unlock()
		s.sendError(&room, "banned from room")
		return
	}
	// +n no outside messages.
	if r.noOutsideMsgs && !joined {
		h.mu.Unlock()
		s.sendError(&room, "no outside messages (+n)")
		return
	}
	// +m moderated.
	if r.moderated && !r.isVoiced(idHex, serverOp) {
		h.mu.Unlock()
		s.sendError(&room, "room is moderated (+m)")
		return
	}
	h.touchRoom(r)
	recipients := roomLinksLocked(r)
	h.mu.Unlock()

	// Bound the extension keys BEFORE fan-out (docs/rrc-extensions.md
	// §5). A hub is an amplifier — one inbound frame becomes one per
	// room member — so bytes it relays without reading are multiplied
	// by the room size. Rejected rather than truncated: silently
	// dropping half of somebody's reply reference produces a message
	// that looks fine and threads wrongly, which is worse than an
	// error the sender can see.
	if n, err := env.ExtEncodedLen(); err != nil || n > maxExtBytes {
		s.sendError(&room, fmt.Sprintf(
			"extension keys exceed the hub limit (%d bytes)", maxExtBytes))
		return
	}

	// Rewrite K_SRC to verified identity, stamp nick.
	//
	// A message may carry K_NICK, and the hub adopts it as the session
	// nick — which is the second way a name is claimed, and so the
	// second place uniqueness has to hold. Enforcing only at HELLO
	// would be theatre: a client would re-assert the taken name here.
	s.mu.Lock()
	nick := s.nick
	s.mu.Unlock()
	var renamedTo, lookalikeTo, spacedTo, spacedFrom string
	if envNick := rrc.NickName(env); envNick != "" {
		if want, ok := normalizeNick(envNick, h.limits.MaxNickBytes); ok && want != nick {
			h.mu.Lock()
			granted, renamed, lookalike := s.setNickLocked(want)
			h.mu.Unlock()
			// Notify on a change of the GRANTED name, not of the asked-for
			// one. A client that keeps asserting a taken "sam" is granted
			// "sam1" every time, and "sam" != "sam1" on every message —
			// testing the request would repeat the notice forever. The
			// same trap applies to a spaced name: "sam jones" arrives
			// on every MSG, and it is want == nick ("sam_jones")
			// that stops this branch running twice.
			asked, _ := normalizeText(envNick, h.limits.MaxNickBytes)
			switch {
			case asked != want && granted != nick:
				spacedTo = granted
				spacedFrom = asked
			case lookalike && granted != nick:
				lookalikeTo = granted
			case renamed && granted != nick:
				renamedTo = granted
			}
			nick = granted
		}
	}
	switch {
	case spacedTo != "":
		s.sendNotice(&room, nickUnmentionableNotice(spacedFrom, spacedTo))
	case lookalikeTo != "":
		s.sendNotice(&room, nickLookalikeNotice(rrc.NickName(env), lookalikeTo))
	case renamedTo != "":
		s.sendNotice(&room, nickTakenNotice(rrc.NickName(env), renamedTo))
	}

	env.Src = id
	if nick != "" {
		env.Nick = &nick
	} else {
		env.Nick = nil
	}
	frame, err := env.Encode()
	if err != nil {
		h.log.Printf("rrc: re-encode of relayed message failed: %v", err)
		return
	}
	for _, l := range recipients {
		if e := l.Send(frame); e != nil {
			h.log.Printf("rrc: fan-out send failed: %v", e)
		}
		h.statAdd(&h.stats.bytesOut, int64(len(frame)))
	}
	switch typ {
	case rrc.TMsg:
		h.statInc(&h.stats.msgsFwd)
	case rrc.TNotice:
		h.statInc(&h.stats.noticesFwd)
	case rrc.TAction:
		h.statInc(&h.stats.actionsFwd)
	}
	// Retain what the room actually saw — env carries the rewritten
	// K_SRC and the normalized nick from just above.
	h.recordMessage(room, env)
	h.noteMentions(room, env)
}

// --- PING / PONG ------------------------------------------------------

func (s *Session) handlePing(env *rrc.Envelope) {
	s.hub.statInc(&s.hub.stats.pingsIn)
	s.hub.statInc(&s.hub.stats.pongsOut)
	s.send(rrc.Pong(s.hub.identityHash, s.hub.now(), rrc.BodyBytes(env)))
}

func (s *Session) handlePong(env *rrc.Envelope) {
	s.hub.statInc(&s.hub.stats.pongsIn)
	s.mu.Lock()
	s.pingAwaitMs = 0
	s.mu.Unlock()
}

// --- lifecycle --------------------------------------------------------

// Close tears the session down: leaves every joined room, announces
// PARTED to remaining members, de-registers, and closes the link.
// Idempotent.
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
		h.touchRoom(r)
		var members [][]byte
		if h.cfg.IncludeJoinedMemberList {
			members = r.memberHashesLocked()
		}
		announce = append(announce, partedRoom{
			recipients: roomLinksLocked(r),
			env:        rrc.Parted(h.identityHash, h.now(), r.Name, members),
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
