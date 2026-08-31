package hub

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thatSFguy/reticulum-relay-chat/internal/history"
	"github.com/thatSFguy/reticulum-relay-chat/internal/roomreg"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// dispatchBody reports whether a MSG/NOTICE body is a hub-local slash
// command. It returns the trimmed body (with the leading slash) when so,
// or "" when the body is not a command.
func dispatchBody(body string) string {
	t := strings.TrimSpace(body)
	if strings.HasPrefix(t, "/") {
		return t
	}
	return ""
}

// handleCommand parses and dispatches a hub-local slash command. room is
// the envelope K_ROOM ("the current room").
//
// Dispatch reads the same table /help prints, so a command cannot exist
// without being documented and /help cannot advertise one that is not
// wired up. That drift is not hypothetical: the shipped default
// greeting told every arriving client "/help for commands" while the
// hub answered "unrecognized command".
func (s *Session) handleCommand(trimmed, _, room string) {
	// A command is an ordinary MSG body, and handleMsg dispatches it
	// BEFORE the body-size check — so max_msg_body_bytes never reached
	// this path. That is not merely a missing cap on one command: every
	// present and future command handler was receiving an argument
	// string bounded only by the 8 KiB link frame, and /topic wrote its
	// share of that straight into rooms.toml and re-broadcast it to the
	// room. Bounded here, once, so a new command cannot reintroduce it.
	//
	// Measured against the same limit an ordinary message gets: a
	// command IS an ordinary message as far as every deployed client is
	// concerned, and a hub that accepts more from one than the other is
	// making a distinction its own clients cannot see.
	if m := s.hub.limits.MaxMsgBodyBytes; m > 0 && len(trimmed) > m {
		s.sendError(roomPtr(room), fmt.Sprintf(
			"command exceeds the hub body-size limit (%d bytes)", m))
		return
	}
	parts := strings.Fields(strings.TrimPrefix(trimmed, "/"))
	if len(parts) == 0 {
		s.sendError(roomPtr(room), "unrecognized command — try /help")
		return
	}
	cmd := strings.ToLower(parts[0])
	spec := commandIndex[cmd]
	if spec == nil {
		// Echo what they typed, bounded: the token is attacker-supplied
		// and may be as long as a whole message body, but it is also
		// the single most useful thing to show back for a typo.
		s.sendError(roomPtr(room), "unrecognized command '/"+snippet(cmd, 24)+"' — try /help")
		return
	}
	// Server-op gating lives here so it is decided by the same table
	// that decides whether /help lists the command at all. Handlers
	// that already check keep their check: this is the filter, not the
	// only lock.
	if spec.serverOp && !s.isServerOp() {
		s.sendError(roomPtr(room), "not authorized")
		return
	}
	spec.run(s, parts, room)
}

// isServerOp reports whether this session's identity is a configured
// server operator.
func (s *Session) isServerOp() bool {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.isServerOp(s.identity())
}

// roomPtr returns a *string for room, or nil when room is "".
func roomPtr(room string) *string {
	if room == "" {
		return nil
	}
	r := room
	return &r
}

// validRoom normalizes and validates a room-name argument. It returns
// the name to use — callers must use the returned value, which is why
// it is returned rather than validated in place.
func (s *Session) validRoom(name string) (string, error) {
	name = normalizeRoomName(name)
	if name == "" {
		return "", fmt.Errorf("empty room name")
	}
	// Reject invalid UTF-8 (audit A7): such a name would be persisted to
	// rooms.toml and make the registry unloadable on the next restart.
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("room name must be valid UTF-8")
	}
	if m := s.hub.limits.MaxRoomNameBytes; m > 0 && len(name) > m {
		return "", fmt.Errorf("room name exceeds the hub limit")
	}
	return name, nil
}

// --- /reload ----------------------------------------------------------

func (s *Session) cmdReload(_ []string, room string) {
	h := s.hub
	h.mu.Lock()
	serverOp := h.isServerOp(s.identity())
	h.mu.Unlock()
	if !serverOp {
		s.sendError(roomPtr(room), "not authorized")
		return
	}
	// UNVERIFIED: rrcd re-reads its config file from disk; this hub keeps
	// no config_path reference after New, so a live config re-read is not
	// possible. We re-derive trust/registry from the in-memory config and
	// re-load persisted klines + registry, which is the auth-gated subset
	// the brief permits.
	h.mu.Lock()
	trustedBefore := len(h.trusted)
	bannedBefore := len(h.banned)
	regBefore := 0
	for _, r := range h.rooms {
		if r.registered {
			regBefore++
		}
	}
	h.reloadTrust()
	h.loadKlines()
	if h.cfg.RoomRegistryPath != "" {
		recs, err := roomreg.LoadRegistry(h.cfg.RoomRegistryPath, h.nowUnix())
		if err == nil {
			for name, rec := range recs {
				if _, live := h.rooms[name]; !live {
					h.rooms[name] = roomFromRecord(name, rec)
				}
			}
		}
	}
	trustedAfter := len(h.trusted)
	bannedAfter := len(h.banned)
	regAfter := 0
	for _, r := range h.rooms {
		if r.registered {
			regAfter++
		}
	}
	h.mu.Unlock()

	s.sendNotice(roomPtr(room), fmt.Sprintf(
		"reloaded: trusted=%d->%d banned=%d->%d registered_rooms=%d->%d",
		trustedBefore, trustedAfter, bannedBefore, bannedAfter, regBefore, regAfter))
}

// --- /stats -----------------------------------------------------------

func (s *Session) cmdStats(_ []string, room string) {
	h := s.hub
	h.mu.Lock()
	serverOp := h.isServerOp(s.identity())
	h.mu.Unlock()
	if !serverOp {
		s.sendError(roomPtr(room), "not authorized")
		return
	}
	s.sendNotice(roomPtr(room), h.snapshotStats())
}

// --- /list ------------------------------------------------------------

