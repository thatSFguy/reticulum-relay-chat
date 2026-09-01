package hub

import (
	"fmt"
	"strings"
	"unicode"
)

// Links to a room.
//
// An offline notification lands in a general messaging app with no way
// back: it says a room's name, and the reader has to already know which
// hub that room is on and how to reach it. RRC defines no way to write
// that down.
//
// Rather than invent one, this uses the form NomadNet already reads
// (rrc-room-links.md v2):
//
//	rrc://<32hex>[:<dest_name>]/<room>
//
// NomadNet 1.2.8 ships a full RRC client and parses this in
// Browser.handle_rrc_link (Browser.py:426-461), reached from the rrc://
// scheme (:277-280) or from an rrc@ micron link whose shorthand
// expand_shorthands maps to rrc.hub.session (:206-214, :312-314). The
// hash is the hub's 16-byte truncated DESTINATION hash — what a client
// dials. dest_name overrides the hub's aspect and is omitted when it is
// the default rrc.hub.
//
// v1 of this emitted rrc@<32hex>:/room/<percent-encoded> on the
// reasoning that an rrc:// scheme would be an invention in an ecosystem
// that already had a convention. The reasoning was sound and the fact
// was wrong: NomadNet had shipped rrc:// five weeks earlier and had
// already claimed the rrc@ shorthand for a different payload grammar.
// Run a v1 link through it and the hub resolves while the room comes out
// as the literal name "room/<x>" — which this hub, accepting any
// non-empty UTF-8 name, then creates. A junk room, no error anywhere.
// ParseRRCLink still reads v1 links; nothing writes them.
//
// Nothing has to understand this. It is plain text in a message body, so
// a client that has never heard of it shows it as text and a person can
// copy it — which is strictly more than the nothing they have today. A
// client that does understand it can make it a tap.

// rrcLinkShorthand is the aspect shorthand, and rrcLinkAspect is what it
// expands to. The expansion is the hub's dest_name and must stay in step
// with config.DefaultDestName.
const (
	rrcLinkShorthand = "rrc"
	rrcLinkAspect    = "rrc.hub"
	// rrcLinkSessionAspect is what NomadNet's expand_shorthands maps
	// "rrc" to (Browser.py:212). Despite the shape it is not a
	// destination aspect — it is the internal label handle_link
	// switches on; the aspect a hub registers is rrcLinkAspect.
	rrcLinkSessionAspect = "rrc.hub.session"
	// rrcLinkScheme is the canonical URL form, matched case-sensitively
	// because Browser.py:278 matches it that way.
	rrcLinkScheme = "rrc://"
	// rrcLinkV1RoomPrefix is v1's path namespace, read but never
	// written. v2 has no path namespace: everything after the first "/"
	// is the room name, which is the cost of using upstream's grammar.
	rrcLinkV1RoomPrefix = "room/"
	// destHashHexLen is a 16-byte truncated destination hash in hex.
	destHashHexLen = 32
)

// RRCLink is a parsed room link.
type RRCLink struct {
	// DestHash is the hub's destination hash, 32 lowercase hex chars.
	DestHash string
	// Room is the room name, or "" when the link names a hub with no
	// particular room.
	Room string
	// DestName is the hub's aspect when the link spells one out, or ""
	// for the default rrc.hub. Carried rather than dropped: a link that
	// names another aspect names another destination, and silently
	// treating it as the default would dial the wrong hub.
	DestName string
}

// String renders the link in its canonical form.
//
// The room segment is LITERAL. NomadNet does no percent-decoding, so an
// encoded name would join a room whose name contains a literal "%20" —
// the same wrong-room failure v1 had, in a different costume. Callers
// must check linkSafeRoom first; roomLinkLocked does.
func (l RRCLink) String() string {
	s := rrcLinkScheme + l.DestHash
	if l.DestName != "" && l.DestName != rrcLinkAspect {
		s += ":" + l.DestName
	}
	if l.Room != "" {
		s += "/" + l.Room
	}
	return s
}

// roomLink renders a link to a room on this hub, or "" when the hub does
// not know its own destination hash.
//
// Returning "" rather than a partial link is deliberate: a link missing
// the hash is not a shorter link, it is a wrong one, and it would be
// copied around as if it worked. Caller must hold h.mu.
func (h *Hub) roomLinkLocked(room string) string {
	dest := h.destHashHexLocked()
	if len(dest) != destHashHexLen {
		return ""
	}
	if room != "" && !linkSafeRoom(room) {
		return ""
	}
	return RRCLink{DestHash: dest, Room: room}.String()
}

