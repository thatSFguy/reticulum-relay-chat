package hub

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/thatSFguy/reticulum-relay-chat/internal/peerreg"
	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// Mentions: noticing that a message named someone, and making sure they
// hear about it.
//
// The hard part is not delivery, it is resolution. RRC nicknames are
// advisory, self-asserted, session-scoped and not unique — K_NICK is a
// display hint, nothing more — so "@alice" does not identify anybody on
// its own. The hub resolves conservatively and silently declines when
// it cannot be sure: a mention that resolves to the wrong person is
// worse than one that resolves to nobody, because it sends someone
// else's conversation to a stranger.
//
// Resolution order, first unambiguous match wins:
//
//  1. a hash prefix (@ followed by 6+ hex characters — the same rule
//     /kick and friends use), which is exact;
//  2. a nickname among the room's current members;
//  3. a nickname in the peer directory — people who have used this hub
//     before but are not here now, who are precisely the people a
//     notification is for.
//
// Ambiguity at any step ends the attempt for that token.

// maxMentionsPerMessage bounds how many people one message can notify.
// Without it, a single message listing a hundred nicknames becomes a
// hundred notifications — a spam amplifier with the hub's return
// address on it.
const maxMentionsPerMessage = 5

// mentionTarget is one resolved recipient of a mention.
type mentionTarget struct {
	idHex   string
	session *Session // nil when the target is not currently connected
}