func (s *Session) cmdList(_ []string, room string) {
	h := s.hub
	h.mu.Lock()
	type rt struct{ name, topic string }
	var rooms []rt
	for name, r := range h.rooms {
		if r.registered && !r.private {
			rooms = append(rooms, rt{name, r.topic})
		}
	}
	h.mu.Unlock()

	if len(rooms) == 0 {
		s.sendNotice(roomPtr(room), "No public rooms registered")
		return
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].name < rooms[j].name })
	var b strings.Builder
	b.WriteString("Registered public rooms:")
	for _, r := range rooms {
		if r.topic != "" {
			b.WriteString(fmt.Sprintf("\n  %s - %s", r.name, r.topic))
		} else {
			b.WriteString(fmt.Sprintf("\n  %s", r.name))
		}
	}
	s.sendNotice(roomPtr(room), b.String())
}

// --- /who -------------------------------------------------------------

func (s *Session) cmdWho(parts []string, room string) {
	h := s.hub
	target := room
	if len(parts) >= 2 {
		target = parts[1]
	}
	if target == "" {
		s.sendNotice(roomPtr(room), "usage: /who [room]")
		return
	}
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}

	h.mu.Lock()
	serverOp := h.isServerOp(s.identity())
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "members in "+target+": (none)")
		return
	}
	// A member may always see who is in their own room. +p hides a room
	// from OUTSIDE — /list omits it and a stranger is told nothing —
	// but the sibling check in cmdTopic's view path already spelled
	// this out as `private && !serverOp && !hasMember`, and only this
	// copy was missing the last clause. Without it a +p room is one
	// nobody inside it can enumerate either, which is not privacy, just
	// breakage.
	if r.private && !serverOp && !r.hasMember(s) {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "room "+target+" is private")
		return
	}
	var rendered []string
	for m := range r.members {
		rendered = append(rendered, renderMember(m))
	}
	h.mu.Unlock()

	sort.Strings(rendered)
	body := "members in " + target + ": "
	if len(rendered) == 0 {
		body += "(none)"
	} else {
		body += strings.Join(rendered, ", ")
	}
	s.sendNotice(roomPtr(room), body)
}

// --- /kick ------------------------------------------------------------

func (s *Session) cmdKick(parts []string, room string) {
	h := s.hub
	if len(parts) < 3 {
		s.sendNotice(roomPtr(room), "usage: /kick <room> <nick|hashprefix>")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}
	tok := parts[2]

	h.mu.Lock()
	serverOp := h.isServerOp(s.identity())
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	if !r.isOp(s.identityHex(), serverOp) {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	matches := h.resolveTargetLocked(tok, r)
	if len(matches) == 0 {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "target '"+tok+"' not found")
		return
	}
	if len(matches) > 1 {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), ambiguityNotice(matches))
		return
	}
	victim := matches[0].session
	if !r.hasMember(victim) {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "target not in room")
		return
	}
	delete(r.members, victim)
	h.touchRoom(r)
	recipients := roomLinksLocked(r)
	var members [][]byte
	if h.cfg.IncludeJoinedMemberList {
		members = r.memberHashesLocked()
	}
	h.dropRoomIfEmptyLocked(r)
	h.mu.Unlock()

	victim.mu.Lock()
	delete(victim.joined, target)
	victim.mu.Unlock()

	victim.sendError(roomPtr(target), "kicked from "+target)
	h.fanout(recipients, rrc.Parted(h.identityHash, h.now(), target, members))
	s.sendNotice(roomPtr(room), "kicked "+tok+" from "+target)
}

// --- /kline -----------------------------------------------------------

func (s *Session) cmdKline(parts []string, room string) {
	h := s.hub
	h.mu.Lock()
	serverOp := h.isServerOp(s.identity())
	h.mu.Unlock()
	if !serverOp {
		s.sendError(roomPtr(room), "not authorized")
		return
	}
	if len(parts) < 2 {
		s.sendNotice(roomPtr(room), "usage: /kline add|del|list [nick|hashprefix|hash]")
		return
	}
	op := strings.ToLower(parts[1])
	switch op {
	case "list":
		h.mu.Lock()
		hashes := make([]string, 0, len(h.klines))
		for k := range h.klines {
			hashes = append(hashes, k)
		}
		h.mu.Unlock()
		sort.Strings(hashes)
		if len(hashes) == 0 {
			s.sendNotice(roomPtr(room), "klines: (none)")
		} else {
			s.sendNotice(roomPtr(room), "klines: "+strings.Join(hashes, ","))
		}
		return
	case "add", "del":
		// fall through
	default:
		s.sendNotice(roomPtr(room), "usage: /kline add|del|list [nick|hashprefix|hash]")
		return
	}
	if len(parts) < 3 {
		s.sendNotice(roomPtr(room), "usage: /kline <op> <nick|hashprefix|hash>")
		return
	}
	tok := parts[2]
	suffix := ""
	if h.cfg.KlinePath == "" {
		suffix = " (not persisted; no kline_path)"
	}

	if op == "add" {
		h.mu.Lock()
		matches := h.resolveTargetLocked(tok, nil)
		if len(matches) == 1 && matches[0].hashHex != "" {
			hh := matches[0].hashHex
			victim := matches[0].session
			h.klines[hh] = struct{}{}
			h.banned[hh] = struct{}{}
			h.persistKlinesLocked()
			h.mu.Unlock()
			victim.sendError(nil, "banned")
			victim.Close()
			s.sendNotice(roomPtr(room), "kline added for "+tok+suffix)
			return
		}
		if len(matches) > 1 {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), ambiguityNotice(matches))
			return
		}
		h.mu.Unlock()
		// No live match — parse the token as a hash.
		hh, err := parseHexHash(tok)
		if err != nil {
			s.sendNotice(roomPtr(room), "bad identity hash: "+err.Error())
			return
		}
		h.mu.Lock()
		h.klines[hh] = struct{}{}
		h.banned[hh] = struct{}{}
		h.persistKlinesLocked()
		victim := h.sessionForHashLocked(hh)
		h.mu.Unlock()
		if victim != nil {
			victim.sendError(nil, "banned")
			victim.Close()
		}
		s.sendNotice(roomPtr(room), "kline added for "+hh+suffix)
		return
	}

	// op == "del"
	hh, err := parseHexHash(tok)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad identity hash: "+err.Error())
		return
	}
	h.mu.Lock()
	_, banned := h.klines[hh]
	if banned {
		delete(h.klines, hh)
		// Recompute banned set: config-banned stays, kline removed.
		delete(h.banned, hh)
		for _, b := range h.cfg.BannedIdentities {
			if normHex(b) == hh {
				h.banned[hh] = struct{}{}
			}
		}
		h.persistKlinesLocked()
	}
	h.mu.Unlock()
	if banned {
		s.sendNotice(roomPtr(room), "kline removed for "+hh+suffix)
	} else {
		s.sendNotice(roomPtr(room), "not klined: "+hh)
	}
}

