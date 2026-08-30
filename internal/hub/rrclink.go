package hub

import (
	"fmt"
	"strings"
)

// Links to a room.
//
// An offline notification lands in a general messaging app with no way
// back: it says a room's name, and the reader has to already know which
// hub that room is on and how to reach it. RRC defines no way to write
// that down.
//
// Rather than invent one, this follows the syntax NomadNet already uses
// for exactly this problem (SPEC §11.6.3), which the ecosystem's clients
// and users already read:
//
//	lxmf@<32hex>              an LXMF conversation      (exists)
//	nnn@<32hex>:/page/x.mu    a NomadNet page           (exists)
//	rrc@<32hex>:/room/lobby   a room on an RRC hub      (this)
//
// `rrc` is the aspect shorthand, expanding to `rrc.hub` exactly as `nnn`
// expands to `nomadnetwork.node` and `lxmf` to `lxmf.delivery`. The hash
// is the hub's 16-byte truncated DESTINATION hash — what a client dials.
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
	// rrcLinkRoomPrefix namespaces the path, leaving room for other
	// targets later (a user, an invite) without ambiguity — the same
	// reason NomadNet has /page/ and /file/ rather than a bare name.
	rrcLinkRoomPrefix = "/room/"
	// destHashHexLen is a 16-byte truncated destination hash in hex.
	destHashHexLen = 32
)

// RRCLink is a parsed room link.
type RRCLink struct {
	// DestHash is the hub's destination hash, 32 lowercase hex chars.
	DestHash string
	// Room is the decoded room name, or "" when the link names a hub
	// with no particular room.
	Room string
}

// String renders the link in its canonical form.
func (l RRCLink) String() string {
	s := rrcLinkShorthand + "@" + l.DestHash
	if l.Room != "" {
		s += ":" + rrcLinkRoomPrefix + encodeLinkSegment(l.Room)
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
	return RRCLink{DestHash: dest, Room: room}.String()
}

// hubLinkLocked renders a link to the hub itself. Caller must hold h.mu.
func (h *Hub) hubLinkLocked() string { return h.roomLinkLocked("") }

// encodeLinkSegment percent-encodes a room name for a link path.
//
// Room names are arbitrary UTF-8 — spaces, punctuation and non-Latin
// scripts are all legal — and a link is a whitespace-delimited token
// somebody pastes out of a message. Anything but the unreserved set is
// escaped, which is stricter than net/url's PathEscape: that leaves
// ":" and "@" alone, and both are structural here.
func encodeLinkSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreservedLinkByte(c) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// decodeLinkSegment reverses encodeLinkSegment.
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

// isUnreservedLinkByte reports the RFC 3986 unreserved set.
func isUnreservedLinkByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '_', c == '.', c == '~':
		return true
	}
	return false
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

// ParseRRCLink parses the forms SPEC §11.6.3 defines, with `rrc` added:
//
//	rrc@<32hex>[:/room/<name>]
//	rrc.hub@<32hex>[:/room/<name>]
//	<32hex>[:/room/<name>]        bare, as NomadNet's bare-hash form
//
// It is here as the reference implementation — the hub itself only
// writes links — so that the format has one definition with tests
// against it rather than a prose description each client re-reads.
//
// Parsing is strict. §11.6.3 warns that accepting forgiving variants of
// a hash creates aliases for one destination and invites cache
// poisoning, so the hash must be exactly 32 hex characters with no
// embedded separators, and is lowercased.
func ParseRRCLink(s string) (RRCLink, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return RRCLink{}, fmt.Errorf("empty link")
	}

	// Aspect shorthand, if present.
	if at := strings.Index(s, "@"); at >= 0 {
		aspect := strings.ToLower(s[:at])
		if aspect != rrcLinkShorthand && aspect != rrcLinkAspect {
			return RRCLink{}, fmt.Errorf("not an RRC link: aspect %q", aspect)
		}
		s = s[at+1:]
	}

	// Path, if present. SplitN so a "/" inside an (encoded) room name
	// cannot be mistaken for structure.
	rest := ""
	if colon := strings.Index(s, ":"); colon >= 0 {
		rest = s[colon+1:]
		s = s[:colon]
	}

	hash := strings.ToLower(s)
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
	if rest == "" {
		return link, nil
	}
	if !strings.HasPrefix(rest, rrcLinkRoomPrefix) {
		return RRCLink{}, fmt.Errorf("unknown link path %q", rest)
	}
	room, err := decodeLinkSegment(strings.TrimPrefix(rest, rrcLinkRoomPrefix))
	if err != nil {
		return RRCLink{}, fmt.Errorf("bad room in link: %w", err)
	}
	// The same normalization every other room name gets, so a link
	// carrying a stray sigil cannot address a room JOIN cannot.
	link.Room = normalizeRoomName(room)
	if link.Room == "" {
		return RRCLink{}, fmt.Errorf("link names an empty room")
	}
	return link, nil
}
