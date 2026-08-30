package hub

import (
	"fmt"
	"strings"
)

// Unique nicknames.
//
// RRC nicknames are advisory, self-asserted and not unique — K_NICK is a
// display hint and nothing more. That is fine until something has to act
// on a name, and then it is the reason mentions fail: resolveMentions
// declines rather than guess when two people answer to "sam", so
// "@sam are you there" reaches neither of them and says nothing about
// having failed. Two people called sam is not an edge case on a hub
// anybody actually uses.
//
// So the hub grants a nick rather than accepting one. The first claim
// wins and later claimants get a numeric suffix — sam, sam1, sam2 —
// which is what IRC clients have done for thirty years and what people
// therefore already expect.
//
// Three things make this work rather than merely sound reasonable:
//
//  1. **A nick is owned by an IDENTITY, not by a session.** Ownership
//     survives disconnection, so somebody who has been "sam" here for a
//     year does not come back to find a stranger holding it. It is the
//     identity hash that owns it, which is the only thing about a peer
//     the hub has actually verified.
//
//  2. **Both entry points are enforced.** A nick arrives in HELLO, and
//     ALSO on any MSG carrying K_NICK, which handleMsg adopts as the
//     session nick. Enforcing only at HELLO would be theatre: a client
//     would simply re-assert the taken name on its next message.
//
//  3. **The peer is told, in an ordinary NOTICE.** Nothing in RRC can
//     carry "you asked for sam and you are sam1" — WELCOME has no field
//     for it and inventing one would need every client rebuilt. A
//     NOTICE is a thing every deployed client already renders.

// maxNickSuffix bounds the search for a free variant.
//
// Past this the hub stops counting and appends the identity prefix,
// which cannot collide: the alternative is an unbounded scan that a
// peer could force by claiming names in a loop.
const maxNickSuffix = 99

// assignNickLocked grants a nick to an identity, appending a numeric
// suffix when somebody else already owns what was asked for.
//
// Returns "" when want is empty, which leaves the peer identified by
// hash alone, exactly as before. Caller must hold h.mu.
func (h *Hub) assignNickLocked(idHex, want string) string {
	if want == "" {
		return ""
	}
	if !h.cfg.UniqueNicks {
		return want
	}
	if h.nickOwnerLocked(want) == "" || h.nickOwnerLocked(want) == idHex {
		return want
	}
	for n := 1; n <= maxNickSuffix; n++ {
		cand := nickWithSuffix(want, fmt.Sprintf("%d", n), h.limits.MaxNickBytes)
		if owner := h.nickOwnerLocked(cand); owner == "" || owner == idHex {
			return cand
		}
	}
	// 99 people called sam. Fall back to something that cannot collide
	// rather than keep counting: the identity prefix is unique by
	// construction and still readable.
	if len(idHex) >= 6 {
		return nickWithSuffix(want, "-"+idHex[:6], h.limits.MaxNickBytes)
	}
	return want
}

// nickOwnerLocked returns the identity that holds a nick, or "" when it
// is free. Caller must hold h.mu.
//
// Both the connected sessions and the peer directory are consulted. The
// directory is what makes ownership outlive a disconnection — and it is
// also what a mention resolves against when the person is away, which
// is the case this whole feature exists for. On a hub with
// mention_notify off there is no directory, and uniqueness is then
// among connected sessions only; that is a real reduction, and it is
// the operator's own choice of configuration.
func (h *Hub) nickOwnerLocked(nick string) string {
	want := strings.ToLower(nick)
	for s := range h.sessions {
		s.mu.Lock()
		got := strings.ToLower(s.nick)
		s.mu.Unlock()
		if got != want {
			continue
		}
		if id := s.identityHex(); id != "" {
			return id
		}
	}
	for id, p := range h.peers {
		if p.Nick != "" && strings.ToLower(p.Nick) == want {
			return id
		}
	}
	return ""
}

// nickWithSuffix appends suffix, trimming the base on a rune boundary so
// the result still fits max_nick_bytes.
//
// Trimming the base rather than dropping the suffix is deliberate: the
// suffix is the part that makes the name correct, and a name that is
// unique but shortened beats one that is intact and wrong.
func nickWithSuffix(base, suffix string, maxBytes int) string {
	if maxBytes <= 0 || len(base)+len(suffix) <= maxBytes {
		return base + suffix
	}
	room := maxBytes - len(suffix)
	if room <= 0 {
		// Pathological max_nick_bytes. The suffix alone is still a
		// unique name, which is the property that matters.
		return suffix
	}
	cut := base[:room]
	for len(cut) > 0 && !isRuneStart(base, len(cut)) {
		cut = cut[:len(cut)-1]
	}
	return cut + suffix
}

// setNickLocked assigns a session's nick and reports the granted value
// plus whether it differs from what was asked. Caller must hold h.mu,
// and must not hold s.mu.
//
// The peer directory is updated in step. That is not bookkeeping: the
// directory is where ownership lives once the session is gone, and it
// is what a mention resolves against for somebody who is away. Left
// behind, it holds the name the peer ASKED for at HELLO rather than the
// one the hub granted and broadcast — so "sam2" would look unclaimed
// the moment its owner disconnected, and "@sam2" would resolve to
// nobody. Observed live before this line existed.
func (s *Session) setNickLocked(want string) (granted string, renamed bool) {
	h := s.hub
	idHex := s.identityHex()
	granted = h.assignNickLocked(idHex, want)
	s.mu.Lock()
	s.nick = granted
	s.mu.Unlock()
	// Only an existing entry is touched: filing a new one is
	// rememberPeer's job, and it has checks this does not.
	if granted != "" && idHex != "" {
		if p, ok := h.peers[idHex]; ok && p.Nick != granted {
			p.Nick = granted
			h.peersDirty = true
		}
	}
	return granted, want != "" && granted != want
}

// nickTakenNotice is what a renamed peer is told.
func nickTakenNotice(want, granted string) string {
	return fmt.Sprintf("the nick %q is already taken here — you are %q. "+
		"Names are unique on this hub so that @mentions reach one person; "+
		"reconnect with a different one if you would rather choose.", want, granted)
}