// --- /register --------------------------------------------------------

func (s *Session) cmdRegister(parts []string, room string) {
	h := s.hub
	if len(parts) < 2 {
		s.sendNotice(roomPtr(room), "usage: /register <room>")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}

	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil || !r.hasMember(s) {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "must be present in the room to register it")
		return
	}
	if r.founder != s.identityHex() || s.identityHex() == "" {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "only the room founder can register")
		return
	}
	if h.cfg.RoomRegistryPath == "" {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "cannot register room: no room_registry_path")
		return
	}
	// Cap registered rooms per founding identity (audit A3): one peer
	// must not be able to fill rooms.toml with unlimited registrations.
	if h.cfg.MaxRegisteredRoomsPerIdentity > 0 && !r.registered {
		idHex := s.identityHex()
		count := 0
		for _, rr := range h.rooms {
			if rr.registered && rr.founder == idHex {
				count++
			}
		}
		if count >= h.cfg.MaxRegisteredRoomsPerIdentity {
			h.mu.Unlock()
			s.sendError(roomPtr(target), "registered-room limit reached for your identity")
			return
		}
	}
	r.registered = true
	r.noOutsideMsgs = true
	r.topicOpsOnly = true
	r.ops[r.founder] = struct{}{}
	h.touchRoom(r)
	h.markRegistryDirtyLocked()
	h.mu.Unlock()

	s.sendNotice(roomPtr(room), "registered room "+target)
}

// --- /unregister ------------------------------------------------------

func (s *Session) cmdUnregister(parts []string, room string) {
	h := s.hub
	if len(parts) < 2 {
		s.sendNotice(roomPtr(room), "usage: /unregister <room>")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}

	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil || !r.hasMember(s) {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "must be present in the room to register it")
		return
	}
	if r.founder != s.identityHex() || s.identityHex() == "" {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "only the room founder can unregister")
		return
	}
	if !r.registered {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "room "+target+" is not registered")
		return
	}
	r.registered = false
	h.markRegistryDirtyLocked()
	// Drop in-memory state if the room is now empty.
	if len(r.members) == 0 {
		delete(h.rooms, target)
	}
	h.mu.Unlock()

	s.sendNotice(roomPtr(room), "unregistered room "+target)
}

// --- /topic -----------------------------------------------------------

func (s *Session) cmdTopic(parts []string, room string) {
	h := s.hub
	if len(parts) < 2 {
		s.sendNotice(roomPtr(room), "usage: /topic <room> [topic]")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}

	if len(parts) == 2 {
		// View. A private (+p) room's topic is disclosed only to members
		// and server-ops (A13) — /list already hides +p rooms, /topic must
		// not leak them.
		h.mu.Lock()
		topic := ""
		if r := h.roomLocked(target); r != nil {
			if r.private && !h.isServerOp(s.identity()) && !r.hasMember(s) {
				h.mu.Unlock()
				s.sendNotice(roomPtr(room), "room "+target+" is private")
				return
			}
			topic = r.topic
		}
		h.mu.Unlock()
		if topic == "" {
			topic = "(none)"
		}
		s.sendNotice(roomPtr(room), "topic for "+target+": "+topic)
		return
	}

	newTopic := strings.Join(parts[2:], " ")
	// A topic must be valid UTF-8 (audit A7) — it is persisted to
	// rooms.toml for registered rooms.
	if !utf8.ValidString(newTopic) {
		s.sendError(roomPtr(target), "topic must be valid UTF-8")
		return
	}
	// And it must be bounded. handleCommand already caps the whole
	// command line, but the topic is the part that reaches DISK and is
	// re-sent to every future joiner in the room-info NOTICE, so it
	// carries its own limit rather than inheriting whatever the command
	// bound happens to be.
	if m := h.cfg.MaxTopicBytes; m > 0 && len(newTopic) > m {
		s.sendError(roomPtr(target), fmt.Sprintf(
			"topic exceeds the hub limit (%d bytes)", m))
		return
	}
	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "bad room: no such room")
		return
	}
	serverOp := h.isServerOp(s.identity())
	// Setting a topic requires being IN the room.
	//
	// Membership is the authorization, for the same reason /history
	// gives: joining already cleared this room's ban, key and invite
	// gates, so "you may change what this room is about" reduces to
	// "you are in here" without re-deriving a second, divergent copy of
	// the join gate. Without this a peer BANNED from the room could
	// still rewrite its topic — commands are dispatched before any room
	// gate — and for a registered room that edit persists to
	// rooms.toml and is broadcast to every member.
	//
	// Note the view path a few lines above already worked this way for
	// +p rooms; only the set path was missing it.
	if !r.hasMember(s) && !serverOp {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "join the room to set its topic")
		return
	}
	if r.topicOpsOnly && !r.isOp(s.identityHex(), serverOp) {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized (+t)")
		return
	}
	r.topic = newTopic
	h.touchRoom(r)
	if r.registered {
		h.markRegistryDirtyLocked()
	}
	recipients := roomLinksLocked(r)
	h.mu.Unlock()

	disp := newTopic
	if disp == "" {
		disp = "(cleared)"
	}
	h.fanout(recipients, rrc.Notice(h.identityHash, h.now(), roomPtr(target),
		"topic for "+target+" is now: "+disp))
}

