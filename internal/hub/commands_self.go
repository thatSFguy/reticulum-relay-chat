package hub

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thatSFguy/reticulum-relay-chat/internal/peerreg"
)

// Commands about yourself.
//
// The hub knows a great deal about each peer — the identity it verified,
// the nick it filed, whether it believes you are present, the address it
// would use to reach you when you are not — and until now said none of
// it out loud. All of that lands in the log, which the person it
// concerns cannot read. These commands say it to them instead.

// --- /whoami ----------------------------------------------------------

func (s *Session) cmdWhoami(_ []string, room string) {
	h := s.hub
	idHex := s.identityHex()
	if idHex == "" {
		s.sendNotice(roomPtr(room), "you have not identified — your client must send LINKIDENTIFY (SPEC §6.7.6)")
		return
	}

	s.mu.Lock()
	nick := s.nick
	away := s.away
	awayReason := s.awayReason
	awaySince := s.awaySinceMs
	rooms := make([]string, 0, len(s.joined))
	for name := range s.joined {
		rooms = append(rooms, name)
	}
	s.mu.Unlock()
	sort.Strings(rooms)

	lines := []string{"you are " + idHex}
	if nick != "" {
		lines = append(lines, fmt.Sprintf("  nick: %s — sent once in HELLO, so changing it means reconnecting", nick))
	} else {
		lines = append(lines, "  nick: (none) — you can only be named by identity hash, e.g. @"+idHex[:8])
	}
	if len(rooms) > 0 {
		lines = append(lines, "  in: "+strings.Join(rooms, ", "))
	} else {
		lines = append(lines, "  in: no rooms — /list, then join one")
	}
	if s.isServerOp() {
		lines = append(lines, "  server operator")
	}
	if away {
		since := ""
		if awaySince > 0 {
			since = " since " + humanAgo(time.Duration(h.now()-awaySince)*time.Millisecond)
		}
		if awayReason != "" {
			lines = append(lines, fmt.Sprintf("  away%s: %s", since, awayReason))
		} else {
			lines = append(lines, "  away"+since)
		}
	}
	lines = append(lines, "  "+s.notifyStateLine()+" — /notify for detail")
	s.sendNoticeLines(roomPtr(room), lines)
}

// --- /seen ------------------------------------------------------------

// cmdSeen answers "did they get my message?" — and, more usefully,
// "is @thatname even them?".
//
// Every match is listed, not just an unambiguous one. Two people with
// the same nick is the reason a mention silently resolves to nobody
// (see resolveOneMentionLocked), and this is where somebody can
// discover that has happened to them.
func (s *Session) cmdSeen(parts []string, room string) {
	h := s.hub
	if len(parts) < 2 {
		s.sendNotice(roomPtr(room), "usage: /seen <nick|hashprefix>")
		return
	}
	tok := parts[1]

	type found struct {
		idHex     string
		nick      string
		connected bool
		away      bool
		lastHeard int64   // ms since last inbound frame, when connected
		lastSeen  float64 // unix seconds from the directory, else 0
	}
	var out []found

	h.mu.Lock()
	// Connected peers first: resolveTargetLocked over every session is
	// the same matcher /kick and a mention use, so what this reports is
	// what those would have named.
	live := map[string]bool{}
	for _, m := range h.resolveTargetLocked(tok, nil) {
		if m.hashHex == "" {
			continue
		}
		m.session.mu.Lock()
		away := m.session.away
		last := m.session.lastAliveMs
		m.session.mu.Unlock()
		age := int64(0)
		if last > 0 {
			age = h.now() - last
		}
		live[m.hashHex] = true
		out = append(out, found{idHex: m.hashHex, nick: m.nick, connected: true, away: away, lastHeard: age})
	}
	// Then the directory: people who have been here and are not here
	// now, who are exactly who this question is usually about.
	want := strings.ToLower(strings.TrimSpace(tok))
	prefix := ""
	if looksLikeHashPrefix(tok) {
		prefix = normHex(tok)
	}
	for id, p := range h.peers {
		if live[id] {
			continue
		}
		match := (prefix != "" && strings.HasPrefix(id, prefix)) ||
			(p.Nick != "" && strings.ToLower(p.Nick) == want)
		if !match {
			continue
		}
		out = append(out, found{idHex: id, nick: p.Nick, lastSeen: p.LastSeenTS})
	}
	h.mu.Unlock()

	if len(out) == 0 {
		s.sendNotice(roomPtr(room), "no one here or on record matches '"+snippet(tok, 32)+"'")
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].idHex < out[j].idHex })

	lines := make([]string, 0, len(out)+1)
	if len(out) > 1 {
		lines = append(lines, fmt.Sprintf("'%s' matches %d identities — a mention by that nick alone resolves to nobody:", snippet(tok, 32), len(out)))
	}
	for _, f := range out {
		who := f.idHex
		if f.nick != "" {
			who = fmt.Sprintf("%s (%s)", f.nick, f.idHex)
		}
		switch {
		case f.connected && f.away:
			lines = append(lines, fmt.Sprintf("  %s — connected but away", who))
		case f.connected:
			lines = append(lines, fmt.Sprintf("  %s — connected, last heard from %s",
				who, humanAgo(time.Duration(f.lastHeard)*time.Millisecond)))
		case f.lastSeen > 0:
			lines = append(lines, fmt.Sprintf("  %s — last here %s", who,
				humanAgo(time.Since(time.Unix(int64(f.lastSeen), 0)))))
		default:
			lines = append(lines, fmt.Sprintf("  %s — on record, never seen connected", who))
		}
	}
	s.sendNoticeLines(roomPtr(room), lines)
}

