package hub

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thatSFguy/reticulum-relay-chat/internal/rrc"
)

// maxNoticeChunkChars is the line-based NOTICE chunk size (rrcd's
// MAX_NOTICE_CHUNK_CHARS).
const maxNoticeChunkChars = 512

// roomLinksLocked collects the links of every member of a room. The
// caller must hold the hub mutex.
func roomLinksLocked(r *Room) []Link {
	out := make([]Link, 0, len(r.members))
	for s := range r.members {
		out = append(out, s.link)
	}
	return out
}

// roomLinksExceptLocked collects member links excluding one session.
func roomLinksExceptLocked(r *Room, except *Session) []Link {
	out := make([]Link, 0, len(r.members))
	for s := range r.members {
		if s == except {
			continue
		}
		out = append(out, s.link)
	}
	return out
}

// shortHash renders the first 4 bytes of an identity hash for logs.
func shortHash(h []byte) string {
	if len(h) == 0 {
		return "(unidentified)"
	}
	n := 4
	if len(h) < n {
		n = len(h)
	}
	return hex.EncodeToString(h[:n])
}

// shortHex12 renders the first 12 hex chars of an identity hash.
func shortHex12(h []byte) string {
	s := hex.EncodeToString(h)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// shortHexStr renders the first 8 chars of an already-hex identity, for
// logs that hold the hex form rather than the bytes.
func shortHexStr(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// normHex lowercases a hex hash and strips an optional "0x" prefix and
// surrounding whitespace.
func normHex(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "0x")
	return s
}

// identityHashLen is the byte length of a full RNS identity hash
// (SHA-256 truncated to 16 bytes — rns.IdentityHashLen).
const identityHashLen = 16

// parseHexHash parses a token as a full identity hash for use as a STORED
// ban/kline/invite key: optional "0x" prefix, whitespace tolerated, must
// be valid lowercase hex of exactly identityHashLen bytes. A short prefix
// is rejected (A14): ban sets are keyed by the full 16-byte hash, so a
// stored short entry would silently never match isBanned — a dead ban.
// Live-session prefix matching stays valid via resolveTargetLocked.
func parseHexHash(tok string) (string, error) {
	n := normHex(tok)
	b, err := hex.DecodeString(n)
	if err != nil {
		return "", fmt.Errorf("not valid hex")
	}
	if len(b) != identityHashLen {
		return "", fmt.Errorf("identity hash must be exactly %d bytes", identityHashLen)
	}
	return n, nil
}

// aclHasRoom reports whether a per-room ACL set (ops / voiced / bans)
// can take one more entry without exceeding the configured cap (audit
// A15). A cap <= 0 disables the limit; re-adding an already-present key
// is always allowed since it does not grow the set.
func aclHasRoom[V any](m map[string]V, key string, cap int) bool {
	if cap <= 0 {
		return true
	}
	if _, exists := m[key]; exists {
		return true
	}
	return len(m) < cap
}

// looksLikeHashPrefix reports whether a token should be treated as an
// identity-hash prefix rather than a nick: all-hex (after 0x strip) and
// at least 6 chars long.
func looksLikeHashPrefix(tok string) bool {
	n := normHex(tok)
	if len(n) < 6 {
		return false
	}
	for _, c := range n {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// normalizeText validates and trims self-asserted text — a nick, an
// /away reason. Returns ("", false) when it is unusable (control chars,
// invalid UTF-8, or too long) so the caller drops it.
//
// Interior spaces are KEPT: an away reason is a sentence.
func normalizeText(s string, maxBytes int) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if !utf8.ValidString(s) {
		return "", false
	}
	if strings.ContainsAny(s, "\n\r\x00") {
		return "", false
	}
	if maxBytes > 0 && len(s) > maxBytes {
		return "", false
	}
	return s, true
}

// normalizeNick is normalizeText plus the one rule that applies to a
// NAME and not to prose: interior whitespace becomes "_".
//
// A nick is not just a label, it is what @mentions address, and
// mentionTokens splits the body on whitespace because that is how
// people write. So "sam jones" is a name nobody can mention:
// "@sam jones" tokenizes to "sam", which names a different person
// or nobody at all — and a mention that resolves to nobody says nothing
// about having failed, to either end. The hub already GRANTS names
// rather than accepting them (see nicks.go), so it grants a mentionable
// one and says why.
func normalizeNick(nick string, maxBytes int) (string, bool) {
	n, ok := normalizeText(nick, maxBytes)
	if !ok {
		return "", false
	}
	if n = mentionableNick(n); n == "" {
		return "", false
	}
	return n, true
}

// mentionableNick rewrites a claimed name into one an @mention can
// actually address, or "" when nothing addressable is left.
//
// The rule is deliberately NOT a list of banned characters. It is a
// fixed point: whatever the hub grants, mentionTokens("@"+nick) must
// hand back that same nick. mentionTokens is the only authority on what
// a mention names, it lives next door in mentions.go, and it can change
// — so the invariant is checked here rather than assumed, and a name
// that still fails it is dropped instead of granted.
//
// Three ways a name failed it, all with the same silent ending — the
// sender believes they notified somebody, the recipient hears nothing,
// and the hub logs neither:
//
//	"sam jones"  ->  "@sam jones" names "sam"
//	"sam!"           ->  "@sam!" names "sam", punctuation trimmed off the end
//	"bob@host"       ->  "@bob@host" is read as an address and skipped whole
func mentionableNick(s string) string {
	// Invisible first: a zero-width space is not whitespace to
	// strings.Fields and not punctuation to the trimmer, so nothing
	// below would have removed it — and a name that differs from
	// another only invisibly is not a different name (lookalikes.go).
	s = stripInvisible(s)
	s = underscoreSpaces(s)
	// A second "@" makes the token an email address to mentionTokens,
	// which skips it entirely — the name is not merely mis-parsed, it
	// is never a mention at all.
	s = strings.ReplaceAll(s, "@", "_")
	// The ends the tokenizer trims. Interior punctuation is untouched,
	// so "O'Brien" and "sam.jones" survive intact — only the ends of a
	// token are stripped, and only there does a name have to give way.
	s = strings.TrimFunc(s, isTrimmedFromMention)
	if s == "" {
		return ""
	}
	if toks := mentionTokens("@" + s); len(toks) != 1 || toks[0] != s {
		return ""
	}
	return s
}

// underscoreSpaces replaces each run of whitespace with a single "_".
//
// Applied after the length check on purpose: it can only shrink the
// string (a run collapses to one byte, and a multi-byte space like
// U+00A0 becomes one), so a nick that fit still fits.
func underscoreSpaces(s string) string {
	if strings.IndexFunc(s, unicode.IsSpace) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte('_')
		}
		pendingSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// chunkText splits text into rune-safe NOTICE chunks of at most
// maxNoticeChunkChars characters, preferring line boundaries.
func chunkText(text string) []string { return chunkTextN(text, maxNoticeChunkChars) }

// chunkTextN is chunkText with an explicit chunk size, so a caller that
// must also respect a tightened max_msg_body_bytes can say so.
func chunkTextN(text string, n int) []string {
	if text == "" {
		return nil
	}
	if n <= 0 {
		n = maxNoticeChunkChars
	}
	var chunks []string
	for _, line := range strings.Split(text, "\n") {
		if utf8.RuneCountInString(line) <= n {
			chunks = append(chunks, line)
			continue
		}
		var cur strings.Builder
		count := 0
		for _, ch := range line {
			if count >= n {
				chunks = append(chunks, cur.String())
				cur.Reset()
				count = 0
			}
			cur.WriteRune(ch)
			count++
		}
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
		}
	}
	return chunks
}

// normalizeRoomName strips the display "#" clients put in front of room
// names, along with surrounding whitespace.
//
// RRC room names carry no leading "#" — the sigil is decoration a client
// adds when it renders one, exactly as the hub's own log format string
// does. But people type it, and some clients pass what was typed
// straight through, so a room created as "#lobby" is stored as "#lobby"
// and thereafter displayed as "##lobby" — a second room, distinct from
// the "lobby" everyone else is in, with a name nobody can type
// consistently.
//
// Every "#" is stripped rather than just one, so "##lobby" from a
// client that has already added its own also lands on "lobby".
//
// The name is also lower-cased, so "Lobby", "LOBBY" and "lobby" are one
// room. RRC is IRC-style and IRC channel names have been case-
// insensitive for forty years, which is what people expect — but the
// concrete reason is that this hub had the bug: an intro room
// configured as "WELCOME-INTRO" was invisible to anyone who typed
// "welcome-intro", because the hub cheerfully created a second, empty
// room of that name and showed them nothing. A room nobody can find by
// typing its name is not a room.
//
// This is normalization, not validation: it changes the name rather
// than rejecting it, because rejecting would break the clients that do
// this today and the person typing has unambiguously named "lobby"
// either way.
//
// Casing is therefore not display data. A room that wants a presentable
// name has a topic; the name is an identifier.
func normalizeRoomName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, "#")
	name = strings.TrimSpace(name)
	// Lower-case only what is already valid UTF-8. strings.ToLower
	// REPLACES malformed bytes with U+FFFD, which would turn a name
	// isValidRoomName must reject (audit A7 — it becomes a rooms.toml
	// table key, and an invalid one makes the registry unloadable) into
	// a name it accepts. Normalization must not launder its input.
	if !utf8.ValidString(name) {
		return name
	}
	return strings.ToLower(name)
}