// --- /op /deop /voice /devoice ----------------------------------------

func (s *Session) cmdOpVoice(cmd string, parts []string, room string) {
	h := s.hub
	if len(parts) < 3 {
		s.sendNotice(roomPtr(room), "usage: /"+cmd+" <room> <nick|hashprefix|hash>")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}
	tok := parts[2]

	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	serverOp := h.isServerOp(s.identity())
	if !r.isOp(s.identityHex(), serverOp) {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	matches := h.resolveTargetLocked(tok, nil)
	if len(matches) == 0 {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "target '"+tok+"' not found")
		return
	}
	if len(matches) > 1 {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), ambiguityNotice(matches))
		return
	}
	hh := matches[0].hashHex
	var reply string
	switch cmd {
	case "op":
		if !aclHasRoom(r.ops, hh, h.cfg.MaxRoomAclEntries) {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "operator list for "+target+" is full")
			return
		}
		r.ops[hh] = struct{}{}
		reply = "op granted in " + target
	case "deop":
		if hh == r.founder {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "cannot deop founder")
			return
		}
		delete(r.ops, hh)
		reply = "op removed in " + target
	case "voice":
		if !aclHasRoom(r.voiced, hh, h.cfg.MaxRoomAclEntries) {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "voice list for "+target+" is full")
			return
		}
		r.voiced[hh] = struct{}{}
		reply = "voice granted in " + target
	case "devoice":
		delete(r.voiced, hh)
		reply = "voice removed in " + target
	}
	h.touchRoom(r)
	if r.registered {
		h.markRegistryDirtyLocked()
	}
	h.mu.Unlock()

	s.sendNotice(roomPtr(room), reply)
}

// --- /mode ------------------------------------------------------------

func (s *Session) cmdMode(parts []string, room string) {
	h := s.hub
	if len(parts) < 3 {
		s.sendNotice(roomPtr(room),
			"supported modes: +m -m +i -i +k -k +t -t +n -n +p -p +r -r +o -o +v -v")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}
	flag := strings.ToLower(parts[2])

	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	serverOp := h.isServerOp(s.identity())
	if !r.isOp(s.identityHex(), serverOp) {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}

	switch flag {
	case "+m", "-m", "+i", "-i", "+t", "-t", "+n", "-n", "+p", "-p":
		set := flag[0] == '+'
		switch flag[1] {
		case 'm':
			r.moderated = set
		case 'i':
			r.inviteOnly = set
		case 't':
			r.topicOpsOnly = set
		case 'n':
			r.noOutsideMsgs = set
		case 'p':
			r.private = set
		}
		h.touchRoom(r)
		if r.registered {
			h.markRegistryDirtyLocked()
		}
		modeStr := r.modeString()
		recipients := roomLinksLocked(r)
		h.mu.Unlock()
		h.fanout(recipients, rrc.Notice(h.identityHash, h.now(), roomPtr(target),
			"mode for "+target+" is now: "+modeStr))
		return

	case "+k":
		if len(parts) < 4 {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "usage: /mode <room> +k <key>")
			return
		}
		key := strings.Join(parts[3:], " ")
		if key == "" {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "key must not be empty")
			return
		}
		r.key = key
		h.touchRoom(r)
		if r.registered {
			h.markRegistryDirtyLocked()
		}
		modeStr := r.modeString()
		recipients := roomLinksLocked(r)
		h.mu.Unlock()
		h.fanout(recipients, rrc.Notice(h.identityHash, h.now(), roomPtr(target),
			"mode for "+target+" is now: "+modeStr))
		return

	case "-k":
		r.key = ""
		h.touchRoom(r)
		if r.registered {
			h.markRegistryDirtyLocked()
		}
		modeStr := r.modeString()
		recipients := roomLinksLocked(r)
		h.mu.Unlock()
		h.fanout(recipients, rrc.Notice(h.identityHash, h.now(), roomPtr(target),
			"mode for "+target+" is now: "+modeStr))
		return

	case "+r", "-r":
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "use /register or /unregister to change +r")
		return

	case "+o", "-o", "+v", "-v":
		if len(parts) < 4 {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room),
				"usage: /mode <room> "+flag+" <nick|hashprefix|hash>")
			return
		}
		tok := parts[3]
		matches := h.resolveTargetLocked(tok, nil)
		if len(matches) == 0 {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "target '"+tok+"' not found")
			return
		}
		if len(matches) > 1 {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), ambiguityNotice(matches))
			return
		}
		hh := matches[0].hashHex
		set := flag[0] == '+'
		switch flag[1] {
		case 'o':
			if !set && hh == r.founder {
				h.mu.Unlock()
				s.sendNotice(roomPtr(room), "cannot deop founder")
				return
			}
			if set {
				if !aclHasRoom(r.ops, hh, h.cfg.MaxRoomAclEntries) {
					h.mu.Unlock()
					s.sendNotice(roomPtr(room), "operator list for "+target+" is full")
					return
				}
				r.ops[hh] = struct{}{}
			} else {
				delete(r.ops, hh)
			}
		case 'v':
			if set {
				if !aclHasRoom(r.voiced, hh, h.cfg.MaxRoomAclEntries) {
					h.mu.Unlock()
					s.sendNotice(roomPtr(room), "voice list for "+target+" is full")
					return
				}
				r.voiced[hh] = struct{}{}
			} else {
				delete(r.voiced, hh)
			}
		}
		h.touchRoom(r)
		if r.registered {
			h.markRegistryDirtyLocked()
		}
		recipients := roomLinksLocked(r)
		short := hh
		if len(short) > 12 {
			short = short[:12]
		}
		h.mu.Unlock()
		h.fanout(recipients, rrc.Notice(h.identityHash, h.now(), roomPtr(target),
			"mode for "+target+" is now: "+flag+" "+short))
		return

	default:
		h.mu.Unlock()
		s.sendNotice(roomPtr(room),
			"supported modes: +m -m +i -i +k -k +t -t +n -n +p -p +r -r +o -o +v -v")
		return
	}
}