// mentionTokens extracts the @-prefixed tokens from a message body.
//
// Trailing punctuation is stripped so "@alice," and "@alice." name
// alice, which is how people actually write. An email-looking token is
// skipped: "mail me at bob@example.com" mentions nobody.
func mentionTokens(body string) []string {
	var out []string
	for _, field := range strings.Fields(body) {
		if !strings.HasPrefix(field, "@") || len(field) == 1 {
			continue
		}
		// A bare "@" inside a longer token (an address, a handle already
		// containing one) is not a mention of anything.
		if strings.Count(field, "@") > 1 {
			continue
		}
		tok := strings.TrimFunc(field[1:], func(r rune) bool {
			return unicode.IsPunct(r) && r != '-' && r != '_'
		})
		if tok == "" {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// resolveMentions maps a message body to the identities it named,
// excluding the sender: naming yourself is not a notification.
//
// Caller must hold h.mu.
func (h *Hub) resolveMentionsLocked(body, room, senderHex string) []mentionTarget {
	var targets []mentionTarget
	seen := map[string]bool{senderHex: true}

	r := h.roomLocked(room)
	for _, tok := range mentionTokens(body) {
		if len(targets) >= maxMentionsPerMessage {
			break
		}
		idHex, sess, ok := h.resolveOneMentionLocked(tok, r)
		if !ok || seen[idHex] {
			continue
		}
		seen[idHex] = true
		targets = append(targets, mentionTarget{idHex: idHex, session: sess})
	}
	return targets
}

// resolveOneMentionLocked resolves a single token. Caller must hold h.mu.
func (h *Hub) resolveOneMentionLocked(tok string, r *Room) (idHex string, sess *Session, ok bool) {
	// 1. Hash prefix — exact, and the escape hatch when nicknames are
	// ambiguous or absent.
	if looksLikeHashPrefix(tok) {
		prefix := normHex(tok)
		var matches []string
		for id := range h.peers {
			if strings.HasPrefix(id, prefix) {
				matches = append(matches, id)
			}
		}
		// A connected peer that has not yet been filed in the directory
		// is still a valid target.
		for _, s := range h.welcomedSessionsLocked() {
			id := s.identityHex()
			if id != "" && strings.HasPrefix(id, prefix) && !containsString(matches, id) {
				matches = append(matches, id)
			}
		}
		if len(matches) > 1 {
			return "", nil, false // ambiguous: decline
		}
		if len(matches) == 1 {
			return matches[0], h.sessionForHashLocked(matches[0]), true
		}
		// Hex-shaped but naming nobody. Nicknames are arbitrary UTF-8,
		// so plenty of ordinary ones are also valid hex — "decade",
		// "facade", "beaded". Returning here would make those people
		// permanently unmentionable, and silently, since a mention that
		// resolves to nobody says nothing. Fall through and try the
		// token as the nickname it probably is; an actual prefix match
		// has already won above, so this cannot override one.
	}

	// 2. A nickname in the room. resolveTargetLocked is the same
	// matcher /kick and friends use, so a mention names whoever a
	// command would have named.
	if r != nil {
		if matches := h.resolveTargetLocked(tok, r); len(matches) == 1 && matches[0].hashHex != "" {
			return matches[0].hashHex, matches[0].session, true
		} else if len(matches) > 1 {
			return "", nil, false // ambiguous in-room: decline
		}
	}

	// 3. A nickname in the directory — someone who has been here before
	// and is not here now. This is the case the whole feature exists
	// for.
	want := strings.ToLower(strings.TrimSpace(tok))
	var matches []string
	for id, p := range h.peers {
		if p.Nick != "" && strings.ToLower(p.Nick) == want {
			matches = append(matches, id)
		}
	}
	if len(matches) != 1 {
		return "", nil, false
	}
	return matches[0], h.sessionForHashLocked(matches[0]), true
}

// noteMentions reacts to one relayed room message.
//
// A target who is in the room already received the message through
// fan-out and needs nothing. A target who is connected but elsewhere is
// told immediately. A target who is gone has the mention held for their
// return.
func (h *Hub) noteMentions(room string, env *rrc.Envelope) {
	if !h.cfg.MentionNotify || room == "" {
		return
	}
	if env.Type != rrc.TMsg && env.Type != rrc.TAction {
		return
	}
	body, ok := env.Body.(string)
	if !ok || body == "" {
		return
	}
	senderHex := ""
	if env.Src != nil {
		senderHex = hex.EncodeToString(env.Src)
	}
	byNick := ""
	if env.Nick != nil {
		byNick = *env.Nick
	}

	h.mu.Lock()
	targets := h.resolveMentionsLocked(body, room, senderHex)
	r := h.roomLocked(room)
	type pending struct {
		sess *Session
		text string
	}
	var live []pending
	var queued int
	for _, t := range targets {
		if p, ok := h.peers[t.idHex]; ok && p.NotifyOptOut {
			continue
		}
		if t.session != nil {
			// Already in the room: fan-out delivered the message itself,
			// and a second "you were mentioned" for a line they are
			// looking at is noise.
			if r != nil && r.hasMember(t.session) {
				continue
			}
			live = append(live, pending{
				sess: t.session,
				text: fmt.Sprintf("you were mentioned in %s by %s: %s",
					room, mentionAuthor(byNick, senderHex), snippet(body, h.mentionSnippetBytes())),
			})
			continue
		}
		h.queueMentionLocked(t.idHex, peerreg.Mention{
			Room:   room,
			ByNick: byNick,
			ByHex:  senderHex,
			Text:   snippet(body, h.mentionSnippetBytes()),
			TS:     h.nowUnix(),
		})
		queued++
	}
	h.mu.Unlock()

	for _, p := range live {
		p.sess.sendNotice(&room, p.text)
	}
	if queued > 0 {
		h.log.Printf("mentions: held %d notification(s) from #%s for absent peers", queued, room)
	}
}

// queueMentionLocked files a mention for a peer who is not connected.
// Caller must hold h.mu.
//
// Only peers already in the directory can be queued for: the hub needs
// a public key to reach someone who is away, and an identity it has
// never seen identify has no key and therefore no address.
func (h *Hub) queueMentionLocked(idHex string, m peerreg.Mention) {
	p, ok := h.peers[idHex]
	if !ok {
		return
	}
	p.NextSeq++
	m.Seq = p.NextSeq
	p.Mentions = append(p.Mentions, m)
	// Oldest-first eviction: a burst while someone is away must not grow
	// the registry without bound, and the newest mentions are the ones
	// still worth acting on.
	if max := h.mentionQueueLimit(); len(p.Mentions) > max {
		p.Mentions = p.Mentions[len(p.Mentions)-max:]
	}
	h.peersDirty = true
}

// flushMentions delivers and clears whatever was held for a session's
// peer. Called once the session is welcomed, so the notices arrive after
// WELCOME and the greeting rather than racing them.
func (s *Session) flushMentions() {
	h := s.hub
	idHex := s.identityHex()
	if idHex == "" || !h.cfg.MentionNotify {
		return
	}

	h.mu.Lock()
	p, ok := h.peers[idHex]
	if !ok || len(p.Mentions) == 0 {
		h.mu.Unlock()
		return
	}
	held := p.Mentions
	p.Mentions = nil
	h.peersDirty = true
	h.mu.Unlock()

	s.sendNotice(nil, fmt.Sprintf("--- %d mention(s) while you were away ---", len(held)))
	for _, m := range held {
		room := m.Room
		s.sendNotice(&room, fmt.Sprintf("%s in %s by %s: %s",
			humanAgo(time.Since(time.Unix(int64(m.TS), 0))),
			m.Room, mentionAuthor(m.ByNick, m.ByHex), m.Text))
	}
}

// rememberPeer files an identified session in the peer directory, so a
// later mention of that nickname can be resolved and addressed.
//
// The public key is what makes the entry worth keeping: without it the
// hub knows who someone is but has no way to reach them.
func (s *Session) rememberPeer() {
	h := s.hub
	if !h.cfg.MentionNotify {
		// The directory exists to address people for notifications. With
		// those off, recording who has been here is state the hub has no
		// use for and no reason to keep.
		return
	}
	idHex := s.identityHex()
	key := s.PeerPublicKey()
	if idHex == "" || len(key) != peerreg.PublicKeyLen {
		return
	}
	// The key must derive the identity the link verified. It always
	// does on the live path — both come from the same LINKIDENTIFY —
	// but this is the one place a bad pairing would enter the directory
	// and become an address, so it is checked here too.
	if !peerreg.KeyMatchesIdentity(key, idHex) {
		h.log.Printf("peers: refusing %s — public key does not derive it", shortHash(s.identity()))
		return
	}

	s.mu.Lock()
	nick := s.nick
	s.mu.Unlock()

	h.mu.Lock()
	p, ok := h.peers[idHex]
	if !ok {
		if lim := h.cfg.MaxKnownPeers; lim > 0 && len(h.peers) >= lim {
			h.evictOldestPeerLocked()
		}
		p = &peerreg.Peer{IdentityHex: idHex}
		h.peers[idHex] = p
	}
	p.PublicKey = append([]byte(nil), key...)
	if nick != "" {
		p.Nick = nick
	}
	p.LastSeenTS = h.nowUnix()
	h.peersDirty = true
	h.mu.Unlock()
}

// evictOldestPeerLocked drops the least recently seen peer to make room
// for a new one. Caller must hold h.mu.
func (h *Hub) evictOldestPeerLocked() {
	oldest := ""
	var oldestTS float64
	for _, id := range peerreg.SortedIdentities(h.peers) {
		p := h.peers[id]
		if oldest == "" || p.LastSeenTS < oldestTS {
			oldest, oldestTS = id, p.LastSeenTS
		}
	}
	if oldest != "" {
		delete(h.peers, oldest)
	}
}

// mentionAuthor renders who did the mentioning: the nickname when there
// is one, otherwise a short hash, which is at least true.
func mentionAuthor(nick, idHex string) string {
	if nick != "" {
		return nick
	}
	if len(idHex) >= 8 {
		return idHex[:8]
	}
	if idHex == "" {
		return "someone"
	}
	return idHex
}

// snippet truncates a body to n bytes on a rune boundary.
func snippet(body string, n int) string {
	body = strings.TrimSpace(body)
	if n <= 0 || len(body) <= n {
		return body
	}
	cut := body[:n]
	for len(cut) > 0 && !isRuneStart(body, len(cut)) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}

// isRuneStart reports whether i indexes a rune boundary in s.
func isRuneStart(s string, i int) bool {
	return i >= len(s) || s[i]&0xC0 != 0x80
}

// humanAgo renders how long ago something happened.
func humanAgo(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return "just now"
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