// isValidRoomName reports whether a NORMALIZED room name is one the hub
// will accept. It is the single statement of that rule, shared by
// command arguments, JOIN, default_rooms and the intro room, because
// each of these ends up as a rooms.toml table key and an invalid one
// makes the registry unloadable on the next restart (audit A7).
func isValidRoomName(name string, maxBytes int) bool {
	if name == "" {
		return false
	}
	if !utf8.ValidString(name) {
		return false
	}
	if maxBytes > 0 && len(name) > maxBytes {
		return false
	}
	return true
}

// roomFromEnvelope reads an inbound envelope's K_ROOM, normalized.
//
// Every inbound room name goes through here — JOIN, PART, MSG and
// resource envelopes alike — because normalizing only where a room is
// created would give a client that joins "#lobby" a membership in
// "lobby" and then drop every message it sends to "#lobby" on the
// floor.
func roomFromEnvelope(e *rrc.Envelope) string {
	return normalizeRoomName(rrc.RoomName(e))
}

// targetMatch is one resolved peer for a /command target token.
type targetMatch struct {
	session *Session
	hashHex string
	nick    string
}

// resolveTarget resolves a command-argument token to connected peers. If
// the token looks like a hash prefix (>= 6 hex chars) it matches peers by
// hex-prefix; otherwise it matches by nick (case-insensitive). When room
// is non-nil, matches are filtered to that room. Caller must hold h.mu.
func (h *Hub) resolveTargetLocked(tok string, room *Room) []targetMatch {
	var pool []*Session
	if room != nil {
		for s := range room.members {
			pool = append(pool, s)
		}
	} else {
		for s := range h.sessions {
			pool = append(pool, s)
		}
	}

	var matches []targetMatch
	if looksLikeHashPrefix(tok) {
		prefix := normHex(tok)
		for _, s := range pool {
			id := s.identity()
			if id == nil {
				continue
			}
			hh := hex.EncodeToString(id)
			if strings.HasPrefix(hh, prefix) {
				s.mu.Lock()
				nick := s.nick
				s.mu.Unlock()
				matches = append(matches, targetMatch{s, hh, nick})
			}
		}
		return matches
	}
	want := strings.ToLower(strings.TrimSpace(tok))
	for _, s := range pool {
		s.mu.Lock()
		nick := s.nick
		s.mu.Unlock()
		if nick != "" && strings.ToLower(nick) == want {
			hh := ""
			if id := s.identity(); id != nil {
				hh = hex.EncodeToString(id)
			}
			matches = append(matches, targetMatch{s, hh, nick})
		}
	}
	return matches
}

// ambiguityNotice renders the multi-match disambiguation text.
func ambiguityNotice(matches []targetMatch) string {
	var b strings.Builder
	b.WriteString("ambiguous target — multiple matches:")
	for _, m := range matches {
		b.WriteString(fmt.Sprintf("\n  - %s nick='%s'", m.hashHex, m.nick))
	}
	b.WriteString("\nUse full or longer identity hash to disambiguate.")
	return b.String()
}

// renderMember renders a member for /who output.
func renderMember(s *Session) string {
	id := s.identity()
	s.mu.Lock()
	nick := s.nick
	away := s.away
	s.mu.Unlock()
	suffix := ""
	if away {
		suffix = " [away]"
	}
	if id == nil {
		return "(unidentified)" + suffix
	}
	full := hex.EncodeToString(id)
	if nick != "" {
		short := full
		if len(short) > 12 {
			short = short[:12]
		}
		return fmt.Sprintf("%s (%s)%s", nick, short, suffix)
	}
	return full + suffix
}