// --- /ban -------------------------------------------------------------

func (s *Session) cmdBan(parts []string, room string) {
	h := s.hub
	if len(parts) < 3 {
		s.sendNotice(roomPtr(room), "usage: /ban <room> add|del|list [nick|hashprefix|hash]")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}
	op := strings.ToLower(parts[2])

	if op == "list" {
		h.mu.Lock()
		r := h.roomLocked(target)
		var bans []string
		if r != nil {
			bans = sortedHexSet(r.bans)
		}
		h.mu.Unlock()
		if len(bans) == 0 {
			s.sendNotice(roomPtr(room), "no bans in "+target)
		} else {
			s.sendNotice(roomPtr(room), "bans in "+target+": "+strings.Join(bans, ","))
		}
		return
	}
	if op != "add" && op != "del" {
		s.sendNotice(roomPtr(room), "usage: /ban <room> add|del|list [nick|hashprefix|hash]")
		return
	}
	if len(parts) < 4 {
		s.sendNotice(roomPtr(room), "usage: /ban <r> <op> <nick|hashprefix|hash>")
		return
	}
	tok := parts[3]

	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	serverOp := h.isServerOp(s.identity())
	if !r.isOp(s.identityHex(), serverOp) {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}

	// Resolve target to a hash: prefer a live match, fall back to a hash
	// token.
	hh := ""
	matches := h.resolveTargetLocked(tok, nil)
	if len(matches) == 1 && matches[0].hashHex != "" {
		hh = matches[0].hashHex
	} else if len(matches) > 1 {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), ambiguityNotice(matches))
		return
	} else {
		parsed, err := parseHexHash(tok)
		if err != nil {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "bad identity hash: "+err.Error())
			return
		}
		hh = parsed
	}

	if op == "add" {
		if !aclHasRoom(r.bans, hh, h.cfg.MaxRoomAclEntries) {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "ban list for "+target+" is full")
			return
		}
		r.bans[hh] = struct{}{}
		h.touchRoom(r)
		if r.registered {
			h.markRegistryDirtyLocked()
		}
		var victims []*Session
		for m := range r.members {
			if id := m.identity(); id != nil && hex.EncodeToString(id) == hh {
				victims = append(victims, m)
			}
		}
		for _, v := range victims {
			delete(r.members, v)
		}
		recipients := roomLinksLocked(r)
		var members [][]byte
		if h.cfg.IncludeJoinedMemberList {
			members = r.memberHashesLocked()
		}
		h.dropRoomIfEmptyLocked(r)
		h.mu.Unlock()
		for _, v := range victims {
			v.mu.Lock()
			delete(v.joined, target)
			v.mu.Unlock()
			v.sendError(roomPtr(target), "banned from "+target)
		}
		h.fanout(recipients, rrc.Parted(h.identityHash, h.now(), target, members))
		s.sendNotice(roomPtr(room), "ban added in "+target)
		return
	}

	// op == "del"
	delete(r.bans, hh)
	h.touchRoom(r)
	if r.registered {
		h.markRegistryDirtyLocked()
	}
	h.mu.Unlock()
	s.sendNotice(roomPtr(room), "ban removed in "+target)
}

// --- /invite ----------------------------------------------------------

func (s *Session) cmdInvite(parts []string, room string) {
	h := s.hub
	if len(parts) < 3 {
		s.sendNotice(roomPtr(room), "usage: /invite <room> add|del|list [nick|hashprefix|hash]")
		return
	}
	target := parts[1]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}
	op := strings.ToLower(parts[2])

	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	serverOp := h.isServerOp(s.identity())
	if !r.isOp(s.identityHex(), serverOp) {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized")
		return
	}
	r.pruneInvitesLocked(h.nowUnix())

	if op == "list" {
		var entries []string
		now := h.nowUnix()
		for hh, exp := range r.invited {
			entries = append(entries, fmt.Sprintf("%s expires_in=%.0fs", hh, exp-now))
		}
		h.mu.Unlock()
		sort.Strings(entries)
		if len(entries) == 0 {
			s.sendNotice(roomPtr(room), "invites in "+target+": (none)")
		} else {
			s.sendNotice(roomPtr(room), "invites in "+target+": "+strings.Join(entries, ", "))
		}
		return
	}
	if op != "add" && op != "del" {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "usage: /invite <room> add|del|list [nick|hashprefix|hash]")
		return
	}
	if len(parts) < 4 {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "usage: /invite <room> "+op+" <nick|hashprefix|hash>")
		return
	}
	tok := parts[3]

	if op == "add" {
		matches := h.resolveTargetLocked(tok, nil)
		if len(matches) == 0 {
			h.mu.Unlock()
			s.sendError(roomPtr(target), "invite failed: target '"+tok+"' not found")
			return
		}
		if len(matches) > 1 {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), ambiguityNotice(matches))
			return
		}
		victim := matches[0].session
		hh := matches[0].hashHex
		if hh == "" {
			h.mu.Unlock()
			s.sendError(roomPtr(target), "invite failed: target not identified")
			return
		}
		keyed := r.key != ""
		inviteRoom := r.inviteOnly
		expiring := keyed || inviteRoom
		ttl := h.inviteTTL()
		var expiresIn int
		if expiring {
			exp := h.nowUnix() + ttl.Seconds()
			r.invited[hh] = exp
			expiresIn = int(ttl.Seconds())
			if r.registered {
				h.markRegistryDirtyLocked()
			}
		}
		h.mu.Unlock()

		// Always notify the invited peer.
		if keyed {
			victim.sendNotice(roomPtr(target),
				"You have been invited to join "+target+
					". This invite allows joining without the key (+k).")
		} else {
			victim.sendNotice(roomPtr(target), "You have been invited to join "+target+".")
		}
		if expiring {
			s.sendNotice(roomPtr(room),
				fmt.Sprintf("invite added in %s (expires in %ds)", target, expiresIn))
		} else {
			s.sendNotice(roomPtr(room), "invite sent to "+tok+" for "+target)
		}
		return
	}

	// op == "del"
	hh := ""
	matches := h.resolveTargetLocked(tok, nil)
	if len(matches) == 1 && matches[0].hashHex != "" {
		hh = matches[0].hashHex
	} else if len(matches) > 1 {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), ambiguityNotice(matches))
		return
	} else {
		parsed, err := parseHexHash(tok)
		if err != nil {
			h.mu.Unlock()
			s.sendNotice(roomPtr(room), "bad identity hash: "+err.Error())
			return
		}
		hh = parsed
	}
	delete(r.invited, hh)
	if r.registered {
		h.markRegistryDirtyLocked()
	}
	h.mu.Unlock()
	s.sendNotice(roomPtr(room), "invite removed in "+target)
}

