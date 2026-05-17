package hub

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

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
func (s *Session) handleCommand(trimmed, _, room string) {
	parts := strings.Fields(strings.TrimPrefix(trimmed, "/"))
	if len(parts) == 0 {
		s.sendError(roomPtr(room), "unrecognized command")
		return
	}
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "reload":
		s.cmdReload(parts, room)
	case "stats":
		s.cmdStats(parts, room)
	case "list":
		s.cmdList(parts, room)
	case "who", "names":
		s.cmdWho(parts, room)
	case "kick":
		s.cmdKick(parts, room)
	case "kline":
		s.cmdKline(parts, room)
	case "register":
		s.cmdRegister(parts, room)
	case "unregister":
		s.cmdUnregister(parts, room)
	case "topic":
		s.cmdTopic(parts, room)
	case "op", "deop", "voice", "devoice":
		s.cmdOpVoice(cmd, parts, room)
	case "mode":
		s.cmdMode(parts, room)
	case "ban":
		s.cmdBan(parts, room)
	case "invite":
		s.cmdInvite(parts, room)
	default:
		s.sendError(roomPtr(room), "unrecognized command")
	}
}

// roomPtr returns a *string for room, or nil when room is "".
func roomPtr(room string) *string {
	if room == "" {
		return nil
	}
	r := room
	return &r
}

// validRoom validates a room-name argument against the configured limit.
func (s *Session) validRoom(name string) error {
	if name == "" {
		return fmt.Errorf("empty room name")
	}
	if m := s.hub.limits.MaxRoomNameBytes; m > 0 && len(name) > m {
		return fmt.Errorf("room name exceeds the hub limit")
	}
	return nil
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
	if err := s.validRoom(target); err != nil {
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
	if r.private && !serverOp {
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
	if err := s.validRoom(target); err != nil {
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
	if err := s.validRoom(target); err != nil {
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
	h.persistRegistryLocked()
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
	if err := s.validRoom(target); err != nil {
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
	h.persistRegistryLocked()
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
	if err := s.validRoom(target); err != nil {
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
	h.mu.Lock()
	r := h.roomLocked(target)
	if r == nil {
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), "bad room: no such room")
		return
	}
	serverOp := h.isServerOp(s.identity())
	if r.topicOpsOnly && !r.isOp(s.identityHex(), serverOp) {
		h.mu.Unlock()
		s.sendError(roomPtr(target), "not authorized (+t)")
		return
	}
	r.topic = newTopic
	h.touchRoom(r)
	if r.registered {
		h.persistRegistryLocked()
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
	if err := s.validRoom(target); err != nil {
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
		h.persistRegistryLocked()
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
	if err := s.validRoom(target); err != nil {
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
			h.persistRegistryLocked()
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
			h.persistRegistryLocked()
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
			h.persistRegistryLocked()
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
			h.persistRegistryLocked()
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
	if err := s.validRoom(target); err != nil {
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
			h.persistRegistryLocked()
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
		h.persistRegistryLocked()
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
	if err := s.validRoom(target); err != nil {
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
				h.persistRegistryLocked()
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
		h.persistRegistryLocked()
	}
	h.mu.Unlock()
	s.sendNotice(roomPtr(room), "invite removed in "+target)
}