// linkSafeRoom reports whether a room name can be written into a link
// that survives being pasted into a message body.
//
// The room segment is literal and a link in running text ends at the
// first whitespace, so a name containing a space would be truncated on
// paste into a link naming a shorter, different room. No link is better
// than a wrong one — the caller omits the link entirely.
//
// This is the one thing v1 could express that v2 cannot. It is the price
// of the segment being literal, which is what makes every other name
// work in the client that reads these.
func linkSafeRoom(room string) bool {
	if room == "" {
		return false
	}
	for _, r := range room {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7F {
			return false
		}
	}
	return true
}

// hubLinkLocked renders a link to the hub itself. Caller must hold h.mu.
func (h *Hub) hubLinkLocked() string { return h.roomLinkLocked("") }

// decodeLinkSegment percent-decodes a v1 link's room segment.
//
// Read path only — v2 writes the room name literally. v1 escaped
// everything outside the unreserved set, which is how it could carry a
// name v2 refuses to write at all.
func decodeLinkSegment(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("truncated %% escape")
		}
		hi, ok1 := unhexByte(s[i+1])
		lo, ok2 := unhexByte(s[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("bad %% escape")
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func unhexByte(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ParseRRCLink parses every form the v2 grammar reads:
//
//	rrc://<32hex>[:<dest_name>]/<room>   canonical
//	rrc@<32hex>[…]                       shorthand, and the aspect
//	rrc.hub@… / rrc.hub.session@…        spellings NomadNet expands
//	<32hex>[…]                           bare, as the bare-hash form
//	rrc@<32hex>:/room/<pct-encoded>      v1, still in the wild
//
// It is here as the reference implementation — the hub itself only
// writes links — so the format has one definition with tests against it
// rather than a prose description each client re-reads.
//
// The payload parse transcribes Browser.handle_rrc_link
// (Browser.py:430-446):
//
//	rest = link_target.strip()
//	if rest.startswith("/"): rest = rest[1:]
//	hub_part, _, room = rest.partition("/")
//	hex_part, _, dest = hub_part.partition(":")
//
// with one narrowing: the hash must be exactly 32 hex characters. §11.6.3
// warns that accepting forgiving variants of a hash creates aliases for
// one destination and invites cache poisoning, and that argument holds
// regardless of upstream's looser bytes.fromhex check.
//
// A v1 link parses here as an EMPTY dest_name (its trailing colon) plus a
// room of "room/<segment>" — a shape v2 cannot otherwise produce, since a
// v2 link has either no colon at all or a non-empty dest_name. That is
// what makes the shim unambiguous.
func ParseRRCLink(s string) (RRCLink, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return RRCLink{}, fmt.Errorf("empty link")
	}

	// URL form, checked before the "@" split exactly where
	// Browser.py:277-280 checks it — which is what makes a room name
	// containing "@" work in this form and not in the shorthand one.
	if strings.HasPrefix(s, rrcLinkScheme) {
		s = s[len(rrcLinkScheme):]
	} else if at := strings.Index(s, "@"); at >= 0 {
		aspect := strings.ToLower(s[:at])
		if aspect != rrcLinkShorthand && aspect != rrcLinkAspect && aspect != rrcLinkSessionAspect {
			return RRCLink{}, fmt.Errorf("not an RRC link: aspect %q", aspect)
		}
		s = s[at+1:]
	}

	s = strings.TrimSpace(s)
	// Upstream tolerates one leading slash (rrc:///<hex>/room).
	s = strings.TrimPrefix(s, "/")

	hubPart, roomPart, _ := strings.Cut(s, "/")
	hexPart, destName, hadColon := strings.Cut(hubPart, ":")
	destName = strings.TrimSpace(destName)

	hash := strings.ToLower(hexPart)
	if len(hash) != destHashHexLen {
		return RRCLink{}, fmt.Errorf("destination hash must be %d hex characters, got %d",
			destHashHexLen, len(hash))
	}
	for i := 0; i < len(hash); i++ {
		if _, ok := unhexByte(hash[i]); !ok {
			return RRCLink{}, fmt.Errorf("destination hash is not hex")
		}
	}

	link := RRCLink{DestHash: hash}
	if hadColon && destName == "" && strings.HasPrefix(roomPart, rrcLinkV1RoomPrefix) {
		decoded, err := decodeLinkSegment(strings.TrimPrefix(roomPart, rrcLinkV1RoomPrefix))
		if err != nil {
			return RRCLink{}, fmt.Errorf("bad room in link: %w", err)
		}
		roomPart = decoded
	} else if destName != "" && !strings.EqualFold(destName, rrcLinkAspect) {
		link.DestName = destName
	}

	// The same normalization every other room name gets, so a link
	// carrying a stray sigil cannot address a room JOIN cannot.
	link.Room = normalizeRoomName(roomPart)
	return link, nil
}