// historyPullIntervalMs is the minimum gap between one session's
// /history requests. A join replay is small and self-limiting; a pull
// is neither, so it gets its own throttle on top of the shared token
// bucket.
const historyPullIntervalMs = 15_000

// cmdHistory serves a client more of a room's transcript than the join
// replay carried, and lets a room operator purge it.
//
//	/history [room] [count]
//	/history purge <room>
//
// It needs nothing from the client but the ability to send text, which
// is the point: a slash command is an ordinary MSG body, so every
// deployed RRC client already supports this without being changed.
func (s *Session) cmdHistory(parts []string, room string) {
	h := s.hub
	if !h.historyEnabled() {
		s.sendNotice(roomPtr(room), "this hub does not retain history")
		return
	}

	if len(parts) >= 2 && strings.EqualFold(parts[1], "purge") {
		s.historyPurge(parts, room)
		return
	}

	target := room
	if len(parts) >= 2 {
		target = parts[1]
	}
	if target == "" {
		s.sendNotice(nil, "usage: /history [room] [count]")
		return
	}
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}

	count := h.cfg.HistoryPullCount
	if len(parts) >= 3 {
		n, err := strconv.Atoi(parts[2])
		if err != nil || n <= 0 {
			s.sendNotice(roomPtr(room), "usage: /history [room] [count]")
			return
		}
		if n < count {
			count = n
		}
	}

	// Throttle before reading anything. One inbound MSG costs the
	// client a single token from a 240/min bucket and can make the hub
	// emit HistoryPullCount messages / HistoryPullBytes of traffic —
	// roughly a 16 KB answer to a 30-byte question. Every other bound
	// in this feature caps one call; this one caps the rate, which is
	// what matters on a hub whose clients may be a LoRa link away.
	now := h.now()
	s.mu.Lock()
	last := s.lastHistoryPullMs
	ready := last == 0 || now-last >= historyPullIntervalMs
	if ready {
		s.lastHistoryPullMs = now
	}
	s.mu.Unlock()
	if !ready {
		wait := (historyPullIntervalMs - (now - last) + 999) / 1000
		s.sendNotice(roomPtr(room), fmt.Sprintf("history is rate limited; try again in %ds", wait))
		return
	}

	// Membership is the authorization: joining already cleared the
	// room's key, invite and ban checks, so "you may read what was said
	// here" reduces to "you are in here". Re-deriving those checks
	// against a transcript would be a second, divergent copy of the
	// join gate.
	h.mu.Lock()
	r := h.roomLocked(target)
	member := r != nil && r.hasMember(s)
	h.mu.Unlock()
	if !member {
		s.sendError(roomPtr(target), "join the room to read its history")
		return
	}

	if n := s.replayTo(target, history.Query{
		Limit:    count,
		MaxBytes: h.cfg.HistoryPullBytes,
		Since:    time.Now().Add(-h.cfg.HistoryRetention.Duration),
	}); n == 0 {
		s.sendNotice(roomPtr(target), "no history for "+target)
	}
}

// historyPurge drops a room's transcript on an operator's say-so.
func (s *Session) historyPurge(parts []string, room string) {
	h := s.hub
	if len(parts) < 3 {
		s.sendNotice(roomPtr(room), "usage: /history purge <room>")
		return
	}
	target := parts[2]
	target, err := s.validRoom(target)
	if err != nil {
		s.sendNotice(roomPtr(room), "bad room: "+err.Error())
		return
	}

	h.mu.Lock()
	serverOp := h.isServerOp(s.identity())
	r := h.roomLocked(target)
	authorized := r != nil && r.isOp(s.identityHex(), serverOp)
	h.mu.Unlock()
	if !authorized {
		s.sendError(roomPtr(target), "not authorized")
		return
	}

	h.dropHistory(target)
	h.log.Printf("%s purged the history of #%s", shortHash(s.identity()), target)
	s.sendNotice(roomPtr(target), "history for "+target+" purged")
}

// cmdNotify is the whole of a peer's relationship with mention
// notification: the preference, and the diagnosis when it appears not
// to work.
//
//	/notify [status]    — what the hub will do, and whether it can
//	/notify on|off      — change the preference
//	/notify address     — where a notification would be sent
//	/notify test        — send one now, direct route, answer in ~10s
//	/notify test full   — the whole production path, including the
//	                      store-and-forward fallback
//
// Consent matters here in a way it does not for the rest of the hub: a
// mention notification can leave this hub entirely and arrive in
// somebody's LXMF client, so anyone must be able to switch it off
// without an operator's help.
//
// Diagnosis matters for a different reason. Every way this feature
// fails — opted out, a nick that resolves to nobody, an LXMF identity
// that is not the RRC one, a stamp the hub cannot meet, a propagation
// node that accepts and serves nothing — fails SILENTLY, and the
// evidence lands in a log the affected person cannot read. "It does not
// work" is all they can report. These subcommands hand them the log
// line instead.
func (s *Session) cmdNotify(parts []string, room string) {
	sub := "status"
	if len(parts) >= 2 {
		sub = strings.ToLower(parts[1])
	}
	switch sub {
	case "status":
		s.notifyStatus(room)
	case "on", "yes", "enable", "off", "no", "disable":
		s.notifySetPreference(sub, room)
	case "address", "addr", "where":
		s.notifyAddress(room)
	case "test":
		s.notifyTest(parts, room)
	default:
		s.sendNotice(roomPtr(room), "usage: /notify [status|on|off|address|test|test full]")
	}
}