// --- /away, /back -----------------------------------------------------

// cmdAway lets a peer declare what the hub cannot measure.
//
// The hub's presence test (mentionLivenessProven) can only ask whether
// frames are still arriving, so a client left connected in a background
// tab is indistinguishable from someone reading the room. A mention in
// that state is written into the room and nowhere else, which is the
// exact complaint "notifications don't work" describes. /away resolves
// it by saying so out loud: the hub then queues the mention and pushes
// it the way it pushes one for somebody who has disconnected.
func (s *Session) cmdAway(parts []string, room string) {
	h := s.hub
	reason := strings.TrimSpace(strings.Join(parts[1:], " "))
	// Bounded and sanitised like a nick, but through normalizeText, not
	// normalizeNick: a reason is a sentence, and "back in 10 minutes"
	// must not come back as "back_in_10_minutes".
	if reason != "" {
		if r, ok := normalizeText(reason, h.limits.MaxNickBytes*4); ok {
			reason = r
		} else {
			s.sendNotice(roomPtr(room), "away reason rejected (too long, or not printable text)")
			return
		}
	}

	s.mu.Lock()
	s.away = true
	s.awayReason = reason
	s.awaySinceMs = h.now()
	s.mu.Unlock()

	msg := "you are marked away"
	if reason != "" {
		msg += " (" + reason + ")"
	}
	switch {
	case !h.cfg.MentionNotify:
		msg += "; this hub does not send mention notifications, so mentions will still only appear here"
	case s.notifyOptedOut():
		msg += "; mentions will be held for your return (/notify on to have them sent to you)"
	case h.mentionLXMFReady():
		msg += "; mentions will be sent to you rather than shown here. /back when you return"
	default:
		msg += "; mentions will be held for your return. /back when you return"
	}
	s.sendNotice(roomPtr(room), msg)
}

func (s *Session) cmdBack(_ []string, room string) {
	s.mu.Lock()
	was := s.away
	s.away = false
	s.awayReason = ""
	s.awaySinceMs = 0
	s.mu.Unlock()
	if !was {
		s.sendNotice(roomPtr(room), "you were not marked away")
		return
	}
	s.sendNotice(roomPtr(room), "welcome back — mentions will appear here again")
	// Anything that arrived while away and has not been pushed is
	// handed over now, exactly as it would be on a fresh connection.
	s.flushMentions()
}

// --- /mentions --------------------------------------------------------

// cmdMentions shows the queue the hub is holding for this peer.
//
// A pending mention is invisible until the hub decides to hand it over,
// and the decision depends on push throttles and route availability
// that nobody outside the log can see. This makes the queue itself
// readable: "it is still waiting" is a completely different answer from
// "it was never noticed", and only one of them is a bug.
func (s *Session) cmdMentions(parts []string, room string) {
	h := s.hub
	if !h.cfg.MentionNotify {
		s.sendNotice(roomPtr(room), "this hub does not track mentions")
		return
	}
	idHex := s.identityHex()
	if idHex == "" {
		s.sendError(roomPtr(room), "identify first")
		return
	}

	if len(parts) >= 2 && strings.EqualFold(parts[1], "clear") {
		h.mu.Lock()
		n := 0
		if p, ok := h.peers[idHex]; ok {
			n = len(p.Mentions)
			p.Mentions = nil
			h.peersDirty = true
		}
		h.mu.Unlock()
		s.sendNotice(roomPtr(room), fmt.Sprintf("cleared %d pending mention(s)", n))
		return
	}

	h.mu.Lock()
	var pending []peerreg.Mention
	if p, ok := h.peers[idHex]; ok {
		pending = append(pending, p.Mentions...)
	}
	lastPush, pushed := h.lastPush[idHex]
	h.mu.Unlock()

	if len(pending) == 0 {
		s.sendNotice(roomPtr(room), "nothing waiting for you")
		return
	}
	lines := []string{fmt.Sprintf("%d mention(s) waiting for you:", len(pending))}
	for _, m := range pending {
		lines = append(lines, fmt.Sprintf("  %s in %s by %s: %s",
			humanAgo(time.Since(time.Unix(int64(m.TS), 0))),
			m.Room, mentionAuthor(m.ByNick, m.ByHex), m.Text))
	}
	if pushed {
		next := mentionPushInterval - time.Since(lastPush)
		if next > 0 {
			lines = append(lines, fmt.Sprintf(
				"the hub last tried to send these %s; next attempt in about %s (/notify test to try now)",
				humanAgo(time.Since(lastPush)), humanUptime(next)))
		}
	}
	lines = append(lines, "/mentions clear discards them")
	s.sendNoticeLines(roomPtr(room), lines)
}

// notifyOptedOut reports this session's mention-notification preference.
func (s *Session) notifyOptedOut() bool {
	h := s.hub
	idHex := s.identityHex()
	if idHex == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.peers[idHex]; ok {
		return p.NotifyOptOut
	}
	return false
}

// notifyStateLine renders the one-line notification state used by
// /whoami and /notify.
func (s *Session) notifyStateLine() string {
	switch {
	case !s.hub.cfg.MentionNotify:
		return "mention notifications: not offered by this hub"
	case s.notifyOptedOut():
		return "mention notifications: off"
	default:
		return "mention notifications: on"
	}
}