// notifyStatus reports what the hub will do for this peer and, where it
// cannot, why.
func (s *Session) notifyStatus(room string) {
	h := s.hub
	if !h.cfg.MentionNotify {
		s.sendNoticeLines(roomPtr(room), []string{
			"this hub does not send mention notifications",
			"  its operator has not enabled mention_notify; being named here reaches you only if you are reading the room",
		})
		return
	}
	idHex := s.identityHex()
	if idHex == "" {
		s.sendError(roomPtr(room), "identify first")
		return
	}

	h.mu.Lock()
	p := h.peers[idHex]
	optOut := p != nil && p.NotifyOptOut
	pending := 0
	var pubKey []byte
	if p != nil {
		pending = len(p.Mentions)
		pubKey = append([]byte(nil), p.PublicKey...)
	}
	lastPush, pushed := h.lastPush[idHex]
	knownNick := ""
	if p != nil {
		knownNick = p.Nick
	}
	h.mu.Unlock()

	lines := []string{s.notifyStateLine()}
	if optOut {
		lines = append(lines, "  /notify on to switch them back on")
		s.sendNoticeLines(roomPtr(room), lines)
		return
	}

	// How you can be named at all. A mention resolves by nick only when
	// that nick is unambiguous, and a peer with no nick can be named
	// solely by hash — neither of which anybody would guess.
	if knownNick != "" {
		if n := h.identitiesWithNick(knownNick); n > 1 {
			lines = append(lines, fmt.Sprintf(
				"  you are @%s — but %d identities use that nick, so @%s alone names nobody. @%s always works",
				knownNick, n, knownNick, idHex[:8]))
		} else {
			lines = append(lines, fmt.Sprintf("  you are named as @%s, or as @%s", knownNick, idHex[:8]))
		}
	} else {
		lines = append(lines, fmt.Sprintf(
			"  you have no nick, so you can only be named as @%s — your client sends a nick in HELLO", idHex[:8]))
	}

	// Whether anything can reach you while you are gone.
	switch {
	case !h.cfg.MentionLXMF:
		lines = append(lines, "  while you are away: held here and handed over when you next connect (this hub does not forward over LXMF)")
	case h.offlineNotifier() == nil:
		lines = append(lines, "  while you are away: held here only — LXMF forwarding is configured but no notifier is running")
	default:
		if route, ok := h.notifyRoute(pubKey); ok {
			lines = append(lines, "  while you are away: sent to "+route.Address)
			if !route.Announced {
				lines = append(lines, "    the hub has not heard that address announce. If that is not an address your")
				lines = append(lines, "    messaging client owns, notifications cannot arrive — compare it with yours")
			}
			for _, n := range route.Notes {
				lines = append(lines, "    "+n)
			}
		} else {
			lines = append(lines, "  while you are away: sent over LXMF (the notifier cannot describe the route)")
		}
	}

	if pending > 0 {
		lines = append(lines, fmt.Sprintf("  %d mention(s) waiting — /mentions to read them", pending))
	}
	if pushed {
		lines = append(lines, "  last send attempt "+humanAgo(time.Since(lastPush)))
	}
	lines = append(lines, "  /notify test sends one to yourself now and reports what happened")
	s.sendNoticeLines(roomPtr(room), lines)
}

// identitiesWithNick counts directory entries claiming a nick. A count
// above one is why a mention by that nick resolves to nobody.
func (h *Hub) identitiesWithNick(nick string) int {
	want := strings.ToLower(strings.TrimSpace(nick))
	if want == "" {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.peers {
		if p.Nick != "" && strings.ToLower(p.Nick) == want {
			n++
		}
	}
	return n
}

// notifySetPreference records the opt-in/opt-out.
func (s *Session) notifySetPreference(sub, room string) {
	h := s.hub
	if !h.cfg.MentionNotify {
		s.sendNotice(roomPtr(room), "this hub does not send mention notifications")
		return
	}
	idHex := s.identityHex()
	if idHex == "" {
		s.sendError(roomPtr(room), "identify first")
		return
	}
	optOut := sub == "off" || sub == "no" || sub == "disable"

	h.mu.Lock()
	p, ok := h.peers[idHex]
	if !ok {
		h.mu.Unlock()
		// Only an identified peer has a directory entry, and only a
		// directory entry can be reached later — so there is nothing to
		// set a preference on.
		s.sendError(roomPtr(room), "identify first")
		return
	}
	p.NotifyOptOut = optOut
	if optOut {
		// Turning notifications off discards what is already waiting.
		// Holding them would deliver, on the next connection, exactly
		// the thing that was just declined.
		p.Mentions = nil
	}
	h.peersDirty = true
	h.mu.Unlock()

	if optOut {
		s.sendNotice(roomPtr(room), "mention notifications off; anything pending was discarded")
		return
	}
	s.sendNotice(roomPtr(room), "mention notifications on — /notify status for how you will be reached")
}

// notifyAddress prints where a notification would go.
//
// This one line settles the commonest cause of "notifications do not
// work": a client whose messaging identity is not the identity it
// connects to RRC with. The hub derives the destination from the key
// proved at LINKIDENTIFY and has no way to know it is the wrong one —
// but the person holding the phone can see in a second whether the
// address matches theirs.
func (s *Session) notifyAddress(room string) {
	h := s.hub
	idHex := s.identityHex()
	if idHex == "" {
		s.sendError(roomPtr(room), "identify first")
		return
	}
	pubKey := s.PeerPublicKey()
	if len(pubKey) == 0 {
		s.sendNotice(roomPtr(room), "the hub holds no public key for you, so it cannot address you at all")
		return
	}
	route, ok := h.notifyRoute(pubKey)
	if !ok {
		s.sendNotice(roomPtr(room), "this hub has no notifier that can reach you while you are away")
		return
	}
	lines := []string{
		"the hub would notify you at " + route.Address,
		"  derived from the key you proved at connect — if your messaging client shows a",
		"  different address, that is why nothing arrives",
	}
	if !route.Announced {
		lines = append(lines, "  the hub has not heard this address announce; it does not know your stamp cost")
	}
	for _, n := range route.Notes {
		lines = append(lines, "  "+n)
	}
	s.sendNoticeLines(roomPtr(room), lines)
}

// notifyTestInterval throttles /notify test per identity.
//
// The test is a real message over a real mesh — the whole point is that
// it takes the production path and not a simulation of it — so it costs
// airtime somebody else is sharing. Long enough that it cannot be used
// to make the hub flood, short enough to iterate on a misconfiguration.
const notifyTestInterval = 5 * time.Minute

// notifyTest sends one notification to the caller, right now, through
// exactly the path a real mention takes, and reports the verbatim
// outcome.
//
// A dry run would be cheaper and would prove less. Three separate
// incidents in this hub's history were fixes to a path that each worked
// and changed nothing at the far end, because the failure was in a hop
// the sending side cannot inspect: a node that accepts an upload and
// serves nothing back, a recipient enforcing a stamp the hub could not
// meet, a delivery destination the hub had registered but never
// announced. None of those is visible without sending.
func (s *Session) notifyTest(parts []string, room string) {
	h := s.hub
	if !h.cfg.MentionNotify {
		s.sendNotice(roomPtr(room), "this hub does not send mention notifications")
		return
	}
	idHex := s.identityHex()
	if idHex == "" {
		s.sendError(roomPtr(room), "identify first")
		return
	}
	pubKey := s.PeerPublicKey()
	if len(pubKey) == 0 {
		s.sendNotice(roomPtr(room), "the hub holds no public key for you, so it cannot address you at all")
		return
	}
	n := h.offlineNotifier()
	if n == nil {
		s.sendNotice(roomPtr(room), "this hub has no notifier that can reach you while you are away — a mention would be held until you next connect")
		return
	}

	now := time.Now()
	h.mu.Lock()
	last, throttled := h.lastNotifyTest[idHex]
	if throttled && now.Sub(last) < notifyTestInterval {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "already tested recently; try again in "+
			humanUptime(notifyTestInterval-now.Sub(last)))
		return
	}
	h.lastNotifyTest[idHex] = now
	h.sweepNotifyTestsLocked(now)
	h.mu.Unlock()

	hubName := h.cfg.Name
	if hubName == "" {
		hubName = "RRC hub"
	}
	title := hubName + ": notification test"
	body := "This is a test notification you asked " + hubName +
		" for with /notify test. If you are reading it in your messaging client, mention notifications reach you."

	// The direct route by default; the whole production path on
	// "/notify test full".
	//
	// The default is direct because this command's value is that the
	// person who typed it READS the answer, and the full path often
	// cannot deliver one in time: the fallback tries several
	// propagation nodes, each costing up to 20 seconds when its LRPROOF
	// times out. Measured here on a public mesh: 45 seconds to produce
	// a result, by which point the client had disconnected and the
	// answer went into a dead link ("rrc: link send failed: link no
	// longer active"). The user saw "the result follows" and then
	// nothing at all.
	//
	// Nothing diagnostic is lost by that default. A propagation upload
	// is acknowledged by the NODE and never by the recipient, so its
	// outcome is ErrDeliveredUnconfirmed whatever happens — it cannot
	// answer "did it reach me?", which is what the user is asking.
	//
	// "full" exists for the question the OPERATOR asks instead: can
	// this hub reach a propagation node at all? That is a real question
	// with no other way to ask it, and it is worth the wait to someone
	// who chose it deliberately.
	full := len(parts) >= 3 && strings.EqualFold(parts[2], "full")
	direct, _ := n.(DirectNotifier)
	send := func() error {
		if direct != nil && !full {
			return direct.NotifyDirect(pubKey, title, body)
		}
		return n.NotifyAbsent(pubKey, title, body)
	}
	wait := "about ten seconds"
	if full || direct == nil {
		wait = "up to a minute — stay connected or you will miss it"
	}

	// The send blocks — a link handshake and a proof — and this is the
	// inbound frame path. Answer immediately, report when there is
	// something to report.
	s.sendNotice(roomPtr(room), "sending a test notification to "+idHex[:8]+
		"… — the result follows in "+wait)
	go func() {
		err := send()
		var text string
		switch {
		case err == nil:
			text = "test notification delivered, and your client acknowledged it. Mention notifications work for you."
		case errors.Is(err, ErrDeliveredUnconfirmed):
			text = "test notification sent, but nothing confirms it arrived: " + err.Error() +
				" — if it did not appear in your messaging client, that client is probably not the identity you connect here with (/notify address)"
		case errors.Is(err, ErrNotifierUnavailable):
			text = "the hub had no route to even try: " + err.Error() +
				" — this is normal for a minute or two after the hub starts. Try again shortly."
		case full:
			text = "every route failed: " + err.Error() +
				" — neither a direct send nor any propagation node could take it."
		default:
			text = "no direct route to you: " + err.Error() +
				" — a real mention would also try store-and-forward, which may still reach you, but nothing can confirm that. " +
				"Check /notify address matches your messaging client, or /notify test full to try the fallback too."
		}
		h.log.Printf("notify test for %s…: %v", idHex[:8], err)
		// A result nobody can read is worth a log line rather than a
		// silent write into a closed link.
		if s.isClosed() {
			h.log.Printf("notify test for %s…: session gone before the result could be delivered", idHex[:8])
			return
		}
		s.sendNotice(roomPtr(room), text)
	}()
}

// sweepNotifyTestsLocked forgets throttle entries that can no longer
// suppress anything, so the map cannot grow without bound as
// identities come and go. Caller must hold h.mu.
func (h *Hub) sweepNotifyTestsLocked(now time.Time) {
	for id, last := range h.lastNotifyTest {
		if now.Sub(last) >= notifyTestInterval {
			delete(h.lastNotifyTest, id)
		}
	}
}
